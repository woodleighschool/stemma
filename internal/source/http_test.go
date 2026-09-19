package source

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

func TestHTTPFilenameAndLockedRecovery(t *testing.T) {
	for _, tc := range []struct{ name, disposition, target, override, want string }{
		{"disposition", `attachment; filename="Vendor Setup.pkg"`, "/download", "", "Vendor Setup.pkg"},
		{"encoded disposition", `attachment; filename="fallback.pkg"; filename*=UTF-8''Vendor%20Setup.pkg`, "/download", "", "Vendor Setup.pkg"},
		{"override", `attachment; filename="Vendor.pkg"`, "/redirected.pkg", "Pinned.pkg", "Pinned.pkg"},
		{"final URL", "", "/Vendor%20Setup.pkg", "", "Vendor Setup.pkg"},
		{"original URL", "", "/", "", "original.pkg"},
		{"unsafe disposition", `attachment; filename="../escape.pkg"`, "/safe.pkg", "", "safe.pkg"},
		{"malformed disposition", `attachment; filename="unfinished`, "/safe.pkg", "", "safe.pkg"},
		{"unsafe URL", "", "/unsafe%5Cname.pkg", "", "original.pkg"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int64
			var disposition atomic.Value
			disposition.Store(tc.disposition)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("unexpected preflight request: %s", r.Method)
				}
				if r.URL.Path == "/original.pkg" {
					http.Redirect(w, r, tc.target, http.StatusFound)
					return
				}
				requests.Add(1)
				w.Header().Set("Content-Disposition", disposition.Load().(string))
				_, _ = io.WriteString(w, "immutable vendor installer")
			}))
			defer server.Close()
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := New(store, t.TempDir(), false)
			input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/original.pkg"}}
			if tc.override != "" {
				input.Config["filename"] = tc.override
			}
			entry, err := manager.Resolve(t.Context(), input)
			if err != nil || entry.Content.Filename != tc.want || requests.Load() != 1 {
				t.Fatalf("resolve: name=%q requests=%d error=%v", entry.Content.Filename, requests.Load(), err)
			}
			cached, err := store.Path(entry.Content.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(cached); err != nil {
				t.Fatal(err)
			}
			disposition.Store(`attachment; filename="changed.pkg"`)
			if cached, err := manager.FetchLocked(t.Context(), input, entry); err != nil || cached || requests.Load() != 2 {
				t.Fatalf("locked recovery: cached=%v requests=%d error=%v", cached, requests.Load(), err)
			}
		})
	}
}

func TestHTTPHeadersStayWithinTheirOrigin(t *testing.T) {
	for _, discovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "redirect", true: "page match"}[discovery], func(t *testing.T) {
			var cdnReads atomic.Int64
			cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cdnReads.Add(1)
				for _, name := range []string{"Authorization", "X-Vendor-Key", "Cookie"} {
					if r.Header.Get(name) != "" {
						t.Errorf("%s escaped its source origin", name)
					}
				}
				if referer := r.Header.Get("Referer"); referer != "" && !strings.HasPrefix(referer, "http://"+r.Host+"/") {
					t.Error("source Referer escaped its origin")
				}
				if r.Header.Get("User-Agent") != "FixtureDownloader" || r.Header.Get("Accept") != "application/octet-stream" {
					t.Error("download headers were lost")
				}
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, "/payload.pkg", http.StatusFound)
					return
				}
				_, _ = io.WriteString(w, "payload")
			}))
			defer cdn.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer synthetic-secret" || r.Header.Get("X-Vendor-Key") != "synthetic-key" {
					t.Error("source credentials were lost")
				}
				switch {
				case r.URL.Path == "/start" && !discovery:
					http.Redirect(w, r, "/download", http.StatusFound)
				case discovery:
					_, _ = io.WriteString(w, cdn.URL+"/redirect")
				default:
					http.Redirect(w, r, cdn.URL+"/redirect", http.StatusFound)
				}
			}))
			defer source.Close()
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			input := plugin.Input{Resolver: "http", Config: map[string]any{"url": source.URL + "/start", "headers": map[string]string{
				"authorization": "Bearer synthetic-secret", "X-Vendor-Key": "synthetic-key", "Cookie": "session=synthetic-cookie",
				"User-Agent": "FixtureDownloader", "Accept": "application/octet-stream", "Referer": source.URL,
			}}}
			if discovery {
				input.Config["match"] = `http://127\.0\.0\.1:\d+/redirect`
			}
			entry, err := New(store, t.TempDir(), false).Resolve(t.Context(), input)
			if err != nil || cdnReads.Load() != 2 {
				t.Fatalf("download: CDN requests=%d error=%v", cdnReads.Load(), err)
			}
			encoded, _ := json.Marshal(entry)
			if strings.Contains(string(encoded), "synthetic-") {
				t.Fatal("lock contains request credentials")
			}
		})
	}
}

func TestHTTPHeaderValidationAndDeclaration(t *testing.T) {
	manager := New(nil, t.TempDir(), false)
	for _, headers := range []any{
		map[string]string{"bad name": "value"}, map[string]string{"X-Value": "one\r\nTwo: injected"},
		map[string]string{"Accept": "one", "accept": "two"}, map[string]string{"Host": "another.example"},
		map[string][]string{"Accept": {"one", "two"}},
	} {
		input := plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://example.test/download", "headers": headers}}
		if _, _, err := manager.Declaration(input); err == nil {
			t.Fatalf("accepted invalid headers: %T", headers)
		}
	}
	headers := map[string]string{"Accept": "application/octet-stream", "Authorization": "Bearer first"}
	input := plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://example.test/download", "headers": headers}}
	_, first, err := manager.Declaration(input)
	if err != nil {
		t.Fatal(err)
	}
	headers["Authorization"] = "Bearer rotated"
	if _, rotated, err := manager.Declaration(input); err != nil || rotated != first {
		t.Fatalf("credential rotation changed declaration: %v", err)
	}
	headers["accept"] = headers["Accept"]
	delete(headers, "Accept")
	if _, normalized, err := manager.Declaration(input); err != nil || normalized != first {
		t.Fatalf("header casing changed declaration: %v", err)
	}
	delete(headers, "accept")
	headers["Accept"] = "application/zip"
	input.Config["headers"] = headers
	if _, changed, err := manager.Declaration(input); err != nil || changed == first {
		t.Fatalf("request representation did not change declaration: %v", err)
	}
	input.Config["token"] = "another-token"
	if _, _, err := manager.Declaration(input); err == nil {
		t.Fatal("accepted conflicting authentication")
	}
}

func TestHTTPDiscoveryResolvesURLReferences(t *testing.T) {
	for _, test := range []struct{ name, references, want string }{
		{"absolute", "https://cdn.example/App.pkg", "https://cdn.example/App.pkg"},
		{"protocol relative", "//cdn.example/App.pkg", "https://cdn.example/App.pkg"},
		{"root relative", "/App.pkg", "https://pages.example/App.pkg"},
		{"path relative", "../files/App.pkg", "https://pages.example/releases/files/App.pkg"},
		{"equivalent", "./App.pkg\nhttps://pages.example/releases/mac/App.pkg", "https://pages.example/releases/mac/App.pkg"},
		{"query", "App.pkg?version=1&amp;arch=arm64", "https://pages.example/releases/mac/App.pkg?version=1&arch=arm64"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := New(store, t.TempDir(), false)
			pages, downloads := 0, 0
			m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Request: r}
				body := "installer"
				switch r.URL.String() {
				case "https://vendor.example/start":
					if r.Header.Get("Authorization") != "Bearer private" || r.Header.Get("X-Vendor-Key") != "private" {
						t.Error("source credentials missing")
					}
					response.StatusCode = http.StatusFound
					response.Header.Set("Location", "https://pages.example/releases/mac/index.html")
				case "https://pages.example/releases/mac/index.html":
					pages++
					body = test.references
				case test.want:
					downloads++
				default:
					t.Fatalf("unexpected request %s", r.URL)
				}
				if r.URL.Host != "vendor.example" && (r.Header.Get("Authorization") != "" || r.Header.Get("X-Vendor-Key") != "" || r.Header.Get("Cookie") != "") {
					t.Error("credentials escaped source origin")
				}
				response.Body = io.NopCloser(strings.NewReader(body))
				return response, nil
			})
			patterns := strings.Split(html.UnescapeString(test.references), "\n")
			for i := range patterns {
				patterns[i] = regexp.QuoteMeta(patterns[i])
			}
			input := plugin.Input{Resolver: "http", Config: map[string]any{
				"url": "https://vendor.example/start", "match": `(?m)^(?:` + strings.Join(patterns, "|") + `)$`, "token": "private",
				"headers": map[string]string{"X-Vendor-Key": "private", "Cookie": "session=private"},
			}}
			entry, err := m.Resolve(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if observation(t, entry).URL != test.want || entry.Content.Filename != "App.pkg" {
				t.Fatalf("unexpected resolved input: %+v", entry)
			}
			cached, err := store.Path(entry.Content.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(cached); err != nil {
				t.Fatal(err)
			}
			// A URL reference pattern need not match its resolved absolute URL.
			if _, err := m.FetchLocked(t.Context(), input, entry); err != nil {
				t.Fatal(err)
			}
			if pages != 1 || downloads != 2 {
				t.Fatalf("pages=%d downloads=%d", pages, downloads)
			}
			input.Config["match"] = `different\.pkg`
			if _, err := m.FetchLocked(t.Context(), input, entry); err == nil || !strings.Contains(err.Error(), "stale input lock") {
				t.Fatalf("changed pattern accepted: %v", err)
			}
		})
	}
}
