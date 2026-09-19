package source

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

func TestGitHubReleaseRetainsRawTag(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, t.TempDir(), false)
	m.Client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "installer"
		if request.URL.Host == "api.github.com" {
			if request.URL.Path != "/repos/example/app/releases/tags/v1.2.3" {
				t.Errorf("wrong release selector: %s", request.URL.Path)
			}
			body = `{"id":12,"tag_name":"v1.2.3","assets":[{"id":34,"name":"App.pkg","browser_download_url":"https://github.com/example/app/releases/download/v1.2.3/App.pkg"}]}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	s := plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "release": "v1.2.3", "asset": "App.pkg"}}
	entry, err := m.Resolve(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if observation(t, entry).Release != "v1.2.3" || observation(t, entry).ReleaseID != 12 || observation(t, entry).AssetID != 34 {
		t.Fatalf("release identity changed: %+v", entry)
	}
	if _, err := m.FetchLocked(t.Context(), s, entry); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadPagePinsUniqueURLAndColdRecovery(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, t.TempDir(), false)
	pageReads := 0
	m.Client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := "installer"
		if request.URL.Host == "vendor.example" {
			pageReads++
			if request.Header.Get("Authorization") != "Bearer private" {
				t.Error("page credential missing")
			}
			body = `<a href="https://cdn.example/App.pkg?version=1&amp;arch=arm64">Download</a><a href="https://cdn.example/App.pkg?version=1&amp;arch=arm64">Duplicate</a>`
		} else if request.Header.Get("Authorization") != "" {
			t.Error("page credential leaked to artifact origin")
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	s := plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://vendor.example/download", "match": `https://cdn\.example/App\.pkg\?version=\d+&arch=arm64`, "token": "private"}}
	entry, err := m.Resolve(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if observation(t, entry).URL != "https://cdn.example/App.pkg?version=1&arch=arm64" || entry.Content.Filename != "App.pkg" {
		t.Fatalf("unexpected discovery: %+v", entry)
	}
	filename, err := store.Path(entry.Content.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if _, err := m.FetchLocked(t.Context(), s, entry); err != nil {
		t.Fatal(err)
	}
	if pageReads != 1 {
		t.Fatalf("frozen recovery fetched download page %d times", pageReads)
	}
}

func TestDownloadPageRejectsAmbiguousOrUnsafeMatches(t *testing.T) {
	for _, tc := range []struct{ name, body, pattern, want string }{
		{"missing", "no download", `https://cdn\.example/\S+`, "0 distinct"},
		{"ambiguous", "https://cdn.example/one.pkg https://cdn.example/two.pkg", `https://cdn\.example/\S+`, "2 distinct"},
		{"scheme", "file:///App.pkg", `file:\S+`, "stable HTTP(S) URL"},
		{"fragment", "/App.pkg#fragment", `/App\.pkg#fragment`, "stable HTTP(S) URL"},
		{"empty", "App.pkg", `^`, "valid URL reference"},
		{"malformed", "/App%zz.pkg", `/App%zz\.pkg`, "valid URL reference"},
		{"signed", "https://cdn.example/App.pkg?token=secret", `https://cdn\.example/\S+`, "stable HTTP(S) URL"},
		{"signed relative", "/App.pkg?token=secret", `/App\.pkg\?\S+`, "stable HTTP(S) URL"},
		{"userinfo", "//user:secret@cdn.example/App.pkg", `//\S+`, "stable HTTP(S) URL"},
		{"downgrade", "http://cdn.example/App.pkg", `http://cdn\.example/\S+`, "HTTPS downgrade"},
		{"oversize", strings.Repeat("x", (4<<20)+1), `https://cdn\.example/\S+`, "exceeds 4 MiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := New(store, t.TempDir(), false)
			m.Client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
				if request.URL.Host != "vendor.example" {
					t.Fatal("unsafe artifact request issued")
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tc.body)), Header: http.Header{}}, nil
			})
			_, err = m.Resolve(t.Context(), plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://vendor.example/download", "match": tc.pattern, "filename": "App.pkg"}})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestRedirectFailureDoesNotExposeTemporaryCredentials(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, closed.URL+"/download?token=private-upload-token", http.StatusFound)
	}))
	defer server.Close()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := New(store, t.TempDir(), false)
	_, err = manager.Resolve(t.Context(), plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}})
	if err == nil {
		t.Fatal("expected failed redirected download")
	}
	if strings.Contains(err.Error(), "private-upload-token") || strings.Contains(err.Error(), "?token=") {
		t.Fatalf("temporary credential leaked: %v", err)
	}
}

func TestStableQueryRetainsOriginalURLAcrossRedirects(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fwlink" {
			if r.URL.Query().Get("linkid") != "853070" {
				t.Error("stable link identifier was lost")
			}
			http.Redirect(w, r, "/installer?sig=temporary-signature&expires=123", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("installer"))
	}))
	defer server.Close()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, t.TempDir(), false)
	s := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/fwlink?linkid=853070", "filename": "CompanyPortal.pkg"}}
	entry, err := m.Resolve(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if observation(t, entry).URL != s.Config["url"].(string) || strings.Contains(observation(t, entry).URL, "temporary-signature") {
		t.Fatalf("lock retained redirected URL: %q", observation(t, entry).URL)
	}
}

func observation(t *testing.T, entry Entry) nativeObservation {
	t.Helper()
	var value nativeObservation
	if err := json.Unmarshal(entry.Observation, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestDownloadReportsActualBytes(t *testing.T) {
	for _, known := range []bool{true, false} {
		t.Run(strconv.FormatBool(known), func(t *testing.T) {
			payload := strings.Repeat("package fixture", 1024)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if known {
					w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
				} else {
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(w, payload)
			}))
			defer server.Close()
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
			entry, err := New(store, t.TempDir(), false).Resolve(ctx, plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/fixture.pkg"}})
			if err != nil {
				t.Fatal(err)
			}
			if entry.Content.Artifact.Size != int64(len(payload)) {
				t.Fatalf("artifact size %d", entry.Content.Artifact.Size)
			}
			decoder := json.NewDecoder(&logs)
			found := false
			for decoder.More() {
				var record struct {
					Current int64 `json:"current"`
					Total   int64 `json:"total"`
					Final   bool  `json:"progress_final"`
				}
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				if record.Final {
					found = true
					if record.Current != int64(len(payload)) || (known && record.Total != record.Current) || (!known && record.Total != 0) {
						t.Fatalf("transfer: %+v", record)
					}
				}
			}
			if !found {
				t.Fatal("download did not report its transferred bytes")
			}
		})
	}
}
