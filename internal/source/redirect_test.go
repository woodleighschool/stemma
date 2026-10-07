package source

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestHomebrewCaskHTTPMirrorReplay(t *testing.T) {
	for _, body := range []string{"installer", "tampered"} {
		t.Run(body, func(t *testing.T) {
			m := manager(t)
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte("installer")))
			input := plugin.Input{Resolver: "homebrew", Config: map[string]any{"cask": "example"}}
			m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "formulae.brew.sh" {
					t.Fatal("discovery downloaded payload")
				}
				metadata := `{"token":"example","version":"1","url":"https://vendor.test/app.dmg","sha256":"` + digest + `","supported_platforms":["arm64_golden_gate"]}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(metadata)), Request: req}, nil
			})
			entry, err := m.Resolve(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			// Replay from the serialized lock using an empty cache and no discovery.
			data, err := json.Marshal(entry)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(data, &entry); err != nil {
				t.Fatal(err)
			}
			cold := manager(t)
			requests := 0
			cold.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				switch req.URL.String() {
				case "https://vendor.test/app.dmg":
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"http://mirror.test/app.dmg"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				case "http://mirror.test/app.dmg":
					return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				default:
					t.Fatalf("unexpected request %s", req.URL)
					return nil, nil
				}
			})
			_, err = cold.FetchLocked(t.Context(), input, entry)
			if body == "tampered" {
				if err == nil || !strings.Contains(err.Error(), "integrity mismatch") {
					t.Fatalf("accepted tampering: %v", err)
				}
				badDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
				if cold.Store.HasDigest(badDigest) {
					t.Fatal("cached tampered payload")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := cold.Store.VerifyDigest(t.Context(), digest); err != nil {
					t.Fatal(err)
				}
			}
			if requests != 2 {
				t.Fatalf("requests = %d", requests)
			}
			// The relaxed acquisition must not change the shared client's policy.
			request, err := cold.request(t.Context(), "https://vendor.test/app.dmg", nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := cold.Client.Do(request)
			if response != nil {
				_ = response.Body.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "HTTPS downgrade blocked") || requests != 3 {
				t.Fatalf("shared policy changed: %v, requests=%d", err, requests)
			}
		})
	}
}

func TestHomebrewDowngradeRemainsBlocked(t *testing.T) {
	for _, kind := range []string{"hashless cask", "bottle", "metadata"} {
		t.Run(kind, func(t *testing.T) {
			m := manager(t)
			requests := 0
			m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"http://mirror.test/app.dmg"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})
			var err error
			if kind == "metadata" {
				_, err = m.Resolve(t.Context(), plugin.Input{Resolver: "homebrew", Config: map[string]any{"cask": "example"}})
			} else {
				config := homebrewConfig{Cask: "example"}
				observation := homebrewObservation{URL: "https://vendor.test/app.dmg", Filename: "app.dmg"}
				if kind == "bottle" {
					config = homebrewConfig{Formula: "example"}
					observation.Bottle = true
					observation.SHA256 = strings.Repeat("a", 64)
					observation.URL = "https://ghcr.io/v2/homebrew/core/example/blobs/sha256:" + observation.SHA256
				}
				data, marshalErr := json.Marshal(observation)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				acquisition, acquireErr := m.acquireHomebrew(t.Context(), config, data)
				if acquireErr != nil {
					t.Fatal(acquireErr)
				}
				_, _, err = m.download(t.Context(), *acquisition.Download, nil)
			}
			if err == nil || !strings.Contains(err.Error(), "HTTPS downgrade blocked") || requests != 1 {
				t.Fatalf("downgrade: %v, requests=%d", err, requests)
			}
		})
	}
}

func TestHTTPRedirectCredentials(t *testing.T) {
	for _, target := range []string{"http://mirror.test/app", "http://user:secret@mirror.test/app", "http://mirror.test/app?token=secret"} {
		t.Run(target, func(t *testing.T) {
			m := manager(t)
			requests := 0
			chain := []string{"https://vendor.test/app", target, "https://vendor.test/returned", "https://vendor.test/final"}
			m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if requests >= len(chain) || req.URL.String() != chain[requests] {
					t.Fatalf("unexpected request %s", req.URL)
				}
				for _, name := range []string{"Authorization", "Cookie", "Referer", "X-Api-Key"} {
					if requests == 0 && req.Header.Get(name) == "" {
						t.Errorf("missing initial %s", name)
					}
					if requests > 0 && req.Header.Get(name) != "" {
						t.Errorf("leaked %s at hop %d", name, requests)
					}
				}
				requests++
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("installer")), Request: req}
				if requests < len(chain) {
					response.StatusCode = http.StatusFound
					response.Header.Set("Location", chain[requests])
				}
				return response, nil
			})
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte("installer")))
			_, _, err := m.download(t.Context(), Download{URL: chain[0], SHA256: digest, AllowHTTPRedirect: true, Headers: map[string]string{"Authorization": "Bearer secret", "Cookie": "session=secret", "Referer": "https://vendor.test/?token=secret", "X-Api-Key": "secret"}}, nil)
			if target == "http://mirror.test/app" {
				if err != nil || requests != len(chain) {
					t.Fatalf("download: %v, requests=%d", err, requests)
				}
			} else if err == nil || requests != 1 || strings.Contains(err.Error(), "secret") {
				t.Fatalf("credential redirect: %v, requests=%d", err, requests)
			}
		})
	}
}

func TestHTTPRedirectRequiresHostVerifiedDigest(t *testing.T) {
	for _, digest := range []string{"", "invalid"} {
		m := manager(t)
		m.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { t.Fatal("sent unpinned request"); return nil, nil })
		if _, _, err := m.download(t.Context(), Download{URL: "https://vendor.test/app", SHA256: digest, AllowHTTPRedirect: true}, nil); err == nil {
			t.Fatal("accepted invalid pin")
		}
	}
}

func TestDownloadValidatorsCannotOverrideDigest(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNotModified} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			m := manager(t)
			previous := &record{Content: Content{SHA256: strings.Repeat("a", 64), Filename: "app.dmg"}, ETag: `"same"`}
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte("new installer")))
			m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("If-None-Match") != "" {
					t.Error("sent validator for different content")
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Etag": {previous.ETag}}, Body: io.NopCloser(strings.NewReader("new installer")), Request: req}, nil
			})
			result, reused, err := m.download(t.Context(), Download{URL: "https://vendor.test/app.dmg", SHA256: digest}, previous)
			if status == http.StatusOK {
				if err != nil || reused || result.Content.SHA256 != digest {
					t.Fatalf("download: %+v, reused=%t, err=%v", result, reused, err)
				}
			} else if err == nil || reused {
				t.Fatalf("accepted stale validator: reused=%t, err=%v", reused, err)
			}
		})
	}
}

func TestPinnedDownloadRedirectPolicy(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			m := manager(t)
			requests := 0
			m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"http://mirror.test/app"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})
			_, _, err := m.download(t.Context(), Download{URL: "https://vendor.test/app", SHA256: strings.Repeat("a", 64), AllowHTTPRedirect: allow}, nil)
			want, count := "HTTPS downgrade blocked", 1
			if allow {
				want, count = "too many redirects", 10
			}
			if err == nil || !strings.Contains(err.Error(), want) || requests != count {
				t.Fatalf("redirect: %v, requests=%d", err, requests)
			}
		})
	}
}
