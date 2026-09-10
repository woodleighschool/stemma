package source

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
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
	s := config.Source{Type: "github", Repository: "example/app", Release: "v1.2.3", Asset: "App.pkg"}
	entry, err := m.Resolve(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Release != "v1.2.3" || entry.ReleaseID != 12 || entry.AssetID != 34 {
		t.Fatalf("release identity changed: %+v", entry)
	}
	if _, err := m.Acquire(t.Context(), s, entry); err != nil {
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
	s := config.Source{Type: "http", URL: "https://vendor.example/download", Match: `https://cdn\.example/App\.pkg\?version=\d+&arch=arm64`, Token: "private"}
	entry, err := m.Resolve(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if entry.URL != "https://cdn.example/App.pkg?version=1&arch=arm64" || entry.Filename != "App.pkg" {
		t.Fatalf("unexpected discovery: %+v", entry)
	}
	filename, err := store.Path(entry.Artifact)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(t.Context(), s, entry); err != nil {
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
		{"partial", "App.pkg", `App\.pkg`, "complete stable URL"},
		{"signed", "https://cdn.example/App.pkg?token=secret", `https://cdn\.example/\S+`, "complete stable URL"},
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
			_, err = m.Resolve(t.Context(), config.Source{Type: "http", URL: "https://vendor.example/download", Match: tc.pattern, Filename: "App.pkg"})
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
	_, err = manager.Resolve(t.Context(), config.Source{Type: "http", URL: server.URL + "/app.pkg"})
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
	s := config.Source{Type: "http", URL: server.URL + "/fwlink?linkid=853070", Filename: "CompanyPortal.pkg"}
	entry, err := m.Resolve(t.Context(), s)
	if err != nil {
		t.Fatal(err)
	}
	if entry.URL != s.URL || strings.Contains(entry.URL, "temporary-signature") {
		t.Fatalf("lock retained redirected URL: %q", entry.URL)
	}
}
