package reconcile

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-billy/v6/osfs"
	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/backend"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/transport"
	githttp "github.com/go-git/go-git/v6/plumbing/transport/http"
	"github.com/golang-jwt/jwt/v5"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/source"
)

// botEmail is the identity GitHub attributes the fixture App's commits to.
const botEmail = "424242+stemma-fixture[bot]@users.noreply.github.com"

// project declares a GitHub App through the environment and publishes into
// an absolute Munki repository, so worktrees can come and go around it.
func project(munki string) string {
	return `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: catalog
spec:
  reconcile:
    source_control:
      type: github
      config:
        client_id: "{{ env.GITHUB_APP_CLIENT_ID }}"
        installation_id: "{{ env.GITHUB_APP_INSTALLATION_ID }}"
        private_key: "{{ env.GITHUB_APP_PRIVATE_KEY }}"
  imports:
    - '*.software.yaml'
  destinations:
    repo:
      operation: munki
      config:
        path: ` + munki + `
`
}

func software(url string) string {
	return `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: fixture
spec:
  source:
    url: ` + url + `/fixture.pkg
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
`
}

// policy keeps the import pattern matching once the fixture document is removed.
const policy = `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: policy
spec:
  destinations:
    repo:
      pkginfo:
        installer_type: nopkg
        version: '1'
        installcheck_script: |
          #!/bin/sh
          exit 1
`

func human() *object.Signature {
	return &object.Signature{Name: "Human", Email: "human@example.com", When: time.Now()}
}

// origin is a bare repository plus a working clone a person commits through.
type origin struct {
	// root is the directory the fake host serves; the bare repository is
	// example/catalog.git inside it.
	root string
	work *gogit.Repository
	dir  string
}

func newOrigin(t *testing.T, files map[string]string) origin {
	t.Helper()
	o := origin{root: t.TempDir(), dir: filepath.Join(t.TempDir(), "work")}
	bare, err := gogit.PlainInit(o.bareDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := bare.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main")); err != nil {
		t.Fatal(err)
	}
	if o.work, err = gogit.PlainInit(o.dir, false); err != nil {
		t.Fatal(err)
	}
	if err := o.work.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, "refs/heads/main")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.work.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{o.bareDir()}}); err != nil {
		t.Fatal(err)
	}
	o.commit(t, "initial catalog", files)
	return o
}

func (o origin) bareDir() string { return filepath.Join(o.root, "example", "catalog.git") }

// bare opens the bare repository afresh, so packs the host received since
// are visible.
func (o origin) bare(t *testing.T) *gogit.Repository {
	t.Helper()
	repo, err := gogit.PlainOpen(o.bareDir())
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

// tip returns a branch's commit, or "" when the branch does not exist.
func (o origin) tip(branch string) string {
	repo, err := gogit.PlainOpen(o.bareDir())
	if err != nil {
		return ""
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return ""
	}
	return ref.Hash().String()
}

// pull brings the working clone's main up to date with the bare repository.
func (o origin) pull(t *testing.T) {
	t.Helper()
	err := o.work.FetchContext(t.Context(), &gogit.FetchOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{"+refs/heads/*:refs/remotes/origin/*"}})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) || errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	tree, err := o.work.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.Checkout(&gogit.CheckoutOptions{Branch: "refs/heads/main", Force: true}); err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		t.Fatal(err)
	}
	if err := tree.Reset(&gogit.ResetOptions{Commit: plumbing.NewHash(o.tip("main")), Mode: gogit.HardReset}); err != nil {
		t.Fatal(err)
	}
}

// stage writes files (an empty value deletes) in the working clone and
// commits them as a person.
func (o origin) stage(t *testing.T, message string, files map[string]string) plumbing.Hash {
	t.Helper()
	tree, err := o.work.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(o.dir, name)
		if content == "" {
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
		} else if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := tree.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := tree.Commit(message, &gogit.CommitOptions{Author: human(), Committer: human()})
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

func (o origin) push(t *testing.T, refspec string) {
	t.Helper()
	err := o.work.PushContext(t.Context(), &gogit.PushOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{config.RefSpec(refspec)}})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		t.Fatal(err)
	}
}

// commit records files on main and pushes.
func (o origin) commit(t *testing.T, message string, files map[string]string) {
	t.Helper()
	o.pull(t)
	o.stage(t, message, files)
	o.push(t, "refs/heads/main:refs/heads/main")
}

// commitBranch adds a person's commit on top of a proposal branch.
func (o origin) commitBranch(t *testing.T, branch string, files map[string]string) {
	t.Helper()
	tree, err := o.work.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := o.work.FetchContext(t.Context(), &gogit.FetchOptions{RemoteName: "origin", RefSpecs: []config.RefSpec{config.RefSpec("+refs/heads/" + branch + ":refs/remotes/origin/" + branch)}}); err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		t.Fatal(err)
	}
	if err := tree.Checkout(&gogit.CheckoutOptions{Hash: plumbing.NewHash(o.tip(branch)), Force: true}); err != nil {
		t.Fatal(err)
	}
	commit := o.stage(t, "review notes", files)
	o.push(t, commit.String()+":refs/heads/"+branch)
	if err := tree.Checkout(&gogit.CheckoutOptions{Branch: "refs/heads/main", Force: true}); err != nil {
		t.Fatal(err)
	}
}

// commitObject reads a commit from the bare repository.
func (o origin) commitObject(t *testing.T, sha string) *object.Commit {
	t.Helper()
	commit, err := o.bare(t).CommitObject(plumbing.NewHash(sha))
	if err != nil {
		t.Fatal(err)
	}
	return commit
}

// fakeGitHub serves the repository over smart HTTP behind the App's
// installation token, mints that token for the App's assertion and keeps pull
// requests and statuses in memory.
type fakeGitHub struct {
	*httptest.Server

	mu       sync.Mutex
	origin   origin
	backend  http.Handler
	pulls    []*pullRequest
	statuses map[string][]map[string]string
	tokens   atomic.Int32
}

type pullRequest struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	State   string `json:"state"`
	HTMLURL string `json:"html_url"`
	// MergedAt is set when a person merged the request, however they merged it.
	MergedAt *string `json:"merged_at"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

func newFakeGitHub(t *testing.T, o origin) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{origin: o, statuses: map[string][]map[string]string{}}
	f.backend = backend.New(transport.NewFilesystemLoader(osfs.New(o.root), false))
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

// remote is the origin URL the reconciled checkout uses.
func (f *fakeGitHub) remote() string { return f.URL + "/example/catalog.git" }

// auth presents the installation token the way the reconciler does.
func (f *fakeGitHub) auth() client.Option {
	return client.WithHTTPAuth(&githttp.BasicAuth{Username: "x-access-token", Password: "minted"})
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	if rest, ok := strings.CutPrefix(r.URL.Path, "/api/v3"); ok {
		f.api(w, r, rest)
		return
	}
	if user, password, ok := r.BasicAuth(); !ok || user != "x-access-token" || password != "minted" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.backend.ServeHTTP(w, r)
}

// api answers the App's JWT-authenticated calls, then everything an
// installation token may do.
func (f *fakeGitHub) api(w http.ResponseWriter, r *http.Request, path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if path == "/app" || path == "/app/installations/7/access_tokens" {
		var claims jwt.RegisteredClaims
		if _, _, err := jwt.NewParser().ParseUnverified(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), &claims); err != nil || claims.Issuer != "Iv1.fixture" {
			http.Error(w, "the App assertion must be issued by the client ID", http.StatusUnauthorized)
			return
		}
		switch path {
		case "/app":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "slug": "stemma-fixture"})
		default:
			f.tokens.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "minted", "expires_at": time.Now().Add(time.Hour)})
		}
		return
	}
	if r.Header.Get("Authorization") != "Bearer minted" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if path == "/users/stemma-fixture[bot]" {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 424242, "login": "stemma-fixture[bot]"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	rest, ok := strings.CutPrefix(path, "/repos/example/catalog/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && rest == "pulls":
		matched := []*pullRequest{}
		for _, pull := range f.pulls {
			if pull.State != r.URL.Query().Get("state") {
				continue
			}
			if head := r.URL.Query().Get("head"); head != "" && head != "example:"+pull.Head.Ref {
				continue
			}
			matched = append(matched, pull)
		}
		_ = json.NewEncoder(w).Encode(matched)
	case r.Method == http.MethodPost && rest == "pulls":
		pull := &pullRequest{Number: len(f.pulls) + 1, Title: body["title"].(string), Body: body["body"].(string), State: "open"}
		pull.Head.Ref = body["head"].(string)
		pull.Head.SHA = f.origin.tip(pull.Head.Ref)
		pull.HTMLURL = "https://github.example/pull/" + strconv.Itoa(pull.Number)
		f.pulls = append(f.pulls, pull)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(pull)
	case r.Method == http.MethodPatch && strings.HasPrefix(rest, "pulls/"):
		number, _ := strconv.Atoi(strings.TrimPrefix(rest, "pulls/"))
		pull := f.pulls[number-1]
		if title, ok := body["title"].(string); ok {
			pull.Title, pull.Body = title, body["body"].(string)
		}
		if state, ok := body["state"].(string); ok {
			pull.State = state
		}
		pull.Head.SHA = f.origin.tip(pull.Head.Ref)
		_ = json.NewEncoder(w).Encode(pull)
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "statuses/"):
		sha := strings.TrimPrefix(rest, "statuses/")
		f.statuses[sha] = append(f.statuses[sha], map[string]string{"context": body["context"].(string), "state": body["state"].(string), "description": body["description"].(string)})
		w.WriteHeader(http.StatusCreated)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "commits/"):
		sha := strings.TrimSuffix(strings.TrimPrefix(rest, "commits/"), "/status")
		latest := map[string]map[string]string{}
		for _, status := range f.statuses[sha] {
			latest[status["context"]] = status
		}
		statuses := []map[string]string{}
		for _, status := range latest {
			statuses = append(statuses, status)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"statuses": statuses})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) open() []*pullRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var open []*pullRequest
	for _, pull := range f.pulls {
		if pull.State == "open" {
			open = append(open, pull)
		}
	}
	return open
}

// squash merges an open proposal the way the host's squash button does: main
// gains one commit with the proposal's files while the branch tip stays outside
// that history, and the request records the merge.
func (f *fakeGitHub) squash(t *testing.T, number int) {
	t.Helper()
	f.mu.Lock()
	pull := f.pulls[number-1]
	f.mu.Unlock()
	file, err := f.origin.commitObject(t, f.origin.tip(pull.Head.Ref)).File("stemma.lock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	f.origin.commit(t, pull.Title, map[string]string{"stemma.lock.yaml": contents})
	f.mu.Lock()
	defer f.mu.Unlock()
	merged := time.Now().UTC().Format(time.RFC3339)
	pull.State, pull.MergedAt = "closed", &merged
}

func (f *fakeGitHub) closeAll() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pull := range f.pulls {
		if pull.State == "open" {
			pull.State = "closed"
		}
	}
}

func (f *fakeGitHub) status(sha, name string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found map[string]string
	for _, status := range f.statuses[sha] {
		if status["context"] == name {
			found = status
		}
	}
	return found
}

// buildPackage writes a small payload package with the given version.
func buildPackage(t *testing.T, version string) []byte {
	t.Helper()
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload", "fixture-"+version+".txt"), []byte(version), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "fixture.pkg")
	if err := pkgbuild.Build(t.Context(), source, output, pkgbuild.Options{Identifier: "edu.example.fixture", Version: version, Payload: "payload", InstallLocation: "/Library/Example", Timestamp: time.Unix(0, 0).UTC()}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
}

func TestRunProposesAppliesAndRetires(t *testing.T) {
	var payload atomic.Value
	var downloads atomic.Int32
	payload.Store(buildPackage(t, "1.0"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data := payload.Load().([]byte)
		etag := fmt.Sprintf("\"%x\"", sha256.Sum256(data))
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		downloads.Add(1)
		_, _ = w.Write(data)
	}))
	t.Cleanup(server.Close)
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testKey(t))
	munki := filepath.Join(t.TempDir(), "munki")
	o := newOrigin(t, map[string]string{"stemma.yaml": project(munki), "fixture.software.yaml": software(server.URL), "policy.software.yaml": policy})
	gh := newFakeGitHub(t, o)

	// The reconciled checkout is an ordinary clone whose origin is the host.
	checkout := filepath.Join(t.TempDir(), "checkout")
	cloned, err := gogit.PlainCloneContext(t.Context(), checkout, &gogit.CloneOptions{URL: gh.remote(), ClientOptions: []client.Option{gh.auth()}})
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: filepath.Join(checkout, "stemma.yaml"), CacheDir: t.TempDir(), StateDir: t.TempDir()}
	const branch = "stemma/MacSoftware/fixture"
	run := func(wantErr string) Report {
		t.Helper()
		report, err := Run(t.Context(), opts)
		if wantErr == "" && err != nil {
			t.Fatalf("run failed: %v\n%+v", err, report)
		}
		if wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
			t.Fatalf("run error %v, want %q\n%+v", err, wantErr, report)
		}
		return report
	}
	head := o.tip("main")

	// An unlocked catalog cannot be applied, but the missing lock is proposed.
	first := run("lockfile")
	if first.Branch != "main" || first.Head != head || first.Apply == nil || first.Apply.Error == "" || gh.status(head, applyContext)["state"] != "failure" {
		t.Fatalf("apply failure not reported: %+v", first)
	}
	if len(first.Updates) != 1 || first.Updates[0].Action != "created" || first.Updates[0].Branch != branch {
		t.Fatalf("proposal: %+v", first.Updates)
	}
	if gh.tokens.Load() != 1 {
		t.Fatalf("minted %d installation tokens in one run", gh.tokens.Load())
	}
	tip := o.tip(branch)
	if tip == "" {
		t.Fatal("proposal branch missing")
	}
	proposal := o.commitObject(t, tip)
	if len(proposal.ParentHashes) != 1 || proposal.ParentHashes[0].String() != head {
		t.Fatalf("proposal is not one commit on the reviewed branch: %v", proposal.ParentHashes)
	}
	if proposal.Author.Name != "stemma-fixture[bot]" || proposal.Author.Email != botEmail || proposal.Committer.Email != botEmail || !strings.Contains(proposal.Message, trailer) {
		t.Fatalf("proposal commit identity: %s", proposal)
	}
	file, err := proposal.File("stemma.lock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := lockfile.Parse([]byte(contents))
	if err != nil || locked.Inputs["stemma/v1alpha1/MacSoftware/fixture"]["source"].Content.Filename != "fixture.pkg" {
		t.Fatalf("proposed lock: %v %+v", err, locked)
	}
	pulls := gh.open()
	if len(pulls) != 1 || pulls[0].Head.Ref != branch || pulls[0].Title != "Update MacSoftware/fixture to 1.0" || !strings.Contains(pulls[0].Body, "| source |") || !strings.Contains(pulls[0].Body, "repo: ") {
		t.Fatalf("pull request: %+v", pulls)
	}
	if status := gh.status(tip, planContext); status["state"] != "success" || !strings.Contains(status["description"], "MacSoftware/fixture 1.0") {
		t.Fatalf("plan status: %v", status)
	}
	if _, err := os.Stat(filepath.Join(opts.StateDir, "reconcile.json")); !os.IsNotExist(err) {
		t.Fatal("failed apply moved the marker")
	}
	assertUndisturbed(t, cloned, checkout, head)

	// Nothing changed: the proposal stays exactly as pushed.
	second := run("lockfile")
	if second.Updates[0].Action != "unchanged" || o.tip(branch) != tip || len(gh.open()) != 1 {
		t.Fatalf("idempotent run rewrote the proposal: %+v", second.Updates)
	}

	// A squash merge applies offline from the warmed cache and retires the
	// branch, although its commit never joined the reviewed history.
	gh.squash(t, pulls[0].Number)
	merged := o.tip("main")
	third := run("")
	if third.Apply.Error != "" || third.Apply.Skipped || gh.status(merged, applyContext)["state"] != "success" {
		t.Fatalf("merged lock was not applied: %+v", third.Apply)
	}
	if len(third.Updates) != 1 || third.Updates[0].Action != "retired" || third.Updates[0].Summary != "merged into the reviewed branch" || o.tip(branch) != "" || len(gh.open()) != 0 {
		t.Fatalf("merged proposal not retired: %+v", third.Updates)
	}
	if _, err := os.Stat(filepath.Join(munki, "pkgsinfo")); err != nil {
		t.Fatal("apply did not publish into the repository destination")
	}
	fourth := run("")
	if !fourth.Apply.Skipped || len(fourth.Updates) != 0 {
		t.Fatalf("applied commit was reconciled again: %+v", fourth)
	}

	// A new upstream release becomes a fresh proposal with its version.
	payload.Store(buildPackage(t, "2.0"))
	fifth := run("")
	if len(fifth.Updates) != 1 || fifth.Updates[0].Action != "created" || gh.open()[0].Title != "Update MacSoftware/fixture to 2.0" {
		t.Fatalf("release proposal: %+v %+v", fifth.Updates, gh.open())
	}
	if pull := gh.open()[0]; !strings.Contains(pull.Body, "`fixture.pkg`") || !strings.Contains(pull.Body, "prepares `fixture-2.0.pkg` (2.0)") {
		t.Fatalf("release body: %s", pull.Body)
	}

	// While the release awaits review, its proposal's observation confirms
	// the bytes conditionally instead of downloading them again.
	served := downloads.Load()
	if again := run(""); again.Updates[0].Action != "unchanged" || downloads.Load() != served {
		t.Fatalf("pending release downloaded again: %+v downloads=%d", again.Updates, downloads.Load()-served)
	}

	// A person's commit on the branch ends the reconciler's ownership.
	o.commitBranch(t, branch, map[string]string{"NOTES.md": "reviewing"})
	reviewed := o.tip(branch)
	sixth := run("")
	if sixth.Updates[0].Action != "skipped" || o.tip(branch) != reviewed || len(gh.open()) != 1 {
		t.Fatalf("touched branch was changed: %+v", sixth.Updates)
	}
	if err := o.bare(t).Storer.RemoveReference(plumbing.NewBranchReferenceName(branch)); err != nil {
		t.Fatal(err)
	}
	gh.closeAll()

	// Removing a document leaves stale entries: apply fails and cleanup is proposed.
	o.commit(t, "retire fixture", map[string]string{"fixture.software.yaml": ""})
	seventh := run("stale")
	if seventh.Apply.Error == "" || len(seventh.Updates) != 1 || seventh.Updates[0].Action != "created" {
		t.Fatalf("cleanup proposal: %+v", seventh)
	}
	cleanup := o.tip(branch)
	if _, err := o.commitObject(t, cleanup).File("stemma.lock.yaml"); !errors.Is(err, object.ErrFileNotFound) || gh.open()[0].Title != "Remove MacSoftware/fixture from the lockfile" {
		t.Fatalf("cleanup branch: %v %+v", err, gh.open())
	}
	if status := gh.status(cleanup, planContext); status["state"] != "success" || status["description"] != "lock maintenance: removed MacSoftware/fixture" {
		t.Fatalf("cleanup status: %v", status)
	}

	// Closing the proposal without merging declines it until it changes.
	gh.closeAll()
	eighth := run("stale")
	if eighth.Updates[0].Action != "declined" || len(gh.open()) != 0 || o.tip(branch) != cleanup {
		t.Fatalf("declined proposal was reopened: %+v", eighth.Updates)
	}

	// An unrelated change to the reviewed branch does not revive it.
	o.commit(t, "unrelated change", map[string]string{"README.md": "catalog\n"})
	ninth := run("stale")
	if ninth.Updates[0].Action != "declined" || len(gh.open()) != 0 || o.tip(branch) != cleanup {
		t.Fatalf("declined proposal was revived by a base move: %+v", ninth.Updates)
	}
	assertUndisturbed(t, cloned, checkout, head)
}

// assertUndisturbed proves runs leave the checkout as the scheduler made it.
func assertUndisturbed(t *testing.T, repo *gogit.Repository, dir, head string) {
	t.Helper()
	current, err := repo.Head()
	if err != nil || current.Name() != "refs/heads/main" || current.Hash().String() != head {
		t.Fatalf("checkout HEAD moved to %v: %v", current, err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if status, err := tree.Status(); err != nil || !status.IsClean() {
		t.Fatalf("the checkout was disturbed: %v\n%s", err, status)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "worktrees")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("worktrees were registered in the checkout")
	}
}

func TestRunRequiresSourceControl(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stemma.yaml"), []byte(strings.Replace(project("/munki"), "github", "forge", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "key")
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/example/catalog.git"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), Options{ConfigPath: filepath.Join(root, "stemma.yaml")}); err == nil || !strings.Contains(err.Error(), `unsupported source_control type "forge"`) {
		t.Fatalf("unknown provider accepted: %v", err)
	}
}

// TestRenderDistinguishesRefreshes covers a proposal that resolved the locked
// bytes again with new metadata, and the rendering of absent observations.
func TestRenderDistinguishesRefreshes(t *testing.T) {
	artifact := cas.Ref{SHA256: "8ccdfd126c8e80411108c8ec45cbc610779a08c54285d02d48681e8d8afed529", Size: 3}
	before := map[string]source.Entry{"source": {Version: 1, Observation: []byte(`{"url":"https://example.test/app.pkg"}`), Content: source.Content{Artifact: artifact, Filename: "app.pkg"}}}
	after := map[string]source.Entry{"source": {Version: 1, Observation: []byte(`{"url":"https://example.test/app.pkg","etag":"\"x\""}`), Content: source.Content{Artifact: artifact, Filename: "app-2.pkg"}}}
	if !sameArtifacts(before, after) || sameArtifacts(before, nil) || sameArtifacts(before, map[string]source.Entry{"source": {Version: 1}}) {
		t.Fatal("artifact comparison")
	}
	refresh := change{kind: "MacSoftware", name: "app", entries: after, refresh: true}
	if got := title(refresh, verification{version: "2"}); got != "Refresh MacSoftware/app lock metadata" {
		t.Fatalf("title %q", got)
	}
	if got := commitMessage(refresh); got != "chore(stemma): refresh MacSoftware/app lock metadata" {
		t.Fatalf("commit message %q", got)
	}
	text := body(refresh, before, after, verification{})
	for _, want := range []string{"resolved the bytes already locked", "`source` observation: etag — → `\"x\"`"} {
		if !strings.Contains(text, want) {
			t.Fatalf("body lacks %q:\n%s", want, text)
		}
	}
	if got := title(change{kind: "MacSoftware", name: "app", entries: after}, verification{version: "2"}); got != "Update MacSoftware/app to 2" {
		t.Fatalf("update title %q", got)
	}
}

func TestDiffAndBranchNames(t *testing.T) {
	if name := branchName("MacSoftware", "Google Chrome (beta).lock"); name != "stemma/MacSoftware/Google-Chrome-beta" {
		t.Fatalf("branch name %q", name)
	}
	if kind, name := splitKey("stemma/v1alpha1/MacSoftware/chrome"); kind != "MacSoftware" || name != "chrome" {
		t.Fatalf("split %s %s", kind, name)
	}
	if !equalEntries(nil, nil) || equalEntries(nil, map[string]source.Entry{"source": {Version: 1}}) {
		t.Fatal("entry comparison")
	}
}

// suspended is software whose package the repository does not carry.
const suspended = `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: private
suspend: true
spec:
  source:
    path: private.pkg
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
`

func TestDiffLeavesSuspendedResourcesAlone(t *testing.T) {
	const key = "stemma/v1alpha1/MacSoftware/private"
	entries := map[string]source.Entry{"source": {Version: 1}}
	candidate := engine.Candidate{Lock: lockfile.File{Version: 2, Inputs: map[string]map[string]source.Entry{key: entries}}, Resources: map[string]engine.CandidateResource{key: {Name: "private", Kind: "MacSoftware", Suspended: true}}}
	if changes := diff(candidate); len(changes) != 0 {
		t.Fatalf("suspended resource was proposed: %+v", changes)
	}
}

func TestRunLeavesSuspendedResourcesAlone(t *testing.T) {
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testKey(t))
	munki := filepath.Join(t.TempDir(), "munki")
	files := map[string]string{"stemma.yaml": project(munki), "policy.software.yaml": policy, "private.software.yaml": suspended}
	// The private package is locked where it exists; the repository carries
	// the lock without the package.
	local := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(local, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(local, "private.pkg"), buildPackage(t, "1.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Run(t.Context(), engine.Options{ConfigPath: filepath.Join(local, "stemma.yaml"), CacheDir: t.TempDir(), Method: "update", Resources: []string{"MacSoftware/private"}}); err != nil {
		t.Fatal(err)
	}
	lock, err := os.ReadFile(lockfile.Filename(local))
	if err != nil {
		t.Fatal(err)
	}
	files["stemma.lock.yaml"] = string(lock)
	o := newOrigin(t, files)
	gh := newFakeGitHub(t, o)
	checkout := filepath.Join(t.TempDir(), "checkout")
	if _, err := gogit.PlainCloneContext(t.Context(), checkout, &gogit.CloneOptions{URL: gh.remote(), ClientOptions: []client.Option{gh.auth()}}); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: filepath.Join(checkout, "stemma.yaml"), CacheDir: t.TempDir(), StateDir: t.TempDir()}
	report, err := Run(t.Context(), opts)
	if err != nil || report.Apply.Skipped || report.Apply.Error != "" || len(report.Updates) != 0 || len(gh.open()) != 0 {
		t.Fatalf("suspended resource disturbed the run: %v\n%+v\n%+v", err, report, report.Apply)
	}
	if status := gh.status(o.tip("main"), applyContext); status["state"] != "success" {
		t.Fatalf("apply status: %v", status)
	}
	if published, _ := filepath.Glob(filepath.Join(munki, "pkgsinfo", "*.plist")); len(published) != 1 {
		t.Fatalf("published items: %v", published)
	}
}

func TestRunProposesIndependentResourcesAfterResolutionFailure(t *testing.T) {
	installer := buildPackage(t, "1.0")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixture.pkg" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(installer)
	}))
	t.Cleanup(server.Close)
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testKey(t))
	broken := strings.ReplaceAll(software(server.URL), "fixture", "broken")
	consumer := `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: consumer}
spec:
  source: {resource: {kind: MacSoftware, name: broken}}
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`
	o := newOrigin(t, map[string]string{
		"stemma.yaml":            project(filepath.Join(t.TempDir(), "munki")),
		"broken.software.yaml":   broken,
		"consumer.software.yaml": consumer,
		"fixture.software.yaml":  software(server.URL),
	})
	gh := newFakeGitHub(t, o)
	checkout := filepath.Join(t.TempDir(), "checkout")
	if _, err := gogit.PlainCloneContext(t.Context(), checkout, &gogit.CloneOptions{URL: gh.remote(), ClientOptions: []client.Option{gh.auth()}}); err != nil {
		t.Fatal(err)
	}
	report, err := Run(t.Context(), Options{ConfigPath: filepath.Join(checkout, "stemma.yaml"), CacheDir: t.TempDir(), StateDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "update: 1 resource failed") || len(report.Updates) != 3 {
		t.Fatalf("resolution failures were not reported independently: %+v, %v", report, err)
	}
	updates := map[string]Update{}
	for _, update := range report.Updates {
		updates[update.Resource] = update
	}
	if updates["MacSoftware/broken"].Action != "failed" || !strings.Contains(updates["MacSoftware/broken"].Error, "HTTP 404") {
		t.Fatalf("source failure missing: %+v", updates)
	}
	if updates["MacSoftware/consumer"].Action != "blocked" || !strings.Contains(updates["MacSoftware/consumer"].Error, "MacSoftware/broken") {
		t.Fatalf("blocked consumer missing: %+v", updates)
	}
	if updates["MacSoftware/fixture"].Action != "created" || len(gh.open()) != 1 || gh.status(o.tip("stemma/MacSoftware/fixture"), planContext)["state"] != "success" {
		t.Fatalf("independent proposal failed: %+v", updates)
	}
}

func TestApplySummaryDistinguishesBlockedResources(t *testing.T) {
	report := engine.Report{Resources: []engine.ResourceReport{
		{Name: "producer", Error: "source unavailable"},
		{Name: "consumer", Error: "blocked by producer", BlockedBy: []string{"producer"}},
		{Name: "healthy"},
	}}
	state, summary := applySummary(report, errors.New("source unavailable"))
	if state != "failure" || summary != "1 resource failed: producer; 1 resource blocked" {
		t.Fatalf("blocked resource counted as failed: %s: %s", state, summary)
	}
}
