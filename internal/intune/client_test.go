package intune

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/microsoft/kiota-abstractions-go/authentication"
)

func sdkFixture(t *testing.T, handler http.HandlerFunc) (*client, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	auth, err := authentication.NewApiKeyAuthenticationProvider("Bearer secret-token", "Authorization", authentication.HEADER_KEYLOCATION)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newSDKClient(server.URL+"/v1.0", auth, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	c.appType = win32Type
	return c, server.URL
}

func TestSDKRetriesPreserveNativeBody(t *testing.T) {
	want := object{"isFeatured": false, "allowedArchitectures": nil, "assignments": []any{}, "installExperience": object{"futureField": "preserved"}}
	for _, method := range []abs.HttpMethod{abs.POST, abs.PATCH} {
		t.Run(method.String(), func(t *testing.T) {
			calls := 0
			c, _ := sdkFixture(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Header.Get("Authorization") != "Bearer secret-token" {
					t.Error("missing SDK authentication")
				}
				reader := io.Reader(r.Body)
				if r.Header.Get("Content-Encoding") == "gzip" {
					compressed, err := gzip.NewReader(reader)
					if err != nil {
						t.Error(err)
						return
					}
					defer func() { _ = compressed.Close() }()
					reader = compressed
				}
				var got object
				if err := json.NewDecoder(reader).Decode(&got); err != nil {
					t.Error(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("body changed on attempt %d: %#v", calls, got)
				}
				w.Header().Set("Content-Type", "application/json")
				if calls == 1 {
					w.Header().Set("Retry-After", "0")
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"code":"TooManyRequests"}}`)
					return
				}
				w.WriteHeader(204)
			})
			if err := c.request(t.Context(), method, c.apps(), want, nil); err != nil {
				t.Fatal(err)
			}
			if calls != 2 {
				t.Fatalf("requests = %d", calls)
			}
		})
	}
}

func TestSDKDoesNotRetryAmbiguousCreation(t *testing.T) {
	calls := 0
	c, _ := sdkFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"code":"Unavailable","message":"secret-token"}}`)
	})
	err := c.request(t.Context(), abs.POST, c.apps(), object{}, nil)
	if err == nil || strings.Contains(err.Error(), "secret-token") || calls != 1 {
		t.Fatalf("creation: calls=%d, error=%v", calls, err)
	}
}

func TestSDKPagingAndResponseBound(t *testing.T) {
	for _, scenario := range []string{"pages", "escaped page", "large response"} {
		t.Run(scenario, func(t *testing.T) {
			c, _ := sdkFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if scenario == "large response" {
					_, _ = io.WriteString(w, strings.Repeat(" ", 8<<20+1))
					return
				}
				if r.URL.Query().Get("$skiptoken") == "next" {
					_, _ = io.WriteString(w, `{"value":[{"id":"second","unknown":false}]}`)
					return
				}
				next := "https://" + r.Host + r.URL.Path + "?$skiptoken=next"
				if scenario == "escaped page" {
					next = "https://example.invalid/v1.0/apps"
				}
				_ = json.NewEncoder(w).Encode(object{"value": []object{{"id": "first"}}, "@odata.nextLink": next})
			})
			apps, err := c.list(t.Context(), c.apps())
			if scenario == "pages" {
				if err != nil || len(apps) != 2 || apps[1]["unknown"] != false {
					t.Fatalf("pages=%v, error=%v", apps, err)
				}
			} else if err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
}

func TestSDKRetryWaitCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c, _ := sdkFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		cancel()
	})
	if err := c.request(ctx, abs.GET, c.apps(), nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestAzureSDKMultipartRetry(t *testing.T) {
	payload := bytes.Repeat([]byte{0x17, 0x58, 0x92}, 2<<20)
	path := filepath.Join(t.TempDir(), "encrypted")
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	blocks := map[string][]byte{}
	retried := false
	var uploaded []byte
	c, base := sdkFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("Graph credential sent to blob storage")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if r.URL.Query().Get("comp") == "block" {
			id := r.URL.Query().Get("blockid")
			if prior, ok := blocks[id]; ok && !bytes.Equal(prior, data) {
				t.Error("retried block bytes changed")
			}
			blocks[id] = data
			if !retried {
				retried = true
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(503)
				return
			}
		} else {
			// Block ordering is validated by the complete SDK block list.
			var list struct {
				Latest []string `xml:"Latest"`
			}
			if err := xml.Unmarshal(data, &list); err != nil {
				t.Error(err)
				return
			}
			for _, id := range list.Latest {
				uploaded = append(uploaded, blocks[id]...)
			}
		}
		w.WriteHeader(201)
	})
	if err := c.uploadBlob(t.Context(), base+"/blob?sig=secret", &preparedArtifact{path: path, raw: true}); err != nil {
		t.Fatal(err)
	}
	if !retried || len(blocks) != 2 || !bytes.Equal(uploaded, payload) {
		t.Fatal("multipart upload lost or reordered bytes")
	}
}
