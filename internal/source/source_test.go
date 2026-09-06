package source

import (
	"io"
	"net/http"
	"net/http/httptest"
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
