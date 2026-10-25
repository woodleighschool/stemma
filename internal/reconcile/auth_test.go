package reconcile

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-git/go-git/v6/config"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/testutil/testgit"
)

//nolint:reassign // Intercept the production GitHub hosts without live requests or changing host matching.
func TestReleaseDiscoveryReusesAppCredentials(t *testing.T) {
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testKey(t))
	for _, tc := range []struct {
		name, remote, host, runToken string
		personal, appFallback        bool
	}{
		{name: "github app", remote: "https://github.com/example/catalog.git", host: "api.github.com", appFallback: true},
		{name: "ssh origin", remote: "git@github.com:example/catalog.git", host: "api.github.com", appFallback: true},
		{name: "enterprise app", remote: "https://ghe.example.com/example/catalog.git", host: "ghe.example.com"},
		{name: "lookalike host", remote: "https://github.com.example.com/example/catalog.git", host: "github.com.example.com"},
		{name: "personal source control token", remote: "https://github.com/example/catalog.git", host: "api.github.com", personal: true},
		{name: "environment override", remote: "https://github.com/example/catalog.git", host: "api.github.com", runToken: "environment-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			document := project(filepath.Join(t.TempDir(), "munki"))
			if tc.personal {
				document = strings.Replace(document, `client_id: "{{ env.GITHUB_APP_CLIENT_ID }}"
        installation_id: "{{ env.GITHUB_APP_INSTALLATION_ID }}"
        private_key: "{{ env.GITHUB_APP_PRIVATE_KEY }}"`, `token: personal-token`, 1)
			}
			upstream := strings.Replace(software("https://unused.example"), "url: https://unused.example/fixture.pkg", "resolver: github\n    repository: upstream/public-app\n    asset: fixture.pkg", 1)
			other := strings.ReplaceAll(strings.Replace(upstream, "name: fixture", "name: other", 1), "upstream/public-app", "upstream/other-app")
			for name, content := range map[string]string{"stemma.yaml": document, "fixture.software.yaml": upstream, "other.software.yaml": other} {
				if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			repo := testgit.Init(t, root)
			if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{tc.remote}}); err != nil {
				t.Fatal(err)
			}
			opts := Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: t.TempDir(), GitHubToken: tc.runToken}
			original := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = original })
			synctest.Test(t, func(t *testing.T) {
				var minted, lookups atomic.Int32
				http.DefaultTransport = releaseTransport(func(req *http.Request) (*http.Response, error) {
					var body any
					status := http.StatusOK
					if req.URL.Host == "api.github.com" && (req.URL.Path == "/repos/upstream/public-app/releases/latest" || req.URL.Path == "/repos/upstream/other-app/releases/latest") {
						lookups.Add(1)
						want := ""
						if tc.appFallback {
							want = fmt.Sprintf("Bearer installation-%d", minted.Load())
						} else if tc.runToken != "" {
							want = "Bearer " + tc.runToken
						}
						if got := req.Header.Get("Authorization"); got != want {
							t.Errorf("release Authorization = %q, want %q", got, want)
						}
						repository := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/repos/"), "/releases/latest")
						body = map[string]any{"id": 12, "tag_name": "v1", "assets": []any{map[string]any{
							"id": 34, "name": "fixture.pkg", "digest": "sha256:" + strings.Repeat("a", 64),
							"browser_download_url": "https://github.com/" + repository + "/releases/download/v1/fixture.pkg",
						}}}
					} else if req.URL.Host == tc.host {
						switch strings.TrimPrefix(req.URL.Path, "/api/v3") {
						case "/app":
							body = map[string]any{"slug": "stemma-fixture"}
						case "/app/installations/7/access_tokens":
							status = http.StatusCreated
							body = map[string]any{
								"token": fmt.Sprintf("installation-%d", minted.Add(1)), "expires_at": time.Now().Add(time.Hour),
								"repositories": []any{map[string]any{"id": 1, "full_name": "example/catalog"}},
							}
						case "/users/stemma-fixture[bot]", "/user":
							body = map[string]any{"id": 424242, "login": "stemma-fixture[bot]"}
						}
					}
					if body == nil {
						return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
					}
					data, err := json.Marshal(body)
					if err != nil {
						return nil, err
					}
					return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data))), Request: req}, nil
				})
				r, err := newRunner(t.Context(), opts)
				if err != nil {
					t.Fatal(err)
				}
				completed := 0
				for lookup := range 3 {
					if lookup == 2 {
						time.Sleep(time.Hour) // Advance the fake clock past token expiry.
					}
					options := r.engineOptions("update", opts.ConfigPath, nil)
					options.ResourceDone = func(engine.ResourceReport) error {
						completed++
						if completed == 1 {
							time.Sleep(time.Hour) // Expire the token within an engine run too.
						}
						return nil
					}
					candidate, err := engine.Resolve(t.Context(), options)
					for _, name := range []string{"fixture", "other"} {
						resource := candidate.Resources[key(name)]
						if err != nil || resource.Error != "" || resource.Inputs["source"].InputVersion != "v1" {
							t.Fatalf("release lookup: %v, %+v", err, resource)
						}
					}
				}
				wantMinted := int32(1)
				if tc.personal {
					wantMinted = 0
				} else if tc.appFallback {
					wantMinted = 3
				}
				if lookups.Load() != 6 || minted.Load() != wantMinted {
					t.Fatalf("lookups=%d, minted=%d; want 6 and %d", lookups.Load(), minted.Load(), wantMinted)
				}
				if tc.appFallback {
					_, password, err := r.host.Credentials(t.Context())
					if err != nil || password != "installation-3" || minted.Load() != 3 {
						t.Fatalf("source control did not reuse the renewed token: %q, %v", password, err)
					}
				}
			})
		})
	}
}

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
