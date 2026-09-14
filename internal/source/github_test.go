package source

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

func TestGitHubAssetSelection(t *testing.T) {
	for _, test := range []struct{ name, pattern, release, want, failure string }{
		{"exact", "SafeExamBrowser-3.7.1.dmg", "3.7.1", "SafeExamBrowser-3.7.1.dmg", ""},
		{"SEB glob", "SafeExamBrowser-*.dmg", "latest", "SafeExamBrowser-3.7.1.dmg", ""},
		{"docs glob", "Application-*-arm64.zip", "", "Application-1.2-arm64.zip", ""},
		{"no match", "Foo-*.dmg", "latest", "", `has no asset matching "Foo-*.dmg"`},
		{"ambiguous", "Application-*.zip", "latest", "", `has 2 assets matching "Application-*.zip"; expected exactly one`},
		{"malformed", "App-[.zip", "latest", "", "invalid GitHub asset pattern"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := New(store, t.TempDir(), false)
			discoveries, downloads := 0, 0
			m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := "installer"
				if r.URL.Host == "api.github.com" {
					discoveries++
					endpoint := "/repos/example/app/releases/latest"
					if test.release != "" && test.release != "latest" {
						endpoint = "/repos/example/app/releases/tags/" + test.release
					}
					if r.URL.Path != endpoint {
						t.Fatalf("lookup %s, want %s", r.URL.Path, endpoint)
					}
					body = `{"id":12,"tag_name":"3.7.1","assets":[{"id":34,"name":"SafeExamBrowser-3.7.1.dmg","browser_download_url":"https://github.com/example/app/releases/download/3.7.1/SafeExamBrowser-3.7.1.dmg"},{"id":35,"name":"Application-1.2-arm64.zip","browser_download_url":"https://github.com/example/app/releases/download/3.7.1/Application-1.2-arm64.zip"},{"id":36,"name":"Application-1.2-x64.zip","browser_download_url":"https://github.com/example/app/releases/download/3.7.1/Application-1.2-x64.zip"}]}`
				} else {
					downloads++
					if r.URL.String() != "https://github.com/example/app/releases/download/3.7.1/"+test.want {
						t.Fatalf("unexpected download %s", r.URL)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})
			input := plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "asset": test.pattern}}
			if test.release != "" {
				input.Config["release"] = test.release
			}
			entry, err := m.Resolve(t.Context(), input)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("got %v, want %s", err, test.failure)
				}
				if downloads != 0 {
					t.Fatal("downloaded without a unique selection")
				}
				if test.name == "malformed" && discoveries != 0 {
					t.Fatal("invalid pattern reached discovery")
				}
				if test.name == "no match" && !strings.Contains(err.Error(), "SafeExamBrowser-3.7.1.dmg") {
					t.Fatal("missing available names")
				}
				if test.name == "ambiguous" && !strings.Contains(err.Error(), "Application-1.2-x64.zip") {
					t.Fatal("missing matched names")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			observed := observation(t, entry)
			wantID := int64(34)
			if test.name == "docs glob" {
				wantID = 35
			}
			if entry.Content.Filename != test.want || observed.Release != "3.7.1" || observed.ReleaseID != 12 || observed.AssetID != wantID {
				t.Fatalf("lost selected identity: %+v %+v", entry.Content, observed)
			}
			cachedPath, err := store.Path(entry.Content.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			// A later release must not be consulted, even after losing the cached bytes.
			if err := os.Remove(cachedPath); err != nil {
				t.Fatal(err)
			}
			if hit, err := m.FetchLocked(t.Context(), input, entry); err != nil || hit {
				t.Fatalf("cold replay: hit=%v err=%v", hit, err)
			}
			if discoveries != 1 || downloads != 2 {
				t.Fatalf("discovery=%d downloads=%d", discoveries, downloads)
			}
			for _, cold := range []bool{false, true} {
				t.Run(fmt.Sprintf("stale cold=%v", cold), func(t *testing.T) {
					if cold {
						if err := os.Remove(cachedPath); err != nil {
							t.Fatal(err)
						}
					}
					input.Config["asset"] = "*.dmg"
					if _, err := m.FetchLocked(t.Context(), input, entry); err == nil || !strings.Contains(err.Error(), "stale input lock") {
						t.Fatalf("changed pattern accepted: %v", err)
					}
				})
			}
		})
	}
}

func TestGitHubLockedFetchVerification(t *testing.T) {
	for _, test := range []struct{ name, address, body, failure string }{
		{"origin", "https://github.com/other/app/releases/download/v1/App.pkg", "installer", "configured GitHub repository"},
		{"content", "https://github.com/example/app/releases/download/v1/App.pkg", "changed installer", "source integrity mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := New(store, t.TempDir(), false)
			m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body := "installer"
				if r.URL.Host == "api.github.com" {
					body = `{"id":12,"tag_name":"v1","assets":[{"id":34,"name":"App.pkg","browser_download_url":"https://github.com/example/app/releases/download/v1/App.pkg"}]}`
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})
			input := plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "asset": "App*.pkg", "filename": "renamed.pkg"}}
			entry, err := m.Resolve(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if entry.Content.Filename != "renamed.pkg" {
				t.Fatal("lost filename override")
			}
			cachedPath, err := store.Path(entry.Content.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(cachedPath); err != nil {
				t.Fatal(err)
			}
			observed := observation(t, entry)
			observed.URL = test.address
			entry.Observation, err = json.Marshal(observed)
			if err != nil {
				t.Fatal(err)
			}
			m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if test.name == "origin" || r.URL.Host == "api.github.com" {
					t.Fatalf("unexpected request %s", r.URL)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body)), Header: http.Header{}}, nil
			})
			if _, err := m.FetchLocked(t.Context(), input, entry); err == nil || !strings.Contains(err.Error(), test.failure) {
				t.Fatalf("replay validation: %v", err)
			}
		})
	}
}
