package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestRefreshAsksHTTPConditionallyWhateverTheLock(t *testing.T) {
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
			if string(previous.Observation) != `{"url":"`+server.URL+`/app.pkg"}` {
				t.Fatalf("lock observation carries transport state: %s", previous.Observation)
			}
			for _, locked := range []Entry{previous, {}} {
				same, cached, err := m.Refresh(t.Context(), input, locked)
				if err != nil || !cached || !same.Equal(previous) || server.bodies.Load() != 1 {
					t.Fatalf("confirmed content was downloaded again: %v cached=%v bodies=%d", err, cached, server.bodies.Load())
				}
			}
			if server.conditionals.Load() != 2 {
				t.Fatalf("conditionals=%d", server.conditionals.Load())
			}
			server.payload.Store("installer v2")
			server.etag.Store(strings.Replace(test.etag, "v1", "v2", 1))
			server.modified.Store(strings.Replace(test.modified, "2015", "2016", 1))
			updated, cached, err := m.Refresh(t.Context(), input, previous)
			if err != nil || cached || updated.Content.Artifact == previous.Content.Artifact || server.bodies.Load() != 2 {
				t.Fatalf("changed content was not downloaded: %v cached=%v", err, cached)
			}
			if same, cached, err := m.Refresh(t.Context(), input, previous); err != nil || !cached || !same.Equal(updated) || server.bodies.Load() != 2 {
				t.Fatalf("new validators were not kept: %v cached=%v bodies=%d", err, cached, server.bodies.Load())
			}
			// Locked recovery must fetch the bytes, never ask whether they changed.
			object, err := m.Store.Path(updated.Content.Artifact)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(object); err != nil {
				t.Fatal(err)
			}
			// The server still confirms the URL, but the bytes are gone.
			if same, cached, err := m.Refresh(t.Context(), input, previous); err != nil || cached || !same.Equal(updated) || server.bodies.Load() != 2 {
				t.Fatalf("absent content was reported cached: %v cached=%v", err, cached)
			}
			conditionals := server.conditionals.Load()
			if hit, err := m.FetchLocked(t.Context(), input, updated); err != nil || hit || server.conditionals.Load() != conditionals || server.bodies.Load() != 3 {
				t.Fatalf("locked fetch was conditional: hit=%v err=%v", hit, err)
			}
		})
	}
}

func TestRefreshAdoptsRotatedValidatorsWithoutChangingTheLock(t *testing.T) {
	server := newConditionalServer(t)
	m := manager(t)
	input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}
	previous, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	server.etag.Store(`"rotated"`)
	server.modified.Store("Thu, 22 Oct 2015 07:28:00 GMT")
	rotated, cached, err := m.Refresh(t.Context(), input, previous)
	if err != nil || cached || !rotated.Equal(previous) || server.bodies.Load() != 2 {
		t.Fatalf("rotated validators changed the entry: %v cached=%v", err, cached)
	}
	if _, cached, err := m.Refresh(t.Context(), input, previous); err != nil || !cached || server.bodies.Load() != 2 {
		t.Fatalf("rotated validators were not adopted: %v cached=%v bodies=%d", err, cached, server.bodies.Load())
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
	same, cached, err := m.Refresh(t.Context(), input, previous)
	if err != nil || cached || !same.Equal(previous) || server.bodies.Load() != 2 || server.conditionals.Load() != 0 {
		t.Fatalf("refresh without validators: %v cached=%v bodies=%d", err, cached, server.bodies.Load())
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
	same, cached, err := m.Refresh(t.Context(), input, previous)
	if err != nil || !cached || !same.Equal(previous) || bodies.Load() != 1 || conditionals.Load() != 1 {
		t.Fatalf("same link was downloaded again: %v cached=%v", err, cached)
	}
	current.Store("/downloads/app-2.0.pkg")
	updated, _, err := m.Refresh(t.Context(), input, previous)
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
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := "installer " + r.URL.Path
		if r.URL.Host == "api.github.com" {
			lookups.Add(1)
			body = fmt.Sprintf(`{"id":12,"tag_name":"v1","assets":[{"id":%d,"name":"App.pkg","browser_download_url":"https://github.com/example/app/releases/download/v%[1]d/App.pkg"}]}`, assetID.Load())
		} else {
			downloads.Add(1)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})
	m := manager(t)
	m.Client.Transport = transport
	input := plugin.Input{Resolver: "github", Config: map[string]any{"repository": "example/app", "asset": "App.pkg"}}
	previous, err := m.Resolve(t.Context(), input)
	if err != nil || previous.Content.Filename != "App.pkg" {
		t.Fatalf("resolve: %v %+v", err, previous.Content)
	}
	if same, cached, err := m.Refresh(t.Context(), input, Entry{}); err != nil || !cached || !same.Equal(previous) || lookups.Load() != 2 || downloads.Load() != 1 {
		t.Fatalf("the cache did not reuse a fetched asset without a lock: %v cached=%v downloads=%d", err, cached, downloads.Load())
	}
	// The lock names the asset without the bytes, so nothing counts as cached.
	cold := manager(t)
	cold.Client.Transport = transport
	if same, cached, err := cold.Refresh(t.Context(), input, previous); err != nil || cached || !same.Equal(previous) || downloads.Load() != 1 {
		t.Fatalf("the lock did not answer for its own asset: %v cached=%v downloads=%d", err, cached, downloads.Load())
	}
	assetID.Store(35)
	replaced, _, err := m.Refresh(t.Context(), input, previous)
	if err != nil || downloads.Load() != 2 || observation(t, replaced).AssetID != 35 {
		t.Fatalf("replaced asset: %v %+v", err, observation(t, replaced))
	}
	// Back on a lock that records the new asset, the cache still knows the old one.
	assetID.Store(34)
	if back, _, err := m.Refresh(t.Context(), input, replaced); err != nil || !back.Equal(previous) || downloads.Load() != 2 {
		t.Fatalf("cache reuse followed the lock rather than the asset: %v downloads=%d", err, downloads.Load())
	}
}

func TestFetchLockedRemembersWhatAMovedSourceServes(t *testing.T) {
	server := newConditionalServer(t)
	m := manager(t)
	input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}
	locked, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.Prune(t.Context()); err != nil {
		t.Fatal(err)
	}
	server.payload.Store("installer v2")
	server.etag.Store(`"v2"`)
	if _, err := m.FetchLocked(t.Context(), input, locked); err == nil || !strings.Contains(err.Error(), "differs from the input lock") || server.bodies.Load() != 2 {
		t.Fatalf("moved source was accepted for the lock: %v", err)
	}
	updated, cached, err := m.Refresh(t.Context(), input, locked)
	if err != nil || !cached || updated.Content.Artifact == locked.Content.Artifact || server.bodies.Load() != 2 {
		t.Fatalf("refresh downloaded what the locked fetch already had: %v cached=%v bodies=%d", err, cached, server.bodies.Load())
	}
}

func TestRefreshTrustsTheSourceIndexOfOneResolverBuild(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "input.pkg")
	if err := os.WriteFile(filename, []byte("installer"), 0o644); err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	m := manager(t)
	register := func(identity string) {
		m.Resolvers["vendor.release"] = Resolver{
			Version: "1", Identity: identity,
			Discover: func(context.Context, plugin.Input) (Discovery, error) {
				return Discovery{Observation: json.RawMessage(`{"release":"1.0"}`), Immutable: true}, nil
			},
			Fetch: func(context.Context, plugin.Input, json.RawMessage) (plugin.Artifact, error) {
				fetches.Add(1)
				return plugin.Artifact{Path: filename, Filename: "input.pkg"}, nil
			},
		}
	}
	input := plugin.Input{Resolver: "vendor.release"}
	register("build-1")
	locked, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, cached, err := m.Refresh(t.Context(), input, Entry{}); err != nil || !cached || fetches.Load() != 1 {
		t.Fatalf("same build fetched a known release again: %v cached=%v", err, cached)
	}
	register("build-2")
	if _, cached, err := m.Refresh(t.Context(), input, Entry{}); err != nil || cached || fetches.Load() != 2 {
		t.Fatalf("new build trusted an older build's fetch: %v cached=%v", err, cached)
	}
	register("build-3")
	if same, _, err := m.Refresh(t.Context(), input, locked); err != nil || !same.Equal(locked) || fetches.Load() != 2 {
		t.Fatalf("new build fetched a release its lock already records: %v", err)
	}
}

func TestRefreshKeepsDeclarationsApart(t *testing.T) {
	server := newConditionalServer(t)
	m := manager(t)
	input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}
	previous, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	other := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/other.pkg"}}
	if _, _, err := m.Refresh(t.Context(), other, previous); err != nil || server.conditionals.Load() != 0 || server.bodies.Load() != 2 {
		t.Fatalf("another declaration reused validators: %v", err)
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
		same, cached, err := m.Refresh(t.Context(), input, previous)
		weak := strings.HasPrefix(etag, "W/")
		want := int64(9)
		if weak {
			want = 18
		}
		if err != nil || cached == weak || !same.Equal(previous) || read.Load() != want {
			t.Fatalf("%s: refresh: %v cached=%v read=%d want=%d", etag, err, cached, read.Load(), want)
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
