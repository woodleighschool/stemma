package lockfile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
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
	data, err := yaml.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Root, "stemma.lock.yaml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
func lockedBytes(t *testing.T, m *source.Manager) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(m.Root, "stemma.lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return data
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
	if _, err := prepare(t, m, inputs, Options{Frozen: true}); err == nil {
		t.Fatal("accepted missing frozen lock")
	}
	first, err := prepare(t, m, inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Changed || requests.Load() != 1 {
		t.Fatal("initial input did not resolve once")
	}
	original := entry(first)
	original.ResolvedAt = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	first.File.Inputs[resource]["source"] = original
	save(t, m, first.File)
	before := lockedBytes(t, m)
	if bytes.Contains(before, []byte("first-token")) {
		t.Fatal("lock retained a credential")
	}
	inputs["stemma/v1alpha1/Policy/source-free"] = map[string]plugin.Input{}
	warm, err := prepare(t, m, inputs, Options{Frozen: true, Offline: true})
	if err != nil || warm.Changed || !warm.CacheHits[resource]["source"] || requests.Load() != 1 {
		t.Fatalf("warm offline input changed: %v", err)
	}
	refreshed, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || refreshed.Changed || !entry(refreshed).ResolvedAt.Equal(original.ResolvedAt) {
		t.Fatalf("same bytes changed reviewed timestamp: %v", err)
	}
	object, err := m.Store.Path(original.Content.Artifact)
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
	recovered, err := prepare(t, m, inputs, Options{Frozen: true})
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
	if _, err := prepare(t, m, inputs, Options{Frozen: true}); err == nil {
		t.Fatal("cold recovery accepted substituted upstream bytes")
	}
	if !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("failed recovery rewrote lock")
	}
	updated, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || !updated.Changed || entry(updated).Content == original.Content || !entry(updated).ResolvedAt.After(original.ResolvedAt) {
		t.Fatalf("refresh failed to record changed input: %v", err)
	}
	if _, err := prepare(t, m, inputs, Options{Ignore: true, Frozen: true}); err == nil {
		t.Fatal("accepted conflicting lock options")
	}
}

func TestReleaseObservationChangesWithoutContentTimestampChurn(t *testing.T) {
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
	first, err := prepare(t, m, inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	previous := entry(first)
	previous.ResolvedAt = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	first.File.Inputs[resource]["source"] = previous
	save(t, m, first.File)
	release := "v1.2.4"
	currentRelease.Store(release)
	current, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if !current.Changed || entry(current).Content != previous.Content || !entry(current).ResolvedAt.Equal(previous.ResolvedAt) || !bytes.Contains(entry(current).Observation, []byte(release)) {
		t.Fatal("resolver observation changed content timestamp")
	}
	if _, err := prepare(t, m, inputs, Options{Frozen: true}); err != nil {
		t.Fatal(err)
	}
	inputs[resource]["source"].Config["asset"] = "App-*.pkg"
	if _, err := prepare(t, m, inputs, Options{Frozen: true}); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("frozen run accepted changed asset pattern: %v", err)
	}
	inputs[resource]["source"].Config["asset"] = "App*.pkg"
	updated, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Changed || entry(updated).Declaration == entry(current).Declaration || entry(updated).Content != entry(current).Content || !entry(updated).ResolvedAt.Equal(entry(current).ResolvedAt) {
		t.Fatal("pattern update lost content identity or failed to replace declaration")
	}
	if _, err := prepare(t, m, inputs, Options{Frozen: true}); err != nil {
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
	if err := os.WriteFile(file, []byte("script"), 0o640); err != nil {
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
	first, err := prepare(t, m, inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.File.Inputs[resource]) != 2 || first.File.Inputs[resource]["script"].Content.Mode != 0o640 || !first.File.Inputs[resource]["payload"].Content.Tree {
		t.Fatal("named input representation was lost")
	}
	script := first.File.Inputs[resource]["script"]
	script.ResolvedAt = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	first.File.Inputs[resource]["script"] = script
	save(t, m, first.File)
	before := lockedBytes(t, m)
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, inputs, Options{Frozen: true}); err == nil {
		t.Fatal("frozen input ignored file mode change")
	}
	if !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("failed mode check changed lock")
	}
	second, err := prepare(t, m, inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if second.File.Inputs[resource]["script"].Content.Artifact != script.Content.Artifact || second.File.Inputs[resource]["script"].Content.Mode != 0o755 || !second.File.Inputs[resource]["script"].ResolvedAt.Equal(script.ResolvedAt) {
		t.Fatal("mode-only change did not preserve byte identity")
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("two", link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, inputs, Options{Frozen: true}); err == nil {
		t.Fatal("warm cache hid symlink target change")
	}
	third, err := prepare(t, m, inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if third.File.Inputs[resource]["payload"].Content.Artifact == second.File.Inputs[resource]["payload"].Content.Artifact {
		t.Fatal("tree digest omitted symlink identity")
	}
	if err := os.Chmod(filepath.Join(tree, "one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, inputs, Options{Frozen: true, Offline: true}); err == nil {
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
	artifact := plugin.Artifact{Path: file, Filename: "input.bin"}
	observation := json.RawMessage(`{"revision":{"id":9007199254740993,"labels":["stable"]},"cursor":"opaque"}`)
	resolves, fetches := 0, 0
	resolver := source.Resolver{Version: "resolver-1", Fingerprint: func(input plugin.Input) (string, error) {
		safe := maps.Clone(input.Config)
		delete(safe, "credential")
		data, err := json.Marshal(safe)
		digest := sha256.Sum256(data)
		return hex.EncodeToString(digest[:]), err
	}, Resolve: func(_ context.Context, input plugin.Input) (source.Resolution, error) {
		if input.Base != "software/App" {
			t.Fatal("external resolver lost its resource-relative context")
		}
		resolves++
		return source.Resolution{Observation: observation, Artifact: artifact}, nil
	}, FetchLocked: func(_ context.Context, input plugin.Input, locked json.RawMessage) (plugin.Artifact, error) {
		if input.Base != "software/App" {
			t.Fatal("external locked fetch lost its resource-relative context")
		}
		fetches++
		if !bytes.Contains(locked, []byte("9007199254740993")) {
			t.Fatal("opaque observation lost integer precision")
		}
		return artifact, nil
	}}
	m.Resolvers["example.release"] = resolver
	inputs := inputset("example.release", map[string]any{"track": "stable", "credential": "private"})
	input := inputs[resource]["source"]
	input.Base = "software/App"
	inputs[resource]["source"] = input
	inputs["stemma/v1alpha1/BuildMacPkg/package"] = map[string]plugin.Input{"payload": inputs[resource]["source"]}
	first, err := prepare(t, m, inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if resolves != 1 {
		t.Fatal("identical named inputs repeated discovery")
	}
	if entry(first).Content.Artifact.SHA256 != hex.EncodeToString(digest[:]) || entry(first).Content.Artifact.Size != int64(len(data)) {
		t.Fatal("manager did not establish the resolver output's content identity")
	}
	before := lockedBytes(t, m)
	if bytes.Contains(before, []byte("private")) || !bytes.Contains(before, []byte("9007199254740993")) || !bytes.Contains(before, []byte("observation:\n")) {
		t.Fatal("lock leaked credentials or lost opaque observation")
	}
	object, _ := m.Store.Path(entry(first).Content.Artifact)
	if err := os.Remove(object); err != nil {
		t.Fatal(err)
	}
	recovered, err := prepare(t, m, inputs, Options{Frozen: true})
	if err != nil || recovered.Changed || fetches != 1 || resolves != 1 {
		t.Fatalf("cold fetch rediscovered or changed locked resolver content: %v", err)
	}
	resolver.Version = "resolver-2"
	m.Resolvers["example.release"] = resolver
	if _, err := prepare(t, m, inputs, Options{Frozen: true}); err == nil {
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
	if _, err := prepare(t, m, inputs, Options{}); err != nil {
		t.Fatal(err)
	}
	if _, err := prepare(t, m, nil, Options{Frozen: true}); err == nil {
		t.Fatal("frozen run removed reviewed input")
	}
	if result, err := prepare(t, m, nil, Options{}); err != nil || !result.Changed {
		t.Fatalf("removed input retained obsolete lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Root, "stemma.lock.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty input lock persisted")
	}
	if _, err := prepare(t, m, nil, Options{Frozen: true, Offline: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := Prepare(t.Context(), m.Root, nil, map[string]plugins.Entry{"fixture": {Image: "registry.example/plugin:v1"}}, m, Options{Frozen: true}); err == nil {
		t.Fatal("source-free project bypassed required plugin lock")
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
	first, err := prepare(t, m, inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	delete(inputs, other)
	selected, err := prepare(t, m, inputs, Options{Frozen: true, PreserveUnselected: true})
	if err != nil || selected.Changed || !selected.File.Inputs[other]["payload"].Equal(first.File.Inputs[other]["payload"]) {
		t.Fatalf("selected frozen run changed unrelated input: %v", err)
	}
	inputs[resource] = map[string]plugin.Input{}
	removed, err := prepare(t, m, inputs, Options{PreserveUnselected: true})
	if err != nil || !removed.Changed || len(removed.File.Inputs) != 1 || !removed.File.Inputs[other]["payload"].Equal(first.File.Inputs[other]["payload"]) {
		t.Fatalf("selected input removal lost another reviewed resource: %v", err)
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
		inputs[name] = map[string]plugin.Input{"source": {Resolver: "file", Config: map[string]any{"path": name}}}
	}
	if _, err := Prepare(t.Context(), m.Root, inputs, nil, m, Options{}); err != nil {
		t.Fatal(err)
	}
	before := lockedBytes(t, m)
	if err := os.WriteFile(filepath.Join(m.Root, "a"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	update, err := Begin(t.Context(), m.Root, inputs, nil, m, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := update.Acquire(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := update.Commit(t.Context()); err == nil || !bytes.Equal(before, lockedBytes(t, m)) {
		t.Fatal("incomplete acquisition replaced the reviewed lockfile")
	}
	if _, _, err := update.Acquire(t.Context(), "b"); err != nil {
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
	original := entry(first)
	original.ResolvedAt = time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	first.File.Inputs[resource]["source"] = original
	save(t, m, first.File)
	before := lockedBytes(t, m)
	refreshed, err := prepare(t, m, inputs, Options{Refresh: true})
	if err != nil || refreshed.Changed || bodies.Load() != 1 || conditionals.Load() != 1 {
		t.Fatalf("refresh downloaded confirmed content: %v changed=%v bodies=%d conditionals=%d", err, refreshed.Changed, bodies.Load(), conditionals.Load())
	}
	if !bytes.Equal(before, lockedBytes(t, m)) || !entry(refreshed).ResolvedAt.Equal(original.ResolvedAt) {
		t.Fatal("confirmed content rewrote the lock")
	}
}

func TestHintsConfirmPendingBytesWithoutDownloading(t *testing.T) {
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
	hinted, err := prepare(t, m, inputs, Options{Refresh: true, Hints: proposed.File.Inputs})
	if err != nil || bodies.Load() != 2 || !entry(hinted).Equal(entry(proposed)) || !entry(hinted).ResolvedAt.Equal(entry(proposed).ResolvedAt) {
		t.Fatalf("pending bytes were downloaded again: %v bodies=%d %+v", err, bodies.Load(), entry(hinted))
	}

	// A source back at the reviewed bytes keeps the reviewed timestamp.
	release.Store("one")
	save(t, m, reviewed.File)
	reverted, err := prepare(t, m, inputs, Options{Refresh: true, Hints: proposed.File.Inputs})
	if err != nil || reverted.Changed || !entry(reverted).ResolvedAt.Equal(entry(reviewed).ResolvedAt) {
		t.Fatalf("reverted source rewrote the reviewed entry: %v changed=%v %+v", err, reverted.Changed, entry(reverted))
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
	first, err := prepare(t, m, inputs, Options{})
	if err != nil {
		t.Fatal(err)
	}
	delete(inputs, other)
	delete(inputs, gone)
	frozen, err := prepare(t, m, inputs, Options{Frozen: true, Retain: []string{other, gone}})
	if err != nil || frozen.Changed || len(frozen.File.Inputs) != 3 {
		t.Fatalf("retained resources changed a frozen lock: %v %+v", err, frozen.File.Inputs)
	}
	retained, err := prepare(t, m, inputs, Options{Retain: []string{other}})
	if err != nil || !retained.Changed || len(retained.File.Inputs) != 2 || !retained.File.Inputs[other]["payload"].Equal(first.File.Inputs[other]["payload"]) {
		t.Fatalf("retention kept the wrong entries: %v %+v", err, retained.File.Inputs)
	}
}
