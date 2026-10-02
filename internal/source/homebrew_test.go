package source

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestHomebrewSelectsMetadataWithoutDownloading(t *testing.T) {
	m := manager(t)
	requests := 0
	m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.String() != "https://formulae.brew.sh/api/cask/example.json" {
			t.Fatalf("unexpected request %s", req.URL)
		}
		body := `{"token":"example","version":"2.0","url":"https://vendor.test/Example-arm.dmg","sha256":"` + strings.Repeat("a", 64) + `","supported_platforms":["arm64_golden_gate","golden_gate"],"variations":{"golden_gate":{"url":"https://vendor.test/Example-intel.dmg","sha256":"` + strings.Repeat("b", 64) + `"}}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	input := plugin.Input{Resolver: "homebrew", Config: map[string]any{"cask": "example"}}
	entry, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Content.SHA256 != strings.Repeat("a", 64) || entry.Content.Filename != "Example-arm.dmg" || entry.InputVersion != "2.0" || entry.ContentRoot != "" || requests != 1 {
		t.Fatalf("entry %+v requests=%d", entry, requests)
	}
	input.Config["architecture"] = "x86_64"
	intel, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if intel.Content.SHA256 != strings.Repeat("b", 64) || requests != 1 {
		t.Fatalf("intel %+v requests=%d", intel, requests)
	}
}

func TestHomebrewBottleConstraintsAndFrozenReplay(t *testing.T) {
	for _, test := range []struct{ name, patch, failure string }{
		{"standalone", ``, ``},
		{"formula revision", `,"revision":2`, ``},
		{"bottle rebuild", `,"bottle":{"stable":{"rebuild":3,"files":{"arm64_golden_gate":{"cellar":":any_skip_relocation","url":"https://ghcr.io/v2/homebrew/core/example/blobs/sha256:HASH","sha256":"HASH"}}}}`, ``},
		{"relocation", `,"bottle":{"stable":{"files":{"arm64_golden_gate":{"cellar":":any","url":"https://ghcr.io/v2/homebrew/core/example/blobs/sha256:HASH","sha256":"HASH"}}}}`, "relocation"},
		{"dependencies", `,"dependencies":["ncurses"]`, "dependencies"},
		{"post-install", `,"post_install_defined":true`, "post-install"},
		{"no bottle", `,"bottle":{"stable":{"files":{}}}`, "no compatible bottle"},
		{"newer OS", `,"bottle":{"stable":{"files":{"arm64_future":{"cellar":":any_skip_relocation"}}}}`, "no compatible bottle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := manager(t)
			sum := fmt.Sprintf("%x", sha256.Sum256([]byte("bottle")))
			discoveries, downloads := 0, 0
			m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				body := "bottle"
				if req.URL.Host == "formulae.brew.sh" {
					discoveries++
					body = `{"name":"example","versions":{"stable":"1.0"},"bottle":{"stable":{"files":{"arm64_golden_gate":{"cellar":":any_skip_relocation","url":"https://ghcr.io/v2/homebrew/core/example/blobs/sha256:HASH","sha256":"HASH"}}}}` + test.patch + `}`
					body = strings.ReplaceAll(body, "HASH", sum)
				} else {
					downloads++
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			input := plugin.Input{Resolver: "homebrew", Config: map[string]any{"formula": "example"}}
			entry, err := m.Resolve(t.Context(), input)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("wanted %q: %v", test.failure, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if downloads != 0 || entry.Content.SHA256 != sum {
				t.Fatalf("update downloaded: %+v", entry)
			}
			version, root := "1.0.0.0", "example/1.0"
			if test.name == "formula revision" {
				version, root = "1.0.2.0", "example/1.0_2"
			}
			if test.name == "bottle rebuild" {
				version = "1.0.0.3"
			}
			if entry.InputVersion != version || entry.ContentRoot != root {
				t.Fatalf("incorrect bottle identity/root: %+v", entry)
			}
			if _, err = m.FetchLocked(t.Context(), input, entry); err != nil {
				t.Fatal(err)
			}
			if discoveries != 1 || downloads != 1 {
				t.Fatalf("replay discovered: %d %d", discoveries, downloads)
			}
		})
	}
}

func TestHomebrewLanguagesAndMutableCasks(t *testing.T) {
	m := manager(t)
	downloads := 0
	body := "first"
	m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		payload := body
		if req.URL.Host == "formulae.brew.sh" {
			payload = `{"token":"example","version":"latest","url":"https://vendor.test/default.dmg","sha256":"no_check","supported_platforms":["arm64_golden_gate"],"language_variations":[{"languages":["en"],"default":true,"value":"en-US","url":"https://vendor.test/english.dmg","sha256":"no_check"},{"languages":["fr"],"value":"fr","url":"https://vendor.test/french.dmg","sha256":"no_check"}]}`
		} else {
			downloads++
			if req.URL.Path != "/french.dmg" {
				t.Fatalf("wrong language URL: %s", req.URL)
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(payload))}, nil
	})
	input := plugin.Input{Resolver: "homebrew", Config: map[string]any{"cask": "example", "language": "fr"}}
	first, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	body = "second"
	second, _, err := m.Refresh(t.Context(), input, first)
	if err != nil {
		t.Fatal(err)
	}
	if downloads != 2 || first.Content.SHA256 == second.Content.SHA256 {
		t.Fatal("mutable cask reused metadata as content identity")
	}
	if !strings.Contains(string(second.Evidence["homebrew.cask"]), `"language":"fr"`) {
		t.Fatal("language missing from evidence")
	}
	input.Config["language"] = "unknown"
	if _, err := m.Resolve(t.Context(), input); err == nil {
		t.Fatal("unknown language accepted")
	}
}

func TestHomebrewDownloadRequirements(t *testing.T) {
	for _, test := range []struct{ name, specs, failure string }{
		{"browser", `{"user_agent":":browser"}`, ""},
		{"git checkout", `{"branch":"main","only_path":"fonts/example"}`, "unsupported cask download option"},
		{"post", `{"data":{"key":"value"}}`, "unsupported cask download option"},
	} {
		t.Run(test.name, func(t *testing.T) {
			m := manager(t)
			m.Client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
				body := `{"token":"example","name":["Example"],"version":"1","url":"https://vendor.test/example.dmg","sha256":"` + strings.Repeat("a", 64) + `","supported_platforms":["arm64_golden_gate"],"url_specs":` + test.specs + `}`
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			entry, err := m.Resolve(t.Context(), plugin.Input{Resolver: "homebrew", Config: map[string]any{"cask": "example"}})
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("wanted %s: %v", test.failure, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(entry.Download.Headers["User-Agent"], "Mozilla/") {
				t.Fatalf("browser request requirement lost: %+v", entry.Download)
			}
		})
	}
}
