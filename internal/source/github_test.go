package source

import (
	"cmp"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

func TestGitHubObservationValidationOnDiscoveryAndReplay(t *testing.T) {
	const body = "installer"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	valid := githubObservation{URL: "https://github.com/example/app/releases/download/v1/App.pkg", Release: "v1", ReleaseID: 12, AssetID: 34, Filename: "App.pkg", SHA256: digest}
	for _, test := range []struct {
		name, failure string
		change        func(*githubObservation)
	}{
		{"repository", "configured GitHub repository", func(o *githubObservation) { o.URL = "https://github.com/other/app/releases/download/v1/App.pkg" }},
		{"insecure origin", "configured GitHub repository", func(o *githubObservation) { o.URL = "http://github.com/example/app/releases/download/v1/App.pkg" }},
		{"release selection", "configured GitHub release", func(o *githubObservation) { o.Release = "v2" }},
		{"URL selection", "selected release asset", func(o *githubObservation) { o.URL = "https://github.com/example/app/releases/download/v2/App.pkg" }},
		{"release identity", "release and asset IDs", func(o *githubObservation) { o.ReleaseID = 0 }},
		{"asset identity", "release and asset IDs", func(o *githubObservation) { o.AssetID = 0 }},
		{"digest conflict", "declared sha256 disagrees", func(o *githubObservation) { o.SHA256 = strings.Repeat("0", 64) }},
		{"invalid digest", "invalid sha256", func(o *githubObservation) { o.SHA256 = "invalid" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := manager(t)
			selected := valid
			test.change(&selected)
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" {
					t.Fatal("invalid observation reached artifact transfer")
				}
				asset := map[string]any{"id": selected.AssetID, "name": selected.Filename, "browser_download_url": selected.URL, "digest": "sha256:" + selected.SHA256}
				data, err := json.Marshal(map[string]any{"id": selected.ReleaseID, "tag_name": selected.Release, "assets": []any{asset}})
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})
			m.Client.Transport = transport
			input := plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "release": "v1", "asset": "App.pkg", "sha256": digest, "token": "synthetic-token"}}
			_, err := m.Resolve(t.Context(), input)
			if err == nil || !strings.Contains(err.Error(), test.failure) {
				t.Fatalf("discovery: %v, want %q", err, test.failure)
			}
			selected = valid
			m = New(m.Store, m.Root, false)
			m.Client.Transport = transport
			entry, err := m.Resolve(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			test.change(&selected)
			entry.Observation, err = json.Marshal(selected)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.FetchLocked(t.Context(), input, entry); err == nil || !strings.Contains(err.Error(), test.failure) {
				t.Fatalf("cold replay: %v, want %q", err, test.failure)
			}
		})
	}
}

func TestGitHubReplayRejectsReleasePathTraversal(t *testing.T) {
	m := manager(t)
	input := plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "asset": "App.pkg", "token": "synthetic-token"}}
	observed := githubObservation{
		URL:       "https://github.com/example/app/releases/download/../../../../other/app/releases/download/v1/App.pkg",
		Release:   "../../../../other/app/releases/download/v1",
		ReleaseID: 12,
		AssetID:   34,
		Filename:  "App.pkg",
	}
	data, err := json.Marshal(observed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.githubResolver().Acquire(t.Context(), input, data); err == nil || !strings.Contains(err.Error(), "selected release asset") {
		t.Fatalf("accepted a release path outside the configured repository: %v", err)
	}
}

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
			cachedPath, err := store.Path(cas.Ref{SHA256: entry.Content.SHA256})
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

func TestGitHubLookupReportsHTTPStatus(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, t.TempDir(), false)
	m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)), Header: http.Header{}, Request: r}, nil
	})
	_, err = m.Resolve(t.Context(), plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "release": "v9", "asset": "App.pkg"}})
	if err == nil || !strings.Contains(err.Error(), "GitHub release lookup returned HTTP 404") {
		t.Fatalf("missing release: %v", err)
	}
}

func TestGitHubDiscoveryKeepsTokenAtAPIOrigin(t *testing.T) {
	for _, target := range []string{"https://elsewhere.example/release", "http://api.github.com/release", "https://api.github.com/repositories/1/releases/latest"} {
		t.Run(target, func(t *testing.T) {
			m := manager(t)
			requests := 0
			m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests++
				if r.URL.Host != "api.github.com" || r.URL.Scheme != "https" {
					if r.Header.Get("Authorization") != "" {
						t.Fatal("GitHub API token crossed its origin")
					}
				} else if r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Fatal("API request lost authentication")
				}
				if requests == 1 {
					return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
				}
				return &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"message":"Not Found"}`)), Request: r}, nil
			})
			var observed githubObservation
			err := m.github(t.Context(), githubConfig{Repository: "example/app", Asset: "App.pkg", Token: "synthetic-token"}, &observed)
			wantRequests, failure := 2, "HTTP 404"
			if strings.HasPrefix(target, "http:") {
				wantRequests, failure = 1, "must not downgrade"
			}
			if requests != wantRequests || err == nil || !strings.Contains(err.Error(), failure) {
				t.Fatalf("redirect: requests=%d, err=%v", requests, err)
			}
		})
	}
}

func TestGitHubLockedFetchVerification(t *testing.T) {
	for _, test := range []struct{ name, address, body, failure string }{
		{"origin", "https://github.com/other/app/releases/download/v1/App.pkg", "installer", "configured GitHub repository"},
		{"content", "https://github.com/example/app/releases/download/v1/App.pkg", "changed installer", "differs from the input lock"},
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
			cachedPath, err := store.Path(cas.Ref{SHA256: entry.Content.SHA256})
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

func TestGitHubReleaseDiscovery(t *testing.T) {
	stable := `{"id":12,"tag_name":"v1","published_at":"2026-01-02T00:00:00Z","prerelease":false,"assets":[{"id":34,"name":"App.pkg","browser_download_url":"https://github.com/example/app/releases/download/v1/App.pkg"}]}`
	preview := `{"id":13,"tag_name":"v2-beta","published_at":"2026-01-03T00:00:00Z","prerelease":true,"assets":[{"id":35,"name":"App.pkg","browser_download_url":"https://github.com/example/app/releases/download/v2-beta/App.pkg"}]}`
	draft := `{"id":14,"tag_name":"draft","draft":true,"published_at":null}`
	for _, test := range []struct {
		name, release, body, tag, failure string
		include, paginate                 bool
	}{
		{name: "default stable", body: stable, tag: "v1"},
		{name: "newer prerelease", include: true, body: "[" + stable + "," + draft + "," + preview + "]", tag: "v2-beta"},
		{name: "newer stable", include: true, release: "latest", body: "[" + strings.Replace(preview, "2026-01-03", "2026-01-01", 1) + "," + stable + "]", tag: "v1"},
		{name: "equal publication times", include: true, body: "[" + strings.Replace(preview, "2026-01-03", "2026-01-02", 1) + "," + stable + "]", tag: "v2-beta"},
		{name: "explicit tag", include: true, release: "v1", body: stable, tag: "v1"},
		{name: "pagination", include: true, paginate: true, body: "[" + preview + "]", tag: "v2-beta"},
		{name: "empty", include: true, body: "[]", failure: "no published GitHub releases"},
		{name: "drafts only", include: true, body: "[" + draft + "]", failure: "no published GitHub releases"},
		{name: "explicit draft", include: true, release: "draft", body: draft, failure: "draft releases are not supported"},
		{name: "no asset fallback", include: true, body: "[" + stable + "," + strings.Replace(preview, "App.pkg", "Other.pkg", 1) + "]", failure: "has no asset matching"},
		{name: "tag glob", release: "v1*", body: "[" + strings.Replace(preview, "true", "false", 1) + "," + stable + "]", tag: "v1"},
		{name: "tag glob skips prereleases", release: "v*", body: "[" + preview + "," + draft + "," + stable + "]", tag: "v1"},
		{name: "tag glob prereleases", include: true, release: "v*", body: "[" + stable + "," + preview + "]", tag: "v2-beta"},
		{name: "tag glob pagination", release: "v1*", paginate: true, body: "[" + preview + "]", tag: "v1"},
		{name: "tag glob no match", release: "v3*", body: "[" + stable + "," + preview + "]", failure: `no published GitHub release tag matches "v3*"`},
		{name: "tag glob no asset fallback", release: "v*", body: "[" + strings.Replace(strings.Replace(preview, "true", "false", 1), "App.pkg", "Other.pkg", 1) + "," + stable + "]", failure: "has no asset matching"},
		{name: "malformed tag glob", release: "v[", failure: "invalid GitHub release pattern"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := New(store, t.TempDir(), false)
			discoveries, downloads := 0, 0
			m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				body, header := "installer", http.Header{}
				if r.URL.Host == "api.github.com" {
					discoveries++
					endpoint := "/repos/example/app/releases/latest"
					latest := test.release == "" || test.release == "latest"
					listed := strings.ContainsAny(test.release, "*?[") || latest && test.include
					if listed {
						endpoint = "/repos/example/app/releases"
					} else if !latest {
						endpoint = "/repos/example/app/releases/tags/" + test.release
					}
					query := r.URL.Query()
					if r.URL.Path != endpoint || listed && (query.Get("per_page") != "100" || cmp.Or(query.Get("page"), "1") != strconv.Itoa(discoveries)) {
						t.Fatalf("request %s, want page %d of %s", r.URL, discoveries, endpoint)
					}
					body = test.body
					if test.paginate && discoveries == 1 {
						body = "[" + strings.Repeat(draft+",", 99) + stable + "]"
						header.Set("Link", `<https://api.github.com/repositories/1/releases?per_page=100&page=2>; rel="next", <https://api.github.com/repositories/1/releases?per_page=100&page=2>; rel="last"`)
					}
				} else {
					downloads++
					if r.URL.Path != "/example/app/releases/download/"+test.tag+"/App.pkg" {
						t.Fatalf("download %s", r.URL)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: header, Request: r}, nil
			})
			input := plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "asset": "App.pkg", "release": test.release, "include_prereleases": test.include}}
			entry, err := m.Resolve(t.Context(), input)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) || downloads != 0 {
					t.Fatalf("err=%v downloads=%d", err, downloads)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			observed := observation(t, entry)
			wantRelease, wantAsset := int64(12), int64(34)
			if test.tag == "v2-beta" {
				wantRelease, wantAsset = 13, 35
			}
			if observed.Release != test.tag || observed.ReleaseID != wantRelease || observed.AssetID != wantAsset {
				t.Fatalf("lost release identity: %+v", observed)
			}
			cached, err := store.Path(cas.Ref{SHA256: entry.Content.SHA256})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(cached); err != nil {
				t.Fatal(err)
			}
			before := discoveries
			if _, err := m.FetchLocked(t.Context(), input, entry); err != nil {
				t.Fatal(err)
			}
			if discoveries != before || downloads != 2 {
				t.Fatalf("locked replay rediscovered: discoveries=%d downloads=%d", discoveries, downloads)
			}
			input.Config["include_prereleases"] = !test.include
			if _, err := m.FetchLocked(t.Context(), input, entry); err == nil || !strings.Contains(err.Error(), "stale input lock") {
				t.Fatalf("changed discovery policy accepted: %v", err)
			}
		})
	}
}
