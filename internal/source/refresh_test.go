package source

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

// conditionalServer serves one payload with validators and honours conditional requests.
type conditionalServer struct {
	*httptest.Server

	payload, etag, modified atomic.Value
	bodies, conditionals    atomic.Int32
}

func newConditionalServer(t *testing.T) *conditionalServer {
	t.Helper()
	s := &conditionalServer{}
	s.payload.Store("installer v1")
	s.etag.Store(`"v1"`)
	s.modified.Store("Wed, 21 Oct 2015 07:28:00 GMT")
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag, modified := s.etag.Load().(string), s.modified.Load().(string)
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		if modified != "" {
			w.Header().Set("Last-Modified", modified)
		}
		if r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != "" {
			s.conditionals.Add(1)
			if r.Header.Get("If-None-Match") == etag && etag != "" || r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Modified-Since") == modified {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		s.bodies.Add(1)
		_, _ = w.Write([]byte(s.payload.Load().(string)))
	}))
	t.Cleanup(s.Close)
	return s
}

func TestRefreshReusesHTTPContentTheServerConfirms(t *testing.T) {
	for _, test := range []struct{ name, etag, modified string }{
		{"etag", `"v1"`, "Wed, 21 Oct 2015 07:28:00 GMT"},
		{"weak etag", `W/"v1"`, ""},
		{"last modified", "", "Wed, 21 Oct 2015 07:28:00 GMT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := newConditionalServer(t)
			server.etag.Store(test.etag)
			server.modified.Store(test.modified)
			m := manager(t)
			input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}
			previous, err := m.Resolve(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if observed := observation(t, previous); observed.ETag != test.etag || observed.LastModified != test.modified {
				t.Fatalf("hints not recorded: %+v", observed)
			}
			same, err := m.Refresh(t.Context(), input, previous)
			if err != nil {
				t.Fatal(err)
			}
			if !same.Equal(previous) || server.bodies.Load() != 1 || server.conditionals.Load() != 1 {
				t.Fatalf("confirmed content was downloaded again: bodies=%d conditionals=%d", server.bodies.Load(), server.conditionals.Load())
			}
			server.payload.Store("installer v2")
			server.etag.Store(strings.Replace(test.etag, "v1", "v2", 1))
			server.modified.Store(strings.Replace(test.modified, "2015", "2016", 1))
			updated, err := m.Refresh(t.Context(), input, previous)
			if err != nil {
				t.Fatal(err)
			}
			if updated.Content.Artifact == previous.Content.Artifact || server.bodies.Load() != 2 {
				t.Fatal("changed content was not downloaded")
			}
			if observed := observation(t, updated); observed.ETag != server.etag.Load().(string) || observed.LastModified != server.modified.Load().(string) {
				t.Fatalf("changed content kept stale hints: %+v", observed)
			}
			// Locked recovery must fetch the bytes, never ask whether they changed.
			object, err := m.Store.Path(updated.Content.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(object); err != nil {
				t.Fatal(err)
			}
			conditionals := server.conditionals.Load()
			if hit, err := m.FetchLocked(t.Context(), input, updated); err != nil || hit || server.conditionals.Load() != conditionals || server.bodies.Load() != 3 {
				t.Fatalf("locked fetch was conditional: hit=%v err=%v", hit, err)
			}
		})
	}
}

func TestRefreshKeepsHintsForUnchangedBytesAndAdoptsMissingOnes(t *testing.T) {
	server := newConditionalServer(t)
	m := manager(t)
	input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}
	previous, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	// A server that rotates validators on identical bytes must not churn the lock.
	server.etag.Store(`"rotated"`)
	server.modified.Store("Thu, 22 Oct 2015 07:28:00 GMT")
	rotated, err := m.Refresh(t.Context(), input, previous)
	if err != nil {
		t.Fatal(err)
	}
	if !rotated.Equal(previous) || server.bodies.Load() != 2 {
		t.Fatalf("rotated validators changed the entry: %+v", observation(t, rotated))
	}
	// Entries locked before hints existed adopt them once without a new timestamp.
	var stripped nativeObservation
	if err := json.Unmarshal(previous.Observation, &stripped); err != nil {
		t.Fatal(err)
	}
	stripped.ETag, stripped.LastModified = "", ""
	legacy := previous
	legacy.Observation, err = json.Marshal(stripped)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := m.Refresh(t.Context(), input, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if adopted.Equal(legacy) || adopted.Content != legacy.Content || !adopted.ResolvedAt.Equal(legacy.ResolvedAt) || observation(t, adopted).ETag != `"rotated"` {
		t.Fatalf("hints were not adopted: %+v", observation(t, adopted))
	}
	if _, err := m.Refresh(t.Context(), input, adopted); err != nil || server.conditionals.Load() != 2 {
		t.Fatalf("adopted hints were not used: %v", err)
	}
}

func TestRefreshWithoutValidatorsDownloadsEveryTime(t *testing.T) {
	server := newConditionalServer(t)
	server.etag.Store("")
	server.modified.Store("")
	m := manager(t)
	input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}
	previous, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if observation(t, previous).hints() {
		t.Fatal("invented hints")
	}
	same, err := m.Refresh(t.Context(), input, previous)
	if err != nil || !same.Equal(previous) || server.bodies.Load() != 2 || server.conditionals.Load() != 0 {
		t.Fatalf("refresh without validators: %v bodies=%d", err, server.bodies.Load())
	}
}

func TestRefreshRediscoversMatchedURLBeforeAskingTheServer(t *testing.T) {
	var current atomic.Value
	current.Store("/downloads/app-1.0.pkg")
	var bodies, conditionals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			_, _ = fmt.Fprintf(w, `<a href="http://%s%s">download</a>`, r.Host, current.Load().(string))
		case current.Load().(string):
			w.Header().Set("ETag", `"`+r.URL.Path+`"`)
			if r.Header.Get("If-None-Match") != "" {
				conditionals.Add(1)
				if r.Header.Get("If-None-Match") == `"`+r.URL.Path+`"` {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
			bodies.Add(1)
			_, _ = w.Write([]byte("installer " + r.URL.Path))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	m := manager(t)
	input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/page", "match": `http://[^"]+/downloads/app-[0-9.]+\.pkg`}}
	previous, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	same, err := m.Refresh(t.Context(), input, previous)
	if err != nil || !same.Equal(previous) || bodies.Load() != 1 || conditionals.Load() != 1 {
		t.Fatalf("same link was downloaded again: %v", err)
	}
	current.Store("/downloads/app-2.0.pkg")
	updated, err := m.Refresh(t.Context(), input, previous)
	if err != nil || updated.Content.Artifact == previous.Content.Artifact || bodies.Load() != 2 || conditionals.Load() != 1 {
		t.Fatalf("new link was asked conditionally with the old validator: %v", err)
	}
	if observation(t, updated).URL != server.URL+"/downloads/app-2.0.pkg" {
		t.Fatalf("observation URL: %s", observation(t, updated).URL)
	}
}

func TestRefreshReusesGitHubAssetsByIdentity(t *testing.T) {
	var assetID atomic.Int64
	assetID.Store(34)
	var lookups, downloads atomic.Int32
	m := manager(t)
	m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := "installer"
		if r.URL.Host == "api.github.com" {
			lookups.Add(1)
			body = fmt.Sprintf(`{"id":12,"tag_name":"v1","assets":[{"id":%d,"name":"App.pkg","browser_download_url":"https://github.com/example/app/releases/download/v1/App.pkg"}]}`, assetID.Load())
		} else {
			downloads.Add(1)
			if r.Header.Get("If-None-Match") != "" {
				t.Error("GitHub download sent a conditional header")
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	input := plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "asset": "App.pkg"}}
	previous, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if observation(t, previous).hints() {
		t.Fatal("GitHub observation recorded HTTP hints")
	}
	same, err := m.Refresh(t.Context(), input, previous)
	if err != nil || !same.Equal(previous) || lookups.Load() != 2 || downloads.Load() != 1 {
		t.Fatalf("unchanged asset was downloaded: %v lookups=%d downloads=%d", err, lookups.Load(), downloads.Load())
	}
	assetID.Store(35)
	replaced, err := m.Refresh(t.Context(), input, previous)
	if err != nil || downloads.Load() != 2 || observation(t, replaced).AssetID != 35 || !replaced.ResolvedAt.Equal(previous.ResolvedAt) {
		t.Fatalf("replaced asset: %v %+v", err, observation(t, replaced))
	}
}

func TestRefreshResolvesLocalAndForeignEntriesInFull(t *testing.T) {
	server := newConditionalServer(t)
	m := manager(t)
	input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}
	previous, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	other := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/other.pkg"}}
	if _, err := m.Refresh(t.Context(), other, previous); err != nil || server.conditionals.Load() != 0 || server.bodies.Load() != 2 {
		t.Fatalf("foreign declaration reused another lock's hints: %v", err)
	}
}

func manager(t *testing.T) *Manager {
	t.Helper()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(store, t.TempDir(), false)
}

// TestRefreshTrustsRepeatedStrongValidators covers hosts that answer
// conditional requests with the full content: matching strong validators
// confirm the lock without a transfer, while a weak ETag still downloads.
func TestRefreshTrustsRepeatedStrongValidators(t *testing.T) {
	for _, etag := range []string{`"abc-1"`, `W/"abc-1"`} {
		var read atomic.Int64
		m := manager(t)
		m.Client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			header := http.Header{"Etag": {etag}, "Last-Modified": {"Mon, 20 Jul 2026 08:52:31 GMT"}}
			return &http.Response{StatusCode: http.StatusOK, Header: header, ContentLength: 9, Body: io.NopCloser(countingReader{strings.NewReader("installer"), &read})}, nil
		})
		input := plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://example.test/app.pkg"}}
		previous, err := m.Resolve(t.Context(), input)
		if err != nil || read.Load() != 9 {
			t.Fatalf("%s: resolve: %v read=%d", etag, err, read.Load())
		}
		same, err := m.Refresh(t.Context(), input, previous)
		want := int64(9)
		if strings.HasPrefix(etag, "W/") {
			want = 18
		}
		if err != nil || !same.Equal(previous) || read.Load() != want {
			t.Fatalf("%s: refresh: %v read=%d want=%d", etag, err, read.Load(), want)
		}
	}
}

type countingReader struct {
	io.Reader

	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n.Add(int64(n))
	return n, err
}
