package lockfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

const resource = "stemma/v1alpha1/Software/app"

func manager(t *testing.T) *source.Manager {
	t.Helper()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return source.New(store, t.TempDir(), false)
}
func inputset(resolver string, config map[string]any) map[string]map[string]plugin.Input {
	return map[string]map[string]plugin.Input{resource: {"source": {Resolver: resolver, Config: config}}}
}
func prepare(t *testing.T, m *source.Manager, inputs map[string]map[string]plugin.Input, opts Options) (Result, error) {
	t.Helper()
	return Prepare(t.Context(), m.Root, inputs, nil, m, opts)
}
func entry(result Result) source.Entry { return result.File.Inputs[resource]["source"] }
func save(t *testing.T, m *source.Manager, file File) {
	t.Helper()
	if err := Save(m.Root, file); err != nil {
		t.Fatal(err)
	}
}

// lockedBytes reads every lock document of the project, in path order.
func lockedBytes(t *testing.T, m *source.Manager) []byte {
	t.Helper()
	var data []byte
	err := documents(m.Root, func(name string, _ fs.DirEntry) error {
		document, err := os.ReadFile(filepath.Join(m.Root, filepath.FromSlash(name)))
		data = append(append(data, name...), document...)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// writeDocument replaces one lock document with arbitrary bytes.
func writeDocument(t *testing.T, m *source.Manager, name, data string) {
	t.Helper()
	filename := filepath.Join(m.Root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLockedColdWarmOfflineAndRefresh(t *testing.T) {
	var requests atomic.Int32
	var payload atomic.Value
	payload.Store("original installer")
	var token atomic.Value
	token.Store("first-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+token.Load().(string) {
			t.Error("wrong acquisition credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(payload.Load().(string)))
	}))
	t.Cleanup(server.Close)
	m := manager(t)
	inputs := inputset("http", map[string]any{"url": server.URL + "/app.pkg", "token": "first-token"})
	if _, err := prepare(t, m, inputs, Options{}); err == nil || err.Error() != "lockfile: missing; run stemma update" {
		t.Fatalf("missing frozen lock: %v", err)
	}
	first, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || requests.Load() != 1 {
		t.Fatal("initial input did not resolve once")
	}
	original := entry(first)
	before := lockedBytes(t, m)
	if bytes.Contains(before, []byte("first-token")) {
		t.Fatal("lock retained a credential")
	}
	inputs["stemma/v1alpha1/Policy/source-free"] = map[string]plugin.Input{}
	warm, err := prepare(t, m, inputs, Options{Offline: true})
	if err != nil || warm.Changed || !warm.CacheHits[resource]["source"] || requests.Load() != 1 {
		t.Fatalf("warm offline input changed: %v", err)
	}
	refreshed, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || refreshed.Changed || !entry(refreshed).Equal(original) {
		t.Fatalf("same bytes changed the reviewed entry: %v", err)
	}
	object, err := m.Store.Path(cas.Ref{SHA256: original.Content.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	count := requests.Load()
	if _, err := prepare(t, m, inputs, Options{Offline: true}); err == nil || requests.Load() != count {
		t.Fatal("offline cache loss attempted remote acquisition")
	}
	token.Store("rotated-token")
	inputs[resource]["source"].Config["token"] = "rotated-token"
	recovered, err := prepare(t, m, inputs, Options{})
	if err != nil || recovered.Changed || recovered.CacheHits[resource]["source"] {
		t.Fatalf("credential rotation or cold recovery changed lock: %v", err)
	}
	if !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("cold recovery rewrote reviewed inputs")
	}
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	payload.Store("substituted installer")
	if _, err := prepare(t, m, inputs, Options{}); err == nil {
		t.Fatal("cold recovery accepted substituted upstream bytes")
	}
	if !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("failed recovery rewrote lock")
	}
	current, err := prepare(t, m, inputs, Options{IgnoreInputs: true})
	if err != nil || entry(current).Content == original.Content || !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatalf("ignoring input locks did not resolve the source without saving: %v", err)
	}
	updated, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || !updated.Changed || entry(updated).Content == original.Content {
		t.Fatalf("refresh failed to record changed input: %v", err)
	}
}

// TestIgnoringInputLocksStillReadsTheLockfile keeps "no input lock" apart from
// "no lockfile": the file still has to load.
func TestIgnoringInputLocksStillReadsTheLockfile(t *testing.T) {
	m := manager(t)
	writeDocument(t, m, "stemma.lock/Software/app.yaml", "inputs: [\n")
	if _, err := prepare(t, m, inputset("file", map[string]any{"path": "app.pkg"}), Options{IgnoreInputs: true}); err == nil || !strings.HasPrefix(err.Error(), "lockfile: ") {
		t.Fatalf("an unreadable lockfile was ignored: %v", err)
	}
}

func TestReleaseObservationChangesKeepContentIdentity(t *testing.T) {
	var currentRelease atomic.Value
	currentRelease.Store("v1.2.3")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			release := currentRelease.Load().(string)
			_, _ = fmt.Fprintf(w, `{"id":12,"tag_name":%q,"assets":[{"id":34,"name":"App.pkg","browser_download_url":"https://github.com/example/app/releases/download/%s/App.pkg"}]}`, release, release)
			return
		}
		_, _ = w.Write([]byte("unchanged installer"))
	}))
	t.Cleanup(server.Close)
	m := manager(t)
	m.Client.Transport = releaseTransport{server}
	inputs := inputset("github", map[string]any{"repository": "example/app", "release": "latest", "asset": "App.pkg"})
	first, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	previous := entry(first)
	release := "v1.2.4"
	currentRelease.Store(release)
	// Each invocation shares discovery within the run and revalidates on the next.
	client := m.Client
	m = source.New(m.Store, m.Root, false)
	m.Client = client
	current, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if !current.Changed || entry(current).Content != previous.Content || !bytes.Contains(entry(current).Observation, []byte(release)) {
		t.Fatal("resolver observation changed content identity")
	}
	if _, err := prepare(t, m, inputs, Options{}); err != nil {
		t.Fatal(err)
	}
	inputs[resource]["source"].Config["asset"] = "App-*.pkg"
	if _, err := prepare(t, m, inputs, Options{}); err == nil || !strings.Contains(err.Error(), "input declaration changed since it was locked") {
		t.Fatalf("frozen run accepted changed asset pattern: %v", err)
	}
	inputs[resource]["source"].Config["asset"] = "App*.pkg"
	updated, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Changed || entry(updated).Declaration == entry(current).Declaration || entry(updated).Content != entry(current).Content {
		t.Fatal("pattern update lost content identity or failed to replace declaration")
	}
	if _, err := prepare(t, m, inputs, Options{}); err != nil {
		t.Fatal(err)
	}
}

type releaseTransport struct{ server *httptest.Server }

func (transport releaseTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	forwarded := request.Clone(request.Context())
	forwarded.URL.Scheme = "http"
	forwarded.URL.Host = strings.TrimPrefix(transport.server.URL, "http://")
	return transport.server.Client().Transport.RoundTrip(forwarded)
}

func TestNamedLocalInputsCommitModesAndSymlinkTargets(t *testing.T) {
	m := manager(t)
	file := filepath.Join(m.Root, "postinstall")
	if err := os.WriteFile(file, []byte("script"), 0o444); err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(m.Root, "payload")
	if err := os.Mkdir(tree, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(tree, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(tree, "current")
	if err := os.Symlink("one", link); err != nil {
		t.Fatal(err)
	}
	inputs := map[string]map[string]plugin.Input{resource: {
		"script":  {Resolver: "file", Config: map[string]any{"path": "postinstall"}},
		"payload": {Resolver: "local", Config: map[string]any{"base": "payload", "include": []string{"**"}}},
	}}
	first, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.File.Inputs[resource]) != 2 || first.File.Inputs[resource]["script"].Content.Mode != 0o444 || !first.File.Inputs[resource]["payload"].Content.Tree {
		t.Fatal("named input representation was lost")
	}
	script := first.File.Inputs[resource]["script"]
	before := lockedBytes(t, m)
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, inputs, Options{}); err == nil {
		t.Fatal("frozen input ignored file mode change")
	}
	if !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("failed mode check changed lock")
	}
	second, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if second.File.Inputs[resource]["script"].Content.SHA256 != script.Content.SHA256 || second.File.Inputs[resource]["script"].Content.Mode != uint32(info.Mode().Perm()) {
		t.Fatal("mode-only change did not preserve byte identity")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("two", link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, inputs, Options{}); err == nil {
		t.Fatal("warm cache hid symlink target change")
	}
	third, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if third.File.Inputs[resource]["payload"].Content.SHA256 == second.File.Inputs[resource]["payload"].Content.SHA256 {
		t.Fatal("tree digest omitted symlink identity")
	}
	if err := os.Chmod(filepath.Join(tree, "one"), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, inputs, Options{Offline: true}); err == nil {
		t.Fatal("offline tree ignored child mode change")
	}
}

func TestResolverOwnedObservationAndSharedResolution(t *testing.T) {
	m := manager(t)
	file := filepath.Join(t.TempDir(), "input.bin")
	data := []byte("external resolver bytes")
	if err := os.WriteFile(file, data, 0o640); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	artifact := plugin.Artifact{Path: file, Filename: "input.bin", Evidence: map[string]json.RawMessage{"vendor.release": json.RawMessage(`{ "version": "1.2", "id": 9007199254740993 }`)}}
	observation := json.RawMessage(`{"revision":{"id":9007199254740993,"labels":["stable"]},"cursor":"opaque"}`)
	discoveries, fetches := 0, 0
	resolver := source.Resolver{Version: "resolver-1", Fingerprint: func(input plugin.Input) (string, error) {
		safe := maps.Clone(input.Config)
		delete(safe, "credential")
		data, err := json.Marshal(safe)
		digest := sha256.Sum256(data)
		return hex.EncodeToString(digest[:]), err
	}, Discover: func(_ context.Context, input plugin.Input) (source.Discovery, error) {
		if input.Base != "software/App" {
			t.Fatal("external resolver lost its resource-relative context")
		}
		discoveries++
		return source.Discovery{Observation: observation}, nil
	}, Acquire: func(_ context.Context, input plugin.Input, observed json.RawMessage) (source.Acquisition, error) {
		if input.Base != "software/App" {
			t.Fatal("external fetch lost its resource-relative context")
		}
		fetches++
		if !bytes.Contains(observed, []byte("9007199254740993")) {
			t.Fatal("opaque observation lost integer precision")
		}
		return source.Acquisition{Artifact: &artifact}, nil
	}}
	if err := m.Register("example.release", resolver); err != nil {
		t.Fatal(err)
	}
	inputs := inputset("example.release", map[string]any{"track": "stable", "credential": "private"})
	input := inputs[resource]["source"]
	input.Base = "software/App"
	inputs[resource]["source"] = input
	inputs["stemma/v1alpha1/BuildMacPkg/package"] = map[string]plugin.Input{"payload": inputs[resource]["source"]}
	first, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if discoveries != 1 || fetches != 1 {
		t.Fatal("identical named inputs repeated acquisition")
	}
	if entry(first).Content.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatal("manager did not establish the resolver output's content identity")
	}
	before := lockedBytes(t, m)
	if bytes.Contains(before, []byte("private")) || !bytes.Contains(before, []byte("9007199254740993")) || !bytes.Contains(before, []byte("observation:\n")) {
		t.Fatal("lock leaked credentials or lost opaque observation")
	}
	if got := string(entry(first).Evidence["vendor.release"]); got != `{"id":9007199254740993,"version":"1.2"}` {
		t.Fatalf("resolver evidence was not canonicalized: %s", got)
	}
	warm, err := prepare(t, m, inputs, Options{Offline: true})
	if err != nil || warm.Changed || !entry(warm).Equal(entry(first)) || discoveries != 1 || fetches != 1 {
		t.Fatalf("warm offline resolver evidence changed: %v", err)
	}
	artifact.Evidence = map[string]json.RawMessage{"vendor.release": json.RawMessage(`{"version":"2.0"}`)}
	object, _ := m.Store.Path(cas.Ref{SHA256: entry(first).Content.SHA256})
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	recovered, err := prepare(t, m, inputs, Options{})
	if err != nil || recovered.Changed || fetches != 2 || discoveries != 1 || !entry(recovered).Equal(entry(first)) {
		t.Fatalf("cold fetch rediscovered or changed locked resolver content: %v", err)
	}
	refreshed, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || !refreshed.Changed || entry(refreshed).Content != entry(first).Content || string(entry(refreshed).Evidence["vendor.release"]) != `{"version":"2.0"}` {
		t.Fatalf("evidence refresh lost content identity: %v", err)
	}
	before = lockedBytes(t, m)
	resolver.Version = "resolver-2"
	m = source.New(m.Store, m.Root, false)
	if err := m.Register("example.release", resolver); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, inputs, Options{}); err == nil {
		t.Fatal("frozen lock accepted a different resolver version")
	}
	if !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("resolver version failure changed lock")
	}
}

func TestInputRemovalAndSourceFreeProjects(t *testing.T) {
	m := manager(t)
	if err := os.WriteFile(filepath.Join(m.Root, "input"), []byte("input"), 0o644); err != nil {
		t.Fatal(err)
	}
	inputs := inputset("file", map[string]any{"path": "input"})
	if _, err := prepare(t, m, inputs, Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, nil, Options{}); err == nil {
		t.Fatal("frozen run removed reviewed input")
	}
	if result, err := prepare(t, m, nil, Options{Refresh: true}); err != nil || !result.Changed {
		t.Fatalf("removed input retained obsolete lock: %v", err)
	}
	if _, err := os.Stat(Path(m.Root)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty input lock persisted")
	}
	if _, err := prepare(t, m, nil, Options{Offline: true}); err != nil {
		t.Fatal(err)
	}
}

func TestSelectedInputsPreserveOtherReviewedResources(t *testing.T) {
	m := manager(t)
	for _, name := range []string{"first", "other"} {
		if err := os.WriteFile(filepath.Join(m.Root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inputs := inputset("file", map[string]any{"path": "first"})
	const other = "stemma/v1alpha1/BuildMacPkg/other"
	inputs[other] = map[string]plugin.Input{"payload": {Resolver: "file", Config: map[string]any{"path": "other"}}}
	first, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	delete(inputs, other)
	selected, err := prepare(t, m, inputs, Options{PreserveUnselected: true})
	if err != nil || selected.Changed || !selected.File.Inputs[other]["payload"].Equal(first.File.Inputs[other]["payload"]) {
		t.Fatalf("selected frozen run changed unrelated input: %v", err)
	}
	inputs[resource] = map[string]plugin.Input{}
	removed, err := prepare(t, m, inputs, Options{Refresh: true, PreserveUnselected: true})
	if err != nil || !removed.Changed || len(removed.File.Inputs) != 1 || !removed.File.Inputs[other]["payload"].Equal(first.File.Inputs[other]["payload"]) {
		t.Fatalf("selected input removal lost another reviewed resource: %v", err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

// TestFrozenRunsSayWhyAnEntryIsStale covers the ways a reviewed entry stops
// answering its declaration, including a document moved with its local file.
func TestFrozenRunsSayWhyAnEntryIsStale(t *testing.T) {
	m := manager(t)
	for _, dir := range []string{"software/first", "software/second"} {
		if err := os.MkdirAll(filepath.Join(m.Root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(m.Root, dir, "payload.pkg"), []byte("installer"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	local := func(base string) plugin.Input {
		return plugin.Input{Resolver: "file", Base: base, Config: map[string]any{"path": "payload.pkg"}}
	}
	m.Client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("installer")), Header: http.Header{}}, nil
	})
	remote := func(filename string) plugin.Input {
		return plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://vendor.example/App.pkg", "filename": filename}}
	}
	const other = "stemma/v1alpha1/Software/remote"
	locked := map[string]map[string]plugin.Input{resource: {"source": local("software/first")}, other: {"source": remote("App.pkg")}}
	if _, err := prepare(t, m, locked, Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		inputs map[string]map[string]plugin.Input
		want   string
	}{
		{"moved document", map[string]map[string]plugin.Input{resource: {"source": local("software/second")}, other: locked[other]}, "local input declaration changed since it was locked, as a new path or a moved document does; run stemma update Software/app"},
		{"changed declaration", map[string]map[string]plugin.Input{resource: locked[resource], other: {"source": remote("Renamed.pkg")}}, "input declaration changed since it was locked; run stemma update Software/remote"},
		{"renamed input", map[string]map[string]plugin.Input{resource: {"payload": local("software/first")}, other: locked[other]}, "input is missing from the lockfile; run stemma update Software/app"},
		{"added input", map[string]map[string]plugin.Input{resource: {"source": local("software/first"), "extra": local("software/second")}, other: locked[other]}, "declared inputs changed since they were locked; run stemma update Software/app"},
		{"new resource", map[string]map[string]plugin.Input{resource: locked[resource], other: locked[other], "stemma/v1alpha1/Software/new": {"source": local("software/second")}}, "inputs are missing from the lockfile; run stemma update Software/new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := prepare(t, m, tc.inputs, Options{}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestProjectLockReportsOnlyContention(t *testing.T) {
	root := t.TempDir()
	var logs bytes.Buffer
	ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
	unlock, err := Lock(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unlock() }()
	if logs.Len() != 0 {
		t.Fatalf("uncontended lock reported progress: %s", logs.String())
	}
	waiting, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := Lock(waiting, root); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended lock: %v", err)
	}
	if !strings.Contains(logs.String(), `"msg":"Waiting for project lock","stage":true`) {
		t.Fatalf("contended lock did not report waiting: %s", logs.String())
	}
}

func TestIncrementalAcquisitionCommitsOnlyCompleteUncancelledUpdates(t *testing.T) {
	m := manager(t)
	inputs := map[string]map[string]plugin.Input{}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(m.Root, name), []byte("original"), 0o644); err != nil {
			t.Fatal(err)
		}
		inputs["stemma/v1alpha1/Software/"+name] = map[string]plugin.Input{"source": {Resolver: "file", Config: map[string]any{"path": name}}}
	}
	if _, err := Prepare(t.Context(), m.Root, inputs, nil, m, Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	before := lockedBytes(t, m)
	if err := os.WriteFile(filepath.Join(m.Root, "a"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	update, err := Begin(t.Context(), m.Root, inputs, nil, m, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if changes := update.Changes("stemma/v1alpha1/Software/b"); len(changes) != 0 {
		t.Fatalf("unacquired input reported as changed: %+v", changes)
	}
	if _, _, err := update.Acquire(t.Context(), "stemma/v1alpha1/Software/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := update.Commit(t.Context()); err == nil || !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("incomplete acquisition replaced the reviewed lockfile")
	}
	if _, _, err := update.Acquire(t.Context(), "stemma/v1alpha1/Software/b"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := update.Commit(ctx); !errors.Is(err, context.Canceled) || !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("cancelled acquisition replaced the reviewed lockfile")
	}
	if _, err := update.Commit(t.Context()); err != nil || bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatalf("complete acquisition did not commit: %v", err)
	}
}

func TestRefreshConfirmsLockedContentWithoutDownloading(t *testing.T) {
	var bodies, conditionals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"stable"`)
		if r.Header.Get("If-None-Match") == `"stable"` {
			conditionals.Add(1)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		bodies.Add(1)
		_, _ = w.Write([]byte("installer"))
	}))
	t.Cleanup(server.Close)
	m := manager(t)
	inputs := inputset("http", map[string]any{"url": server.URL + "/app.pkg"})
	// A second resource declaring the same source shares the one observation.
	inputs["stemma/v1alpha1/Software/twin"] = map[string]plugin.Input{"source": {Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}}
	first, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || bodies.Load() != 1 {
		t.Fatalf("initial resolution: %v bodies=%d", err, bodies.Load())
	}
	before := lockedBytes(t, m)
	refreshed, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || refreshed.Changed || bodies.Load() != 1 || conditionals.Load() != 1 {
		t.Fatalf("refresh downloaded confirmed content: %v changed=%v bodies=%d conditionals=%d", err, refreshed.Changed, bodies.Load(), conditionals.Load())
	}
	twin := "stemma/v1alpha1/Software/twin"
	if first.CacheHits[resource]["source"] || !refreshed.CacheHits[resource]["source"] || !refreshed.CacheHits[twin]["source"] {
		t.Fatalf("cache hits do not follow what was downloaded: first=%v refreshed=%v", first.CacheHits, refreshed.CacheHits)
	}
	if !bytes.Equal(before, lockedBytes(t, m)) || !entry(refreshed).Equal(entry(first)) {
		t.Fatal("confirmed content rewrote the lock")
	}
}

func TestRefreshReusesPendingBytesWhicheverLockIsCheckedOut(t *testing.T) {
	var bodies atomic.Int32
	var release atomic.Value
	release.Store("one")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := release.Load().(string)
		w.Header().Set("ETag", `"`+current+`"`)
		if r.Header.Get("If-None-Match") == `"`+current+`"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		bodies.Add(1)
		_, _ = w.Write([]byte(current))
	}))
	t.Cleanup(server.Close)
	m := manager(t)
	inputs := inputset("http", map[string]any{"url": server.URL + "/app.pkg"})
	reviewed, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	release.Store("two")
	proposed, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || bodies.Load() != 2 {
		t.Fatalf("release resolution: %v bodies=%d", err, bodies.Load())
	}

	// The reviewed lock still records the first release while the proposal waits.
	save(t, m, reviewed.File)
	pending, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || bodies.Load() != 2 || !entry(pending).Equal(entry(proposed)) {
		t.Fatalf("pending bytes were downloaded again: %v bodies=%d %+v", err, bodies.Load(), entry(pending))
	}

	// A source back at the reviewed bytes leaves the reviewed entry as it was.
	release.Store("one")
	save(t, m, reviewed.File)
	reverted, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || reverted.Changed {
		t.Fatalf("reverted source rewrote the reviewed entry: %v changed=%v %+v", err, reverted.Changed, entry(reverted))
	}
}

func TestPreparationFetchesContentTheSourceIndexNamed(t *testing.T) {
	file := filepath.Join(t.TempDir(), "input.pkg")
	if err := os.WriteFile(file, []byte("installer"), 0o644); err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	m := manager(t)
	resolver := source.Resolver{
		Fingerprint: func(input plugin.Input) (string, error) {
			data, err := json.Marshal(input.Config)
			digest := sha256.Sum256(data)
			return hex.EncodeToString(digest[:]), err
		},
		Version: "1",
		Discover: func(context.Context, plugin.Input) (source.Discovery, error) {
			return source.Discovery{Observation: json.RawMessage(`{"release":"1.0"}`), Immutable: true}, nil
		},
		Acquire: func(context.Context, plugin.Input, json.RawMessage) (source.Acquisition, error) {
			fetches.Add(1)
			return source.Acquisition{Artifact: &plugin.Artifact{Path: file, Filename: "input.pkg"}}, nil
		},
	}
	if err := m.Register("example.release", resolver); err != nil {
		t.Fatal(err)
	}
	inputs := inputset("example.release", nil)
	first, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	object, err := m.Store.Path(cas.Ref{SHA256: entry(first).Content.SHA256})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	// The index still names the release, and update reads no bytes.
	if updated, err := prepare(t, m, inputs, Options{Refresh: true}); err != nil || updated.Changed || fetches.Load() != 1 {
		t.Fatalf("update fetched bytes it does not read: %v fetches=%d", err, fetches.Load())
	}
	if _, err := prepare(t, m, inputs, Options{IgnoreInputs: true}); err != nil || fetches.Load() != 2 || m.Store.VerifyDigest(t.Context(), entry(first).Content.SHA256) != nil {
		t.Fatalf("preparation was left without the bytes the index named: %v fetches=%d", err, fetches.Load())
	}
}

func TestRetainedResourcesKeepReviewedEntries(t *testing.T) {
	m := manager(t)
	for _, name := range []string{"first", "other", "gone"} {
		if err := os.WriteFile(filepath.Join(m.Root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inputs := inputset("file", map[string]any{"path": "first"})
	const other, gone = "stemma/v1alpha1/BuildMacPkg/other", "stemma/v1alpha1/BuildMacPkg/gone"
	for _, name := range []string{other, gone} {
		inputs[name] = map[string]plugin.Input{"payload": {Resolver: "file", Config: map[string]any{"path": path.Base(name)}}}
	}
	first, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	delete(inputs, other)
	delete(inputs, gone)
	frozen, err := prepare(t, m, inputs, Options{Retain: []string{other, gone}})
	if err != nil || frozen.Changed || len(frozen.File.Inputs) != 3 {
		t.Fatalf("retained resources changed a frozen lock: %v %+v", err, frozen.File.Inputs)
	}
	retained, err := prepare(t, m, inputs, Options{Refresh: true, Retain: []string{other}})
	if err != nil || !retained.Changed || len(retained.File.Inputs) != 2 || !retained.File.Inputs[other]["payload"].Equal(first.File.Inputs[other]["payload"]) {
		t.Fatalf("retention kept the wrong entries: %v %+v", err, retained.File.Inputs)
	}
}

func TestCommitRetainsRejectedResourcesWithoutPartialInputs(t *testing.T) {
	m := manager(t)
	const acquisition, preparation, healthy, added = "stemma/v1alpha1/Software/acquisition", "stemma/v1alpha1/Software/preparation", "stemma/v1alpha1/Software/healthy", "stemma/v1alpha1/Software/new"
	inputs := map[string]map[string]plugin.Input{}
	for _, resource := range []string{acquisition, preparation, healthy} {
		inputs[resource] = map[string]plugin.Input{}
		for _, name := range []string{"a", "b"} {
			filename := path.Base(resource) + "-" + name
			if err := os.WriteFile(filepath.Join(m.Root, filename), []byte("reviewed"), 0o644); err != nil {
				t.Fatal(err)
			}
			inputs[resource][name] = plugin.Input{Resolver: "file", Config: map[string]any{"path": filename}}
		}
	}
	before, err := Prepare(t.Context(), m.Root, inputs, nil, m, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	for resource := range inputs {
		if err := os.WriteFile(filepath.Join(m.Root, path.Base(resource)+"-a"), []byte("updated"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(filepath.Join(m.Root, "acquisition-b")); err != nil {
		t.Fatal(err)
	}
	// Rejected resources keep old inputs even when their declarations removed
	// them, and new failed resources do not gain partial lock entries.
	delete(inputs[preparation], "b")
	inputs[added] = inputs[acquisition]
	update, err := Begin(t.Context(), m.Root, inputs, nil, m, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{acquisition, added, preparation, healthy} {
		_, _, err := update.Acquire(t.Context(), resource)
		wantError := resource == acquisition || resource == added
		if (err != nil) != wantError {
			t.Fatalf("acquire %s: %v", resource, err)
		}
	}
	// Each acquired resource's changes are known before the commit, so a run
	// can report them as resources finish.
	acquired := update.Changes(healthy)
	if prepared := update.Changes(preparation); len(prepared) != 2 || prepared[0].Input != "a" || !prepared[0].ContentChanged || prepared[1].Input != "b" || prepared[1].After != nil {
		t.Fatalf("acquired resource changes: %+v", prepared)
	}
	result, err := update.Commit(t.Context(), acquisition, preparation, added)
	if !slices.EqualFunc(acquired, result.Changes, func(a, b InputChange) bool {
		return a.Resource == b.Resource && a.Input == b.Input && a.After.Equal(*b.After)
	}) {
		t.Fatalf("changes before the commit %+v differ from the commit %+v", acquired, result.Changes)
	}
	if err != nil || !result.Changed {
		t.Fatalf("successful resource could not update the lock: %+v, %v", result, err)
	}
	if len(result.Changes) != 1 || result.Changes[0].Resource != healthy || result.Changes[0].Input != "a" || result.Changes[0].Before == nil || result.Changes[0].After == nil {
		t.Fatalf("report included uncommitted inputs: %+v", result.Changes)
	}
	after, err := Load(m.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range []string{acquisition, preparation} {
		if !maps.EqualFunc(before.File.Inputs[resource], after.Inputs[resource], source.Entry.Equal) {
			t.Fatalf("rejected %s changed its reviewed entries: %+v", resource, after.Inputs[resource])
		}
	}
	if _, exists := after.Inputs[added]; exists || before.File.Inputs[healthy]["a"].Equal(after.Inputs[healthy]["a"]) {
		t.Fatalf("wrong resource inputs committed: %+v", after.Inputs)
	}
	if _, _, err := update.Acquire(t.Context(), acquisition); err == nil {
		t.Fatal("retaining reviewed entries turned failed acquisition into success")
	}
}

func TestDecodeChecksTheVersionBeforeAnyEntry(t *testing.T) {
	const document = "stemma.lock/Software/app.yaml"
	// Reject unsupported document versions before decoding their entries.
	older := "version: 2\ninputs:\n  source:\n    version: 1\n    resolved_at: \"2026-09-23T03:32:34Z\"\n"
	const unsupported = "lockfile: " + document + ": version 2 is not supported; delete it and run stemma update"
	for data, want := range map[string]string{
		older:                      unsupported,
		"version: 6\ninputs: {}\n": "lockfile: " + document + ": version 6 needs a newer stemma",
		"version: 5\n":             "lockfile: " + document + " locks named inputs and nothing else",
	} {
		if _, err := Decode(map[string][]byte{document: []byte(data)}); err == nil || err.Error() != want {
			t.Errorf("Decode(%q) = %v, want %q", data, err, want)
		}
	}
	for name, want := range map[string]string{
		"stemma.lock/app.yaml":                          "lockfile: stemma.lock/app.yaml is not a lock document",
		"stemma.lock/stemma/v1alpha1/Software/app.yaml": "lockfile: stemma.lock/stemma/v1alpha1/Software/app.yaml is not a lock document",
		"stemma.lock/Software/app.yml":                  "lockfile: stemma.lock/Software/app.yml is not a lock document",
		"Software/app.yaml":                             "lockfile: Software/app.yaml is not a lock document",
		"stemma.lock/v1/Software/app.yaml":              "lockfile: stemma.lock/v1/Software/app.yaml is not a lock document",
		"stemma.lock/../Software/app.yaml":              "lockfile: stemma.lock/../Software/app.yaml is not a lock document",
		"stemma.lock/Software//app.yaml":                "lockfile: stemma.lock/Software//app.yaml is not a lock document",
	} {
		if _, err := Decode(map[string][]byte{name: []byte("version: 5\n")}); err == nil || err.Error() != want {
			t.Errorf("Decode(%s) = %v, want %q", name, err, want)
		}
	}
	if file, err := Decode(nil); err != nil || file.Version != 0 {
		t.Fatalf("no documents = %+v: %v", file, err)
	}
	// A run names the document once too.
	m := manager(t)
	writeDocument(t, m, document, older)
	if _, err := prepare(t, m, map[string]map[string]plugin.Input{}, Options{}); err == nil || err.Error() != unsupported {
		t.Fatalf("run read the lock as %v", err)
	}
}

func TestDecodeReportsMissingObservation(t *testing.T) {
	const document = "stemma.lock/example.test/v1/Fixture/app.yaml"
	data := []byte(`version: 5
inputs:
  source:
    version: 1
    resolver: file
    resolver_version: "1"
    declaration: ` + strings.Repeat("a", 64) + `
    content:
      sha256: ` + strings.Repeat("b", 64) + `
      filename: input.pkg
      mode: 420
`)
	want := "lockfile: " + document + " input source: missing resolver observation"
	if _, err := Decode(map[string][]byte{document: data}); err == nil || err.Error() != want {
		t.Fatalf("missing observation: %v, want %q", err, want)
	}
	data = append(data, []byte("    observation: {}\n")...)
	file, err := Decode(map[string][]byte{document: data})
	if err != nil || len(file.Inputs["example.test/v1/Fixture/app"]) != 1 {
		t.Fatalf("explicit empty observation: %+v: %v", file, err)
	}
}

func TestCheckComparesTheLockfileShapeWithoutReadingValues(t *testing.T) {
	m := manager(t)
	if err := Check(m.Root, inputset("file", map[string]any{"path": "first"}), nil); err == nil || err.Error() != "lockfile: missing; run stemma update" {
		t.Fatalf("missing lockfile: %v", err)
	}
	for _, name := range []string{"first", "other", "paused"} {
		if err := os.WriteFile(filepath.Join(m.Root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inputs := inputset("file", map[string]any{"path": "first"})
	const other, paused = "stemma/v1alpha1/BuildMacPkg/other", "stemma/v1alpha1/BuildMacPkg/paused"
	inputs[other] = map[string]plugin.Input{"payload": {Resolver: "file", Config: map[string]any{"path": "other"}}}
	inputs[paused] = map[string]plugin.Input{"payload": {Resolver: "file", Config: map[string]any{"path": "paused"}}}
	if _, err := prepare(t, m, inputs, Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	delete(inputs, paused)
	// Values are left to the runs that acquire them.
	inputs[resource]["source"].Config["path"] = "{{ env.STEMMA_TEST_UNSET }}"
	if err := Check(m.Root, inputs, []string{paused}); err != nil {
		t.Fatalf("retained resource or declared values failed the check: %v", err)
	}
	inputs[other] = map[string]plugin.Input{"script": {Resolver: "file", Config: map[string]any{"path": "first"}}}
	err := Check(m.Root, inputs, nil)
	for _, want := range []string{
		"lockfile: stemma.lock/BuildMacPkg/paused.yaml locks a resource that is no longer declared; delete it or run stemma update",
		"lockfile: " + other + " input payload is no longer declared; run stemma update",
		"lockfile: " + other + " input script is missing; run stemma update",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("check did not report %q: %v", want, err)
		}
	}
}

// A renamed resource is a new one to the lock. Moving its document to the new
// name carries the reviewed entry along; otherwise an update of the new name
// locks it afresh and the old document stays until a full update or its
// deletion.
func TestRenamedResourceMovesOrReplacesItsDocument(t *testing.T) {
	m := manager(t)
	requests := 0
	m.Client.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("installer")), Header: http.Header{}}, nil
	})
	declared := plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://vendor.example/App.pkg"}}
	first, err := prepare(t, m, map[string]map[string]plugin.Input{resource: {"source": declared}}, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	document := func(name string) string {
		return filepath.Join(Path(m.Root), "Software", name+".yaml")
	}
	const renamed, again = "stemma/v1alpha1/Software/renamed", "stemma/v1alpha1/Software/again"
	inputs := map[string]map[string]plugin.Input{renamed: {"source": declared}}
	if _, err := prepare(t, m, inputs, Options{}); err == nil || !strings.Contains(err.Error(), "stemma.lock/Software/app.yaml locks a resource that is no longer declared; delete it or run stemma update") {
		t.Fatalf("renamed resource: %v", err)
	}
	if err := os.Rename(document("app"), document("renamed")); err != nil {
		t.Fatal(err)
	}
	fetched := requests
	moved, err := prepare(t, m, inputs, Options{})
	if err != nil || moved.Changed || requests != fetched || !moved.File.Inputs[renamed]["source"].Equal(entry(first)) {
		t.Fatalf("moved document: changed=%t requests=%d: %v", moved.Changed, requests-fetched, err)
	}

	inputs = map[string]map[string]plugin.Input{again: {"source": declared}}
	if _, err := prepare(t, m, inputs, Options{Refresh: true, PreserveUnselected: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, inputs, Options{}); err == nil || !strings.Contains(err.Error(), "stemma.lock/Software/renamed.yaml locks a resource that is no longer declared") {
		t.Fatalf("a selective update removed or accepted the old document: %v", err)
	}
	if _, err := prepare(t, m, inputs, Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(document("renamed")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a full update kept the old document: %v", err)
	}
	if result, err := prepare(t, m, inputs, Options{}); err != nil || result.Changed {
		t.Fatalf("after the full update: changed=%t: %v", result.Changed, err)
	}
	for _, renamed := range []string{"stemma/v1alpha1/Software/AGAIN", "stemma/v1alpha1/software/AGAIN"} {
		inputs = map[string]map[string]plugin.Input{renamed: {"source": declared}}
		if _, err := prepare(t, m, inputs, Options{Refresh: true}); err != nil {
			t.Fatal(err)
		}
		if loaded, err := Load(m.Root); err != nil || len(loaded.Inputs) != 1 || !loaded.Inputs[renamed]["source"].Equal(entry(first)) {
			t.Fatalf("case-only rename %s lost the lock or kept its old identity: %+v: %v", renamed, loaded, err)
		}
	}
}

// Each resource's inputs are locked in a document of its own, so an update
// rewrites only the documents of the resources it changed.
func TestEachResourceHasItsOwnLockDocument(t *testing.T) {
	m := manager(t)
	const other, plugged = "stemma/v1alpha1/Software/other", "example.test/v1/Fixture/app"
	inputs := map[string]map[string]plugin.Input{}
	for resource, name := range map[string]string{resource: "first", other: "other", plugged: "plugged"} {
		if err := os.WriteFile(filepath.Join(m.Root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		inputs[resource] = map[string]plugin.Input{"source": {Resolver: "file", Config: map[string]any{"path": name}}}
	}
	pinned := map[string]plugins.Entry{"provider": {Declaration: "local-plugin", Digest: "sha256:0"}}
	if _, err := Prepare(t.Context(), m.Root, inputs, pinned, m, Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]string {
		t.Helper()
		documents := map[string]string{}
		err := filepath.WalkDir(Path(m.Root), func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			data, err := os.ReadFile(name)
			relative, _ := filepath.Rel(m.Root, name)
			documents[filepath.ToSlash(relative)] = string(data)
			return err
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return documents
	}
	first := read()
	want := []string{"stemma.lock/Software/app.yaml", "stemma.lock/Software/other.yaml", "stemma.lock/example.test/v1/Fixture/app.yaml", "stemma.lock/plugins.yaml"}
	if got := slices.Sorted(maps.Keys(first)); !slices.Equal(got, want) || !strings.HasPrefix(first[want[0]], "version: 5\ninputs:\n  source:\n") || !strings.HasPrefix(first[want[3]], "version: 5\nplugins:\n  provider:\n") {
		t.Fatalf("lock documents = %v\n%s", got, first[want[0]])
	}
	if loaded, err := Load(m.Root); err != nil || len(loaded.Inputs) != 3 || len(loaded.Plugins) != 1 || loaded.Inputs[plugged]["source"].Content.Filename != "plugged" {
		t.Fatalf("loaded = %+v: %v", loaded, err)
	}
	t.Run("conflicting paths", func(t *testing.T) {
		loaded, err := Load(m.Root)
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct{ resource, document, reason string }{
			{"stemma/v1alpha1/Software/OTHER", "stemma.lock/Software/OTHER.yaml", "same case-insensitive path"},
			{"stemma/v1alpha1/software/third", "stemma.lock/software/third.yaml", "same case-insensitive path"},
			{"Software/app.yaml/Fixture/third", "stemma.lock/Software/app.yaml/Fixture/third.yaml", "both a document and a directory"},
		} {
			loaded.Inputs[tc.resource] = loaded.Inputs[other]
			if _, err := Encode(loaded); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("colliding resources would overwrite a lock: %v", err)
			}
			delete(loaded.Inputs, tc.resource)
			collision := map[string][]byte{want[0]: []byte(first[want[0]]), want[1]: []byte(first[want[1]]), tc.document: []byte(first[want[1]])}
			if _, err := Decode(collision); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("colliding lock documents were accepted: %v", err)
			}
		}
	})

	// A file the host leaves in the directory is not a lock document.
	stray := filepath.Join(Path(m.Root), "Software", ".DS_Store")
	if err := os.WriteFile(stray, []byte("finder"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Root, "other"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	delete(inputs, plugged)
	if _, err := Prepare(t.Context(), m.Root, inputs, pinned, m, Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	second := read()
	if second[want[0]] != first[want[0]] || second[want[3]] != first[want[3]] || second[want[1]] == first[want[1]] {
		t.Fatal("an update rewrote the documents of resources it did not change")
	}
	if _, kept := second[want[2]]; kept || len(second) != 4 {
		t.Fatalf("documents after a resource was removed = %v", slices.Sorted(maps.Keys(second)))
	}
	if _, err := os.Stat(filepath.Join(Path(m.Root), "example.test")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("an emptied directory stayed: %v", err)
	}
	if err := os.Remove(stray); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(t.Context(), m.Root, nil, nil, m, Options{Refresh: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Path(m.Root)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a lock that holds nothing left its directory: %v", err)
	}
}
