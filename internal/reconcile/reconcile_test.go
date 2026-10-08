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
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
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

// lock reads the lockfile a branch holds.
func (o origin) lock(t *testing.T, branch string) lockfile.File {
	t.Helper()
	file, err := o.commitObject(t, o.tip(branch)).File("stemma.lock.yaml")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	locked, err := lockfile.Parse([]byte(contents))
	if err != nil {
		t.Fatal(err)
	}
	return locked
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

	mu             sync.Mutex
	origin         origin
	backend        http.Handler
	pulls          []*pullRequest
	statuses       map[string][]map[string]string
	tokens         atomic.Int32
	rejectStatuses atomic.Bool
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
		if f.rejectStatuses.Load() {
			http.Error(w, "status unavailable", http.StatusForbidden)
			return
		}
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

// decline closes the open proposal of one branch without merging it.
func (f *fakeGitHub) decline(branch string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pull := range f.pulls {
		if pull.State == "open" && pull.Head.Ref == branch {
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
	if err := pkgbuild.Build(t.Context(), source, output, pkgbuild.Options{Identifier: "edu.example.fixture", Version: version, Payload: "payload", Compression: pkgbuild.Gzip, InstallLocation: "/Library/Example", Timestamp: time.Unix(0, 0).UTC()}); err != nil {
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
	// run expects the apply to fail with wantErr while the update phase
	// succeeds, or the whole run to succeed.
	run := func(wantErr string) Report {
		t.Helper()
		report, err := Run(t.Context(), opts)
		if wantErr == "" && err != nil {
			t.Fatalf("run failed: %v\n%+v", err, report)
		}
		if wantErr != "" && (!errors.Is(err, ErrFailed) || report.Error != "" || !strings.Contains(report.Apply.Error, wantErr) || report.Update.Failed()) {
			t.Fatalf("run error %v, want an apply failing with %q\n%+v", err, wantErr, report)
		}
		return report
	}
	head := o.tip("main")

	// An unlocked catalog cannot be applied, but the missing lock is proposed.
	first := run("lockfile")
	if first.Branch != "main" || first.Head != head || first.Apply == nil || first.Apply.Error == "" || gh.status(head, applyContext)["state"] != "failure" {
		t.Fatalf("apply failure not reported: %+v", first)
	}
	if len(first.Update.Proposals) != 1 || first.Update.Proposals[0].Action != "created" || first.Update.Proposals[0].Branch != branch {
		t.Fatalf("proposal: %+v", first.Update.Proposals)
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
	if len(pulls) != 1 || pulls[0].Head.Ref != branch || pulls[0].Title != "Update MacSoftware/fixture to 1.0" || !strings.Contains(pulls[0].Body, "| Source | `source` added: `fixture.pkg` |") || !strings.Contains(pulls[0].Body, "| repo | Upload ") {
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
	if second.Update.Proposals[0].Action != "unchanged" || o.tip(branch) != tip || len(gh.open()) != 1 {
		t.Fatalf("idempotent run rewrote the proposal: %+v", second.Update.Proposals)
	}

	// A squash merge applies with a fresh cache and retires the
	// branch, although its commit never joined the reviewed history.
	gh.squash(t, pulls[0].Number)
	merged := o.tip("main")
	opts.CacheDir = t.TempDir()
	beforeFetch := downloads.Load()
	third := run("")
	if third.Apply.Error != "" || third.Apply.Skipped || gh.status(merged, applyContext)["state"] != "success" {
		t.Fatalf("merged lock was not applied: %+v", third.Apply)
	}
	if len(third.Update.Proposals) != 1 || third.Update.Proposals[0].Action != "retired" || third.Update.Proposals[0].Summary != "merged into the reviewed branch" || o.tip(branch) != "" || len(gh.open()) != 0 {
		t.Fatalf("merged proposal not retired: %+v", third.Update.Proposals)
	}
	if _, err := os.Stat(filepath.Join(munki, "pkgsinfo")); err != nil {
		t.Fatal("apply did not publish into the repository destination")
	}
	if downloads.Load() != beforeFetch+1 {
		t.Fatalf("fresh publication did not fetch reviewed bytes once: %d", downloads.Load()-beforeFetch)
	}
	// Losing only the marker repeats apply, reusing verified cached bytes.
	opts.StateDir = t.TempDir()
	if warm := run(""); warm.Apply.Skipped || downloads.Load() != beforeFetch+1 {
		t.Fatalf("warm publication fetched again: %+v downloads=%d", warm.Apply, downloads.Load()-beforeFetch)
	}
	fourth := run("")
	if !fourth.Apply.Skipped || len(fourth.Update.Proposals) != 0 {
		t.Fatalf("applied commit was reconciled again: %+v", fourth)
	}

	// A new upstream release becomes a fresh proposal with its version.
	payload.Store(buildPackage(t, "2.0"))
	fifth := run("")
	if len(fifth.Update.Proposals) != 1 || fifth.Update.Proposals[0].Action != "created" || gh.open()[0].Title != "Update MacSoftware/fixture to 2.0" {
		t.Fatalf("release proposal: %+v %+v", fifth.Update.Proposals, gh.open())
	}
	if pull := gh.open()[0]; !strings.Contains(pull.Body, "| Source | `source`: new content for `fixture.pkg` |") || !strings.Contains(pull.Body, "| Prepared | `fixture-2.0.pkg` (2.0) |") {
		t.Fatalf("release body: %s", pull.Body)
	}

	// While the release awaits review, its proposal's observation confirms
	// the bytes conditionally instead of downloading them again.
	served := downloads.Load()
	if again := run(""); again.Update.Proposals[0].Action != "unchanged" || downloads.Load() != served {
		t.Fatalf("pending release downloaded again: %+v downloads=%d", again.Update.Proposals, downloads.Load()-served)
	}

	// A person's commit on the branch ends the reconciler's ownership.
	o.commitBranch(t, branch, map[string]string{"NOTES.md": "reviewing"})
	reviewed := o.tip(branch)
	sixth := run("")
	if sixth.Update.Proposals[0].Action != "skipped" || o.tip(branch) != reviewed || len(gh.open()) != 1 {
		t.Fatalf("touched branch was changed: %+v", sixth.Update.Proposals)
	}
	if err := o.bare(t).Storer.RemoveReference(plumbing.NewBranchReferenceName(branch)); err != nil {
		t.Fatal(err)
	}
	gh.closeAll()

	// Removing a document leaves stale entries: apply fails and the lock
	// refresh proposes dropping them.
	o.commit(t, "retire fixture", map[string]string{"fixture.software.yaml": ""})
	seventh := run("stale")
	if seventh.Apply.Error == "" || len(seventh.Update.Proposals) != 1 || seventh.Update.Proposals[0].Action != "created" || seventh.Update.Proposals[0].Name != refreshName {
		t.Fatalf("cleanup proposal: %+v", seventh)
	}
	cleanup := o.tip(refreshBranch)
	if _, err := o.commitObject(t, cleanup).File("stemma.lock.yaml"); !errors.Is(err, object.ErrFileNotFound) || gh.open()[0].Title != "Refresh locks" {
		t.Fatalf("cleanup branch: %v %+v", err, gh.open())
	}
	if pull := gh.open()[0]; !strings.Contains(pull.Body, "| `MacSoftware/fixture` | Removed |  |") || !strings.Contains(pull.Body, "The reviewed branch can't apply until this merges.") {
		t.Fatalf("cleanup body: %s", pull.Body)
	}
	if status := gh.status(cleanup, planContext); status["state"] != "success" || status["description"] != "1 lock removed" {
		t.Fatalf("cleanup status: %v", status)
	}

	// Closing the proposal without merging declines it until it changes.
	gh.closeAll()
	eighth := run("stale")
	if eighth.Update.Proposals[0].Action != "declined" || len(gh.open()) != 0 || o.tip(refreshBranch) != cleanup {
		t.Fatalf("declined proposal was reopened: %+v", eighth.Update.Proposals)
	}

	// An unrelated change to the reviewed branch does not revive it.
	o.commit(t, "unrelated change", map[string]string{"README.md": "catalog\n"})
	ninth := run("stale")
	if ninth.Update.Proposals[0].Action != "declined" || len(gh.open()) != 0 || o.tip(refreshBranch) != cleanup {
		t.Fatalf("declined proposal was revived by a base move: %+v", ninth.Update.Proposals)
	}
	assertUndisturbed(t, cloned, checkout, head)
}

func TestApplyRetriesWhenCommitStatusWasNotRecorded(t *testing.T) {
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testKey(t))
	munki := filepath.Join(t.TempDir(), "munki")
	o := newOrigin(t, map[string]string{"stemma.yaml": project(munki), "policy.software.yaml": policy})
	gh := newFakeGitHub(t, o)
	checkout := filepath.Join(t.TempDir(), "checkout")
	if _, err := gogit.PlainCloneContext(t.Context(), checkout, &gogit.CloneOptions{URL: gh.remote(), ClientOptions: []client.Option{gh.auth()}}); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: filepath.Join(checkout, "stemma.yaml"), CacheDir: t.TempDir(), StateDir: t.TempDir()}
	gh.rejectStatuses.Store(true)
	failed, err := Run(t.Context(), opts)
	if !errors.Is(err, ErrFailed) || failed.Apply == nil || !strings.Contains(failed.Apply.Error, "commit status") {
		t.Fatalf("status failure was not reported: %+v, %v", failed, err)
	}
	if m, err := readMarker(opts.StateDir); err != nil || m.Applied != "" {
		t.Fatalf("status failure moved the completion marker: %+v, %v", m, err)
	}
	gh.rejectStatuses.Store(false)
	retried, err := Run(t.Context(), opts)
	if err != nil || retried.Apply == nil || retried.Apply.Skipped || retried.Apply.Failed() || gh.status(retried.Head, applyContext)["state"] != "success" {
		t.Fatalf("status was not repaired by the next run: %+v, %v", retried, err)
	}
	if m, err := readMarker(opts.StateDir); err != nil || m.Applied != retried.Head {
		t.Fatalf("completed apply was not recorded: %+v, %v", m, err)
	}
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

func TestRunReadsSourceControlSettingsWhenConnecting(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stemma.yaml"), []byte(project("/munki")), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "")
	if err := os.Unsetenv("GITHUB_APP_PRIVATE_KEY"); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://github.com/example/catalog.git"}}); err != nil {
		t.Fatal(err)
	}
	// Loading keeps the settings as written; connecting reads their values.
	if _, err := Run(t.Context(), Options{ConfigPath: filepath.Join(root, "stemma.yaml")}); err == nil || !strings.Contains(err.Error(), "source_control config") || !strings.Contains(err.Error(), "required reference is missing") {
		t.Fatalf("source control connected without its settings: %v", err)
	}
}

// entry is a lock entry for content under a declaration.
func entry(content, declaration, filename string) source.Entry {
	return source.Entry{Version: 1, Resolver: "http", ResolverVersion: "2", Declaration: strings.Repeat(declaration, 64), Observation: []byte(`{}`), Content: source.Content{SHA256: strings.Repeat(content, 64), Filename: filename}}
}

func key(name string) string { return "stemma/v1alpha1/MacSoftware/" + name }

// TestChangesetsSplitUpdatesFromTheLockRefresh covers which branch a lock
// difference lands on: new content is reviewed per resource, everything that
// keeps the reviewed content shares the lock refresh.
func TestChangesetsSplitUpdatesFromTheLockRefresh(t *testing.T) {
	locked := func(e source.Entry) map[string]source.Entry { return map[string]source.Entry{"source": e} }
	moved := entry("b", "2", "moved.pkg")
	moved.InputVersion = "1.0"
	candidate := engine.Candidate{
		Lock: lockfile.File{Version: lockfile.Version, Inputs: map[string]map[string]source.Entry{
			key("released"):  locked(entry("a", "1", "released-1.pkg")),
			key("moved"):     locked(entry("b", "1", "moved.pkg")),
			key("retired"):   locked(entry("c", "1", "retired.pkg")),
			key("unchanged"): locked(entry("d", "1", "unchanged.pkg")),
			key("trimmed"):   {"source": entry("e", "1", "trimmed.pkg"), "extra": entry("f", "1", "extra.pkg")},
		}},
		Resources: map[string]engine.CandidateResource{
			key("released"):  {Kind: "MacSoftware", Name: "released", Inputs: locked(entry("0", "1", "released-2.pkg"))},
			key("moved"):     {Kind: "MacSoftware", Name: "moved", Inputs: locked(moved)},
			key("unchanged"): {Kind: "MacSoftware", Name: "unchanged", Inputs: locked(entry("d", "1", "unchanged.pkg"))},
			key("trimmed"):   {Kind: "MacSoftware", Name: "trimmed", Inputs: locked(entry("e", "1", "trimmed.pkg"))},
			key("added"):     {Kind: "MacSoftware", Name: "added", Inputs: locked(entry("9", "1", "added.pkg"))},
		},
	}
	sets := changesets(candidate)
	var got []string
	for _, set := range sets {
		got = append(got, set.branch+" "+title(set, planned{})+" "+commitMessage(set))
	}
	want := []string{
		"stemma/MacSoftware/added Update MacSoftware/added chore(stemma): update MacSoftware/added inputs",
		"stemma/MacSoftware/released Update MacSoftware/released chore(stemma): update MacSoftware/released inputs",
		"stemma/MacSoftware/trimmed Update MacSoftware/trimmed chore(stemma): update MacSoftware/trimmed inputs",
		"stemma/refresh-locks Refresh locks chore(stemma): refresh locks",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("changesets:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	refresh := sets[len(sets)-1]
	if members := slices.Sorted(maps.Keys(refresh.changes)); !slices.Equal(members, []string{key("moved"), key("retired")}) || !refresh.changes[key("retired")].removed {
		t.Fatalf("lock refresh members: %v", members)
	}
	prepared := planned{report: &engine.Report{Resources: []engine.ResourceReport{{Kind: "MacSoftware", Name: "released", Key: key("released"), Artifacts: map[string]engine.Prepared{"installer": {Filename: "released-2.pkg", Version: "2"}}}}}}
	if got := title(sets[1], prepared); got != "Update MacSoftware/released to 2" {
		t.Fatalf("title %q", got)
	}
	if got := planSummary(refresh, planned{}); got != "1 lock refreshed, 1 removed" {
		t.Fatalf("status %q", got)
	}
}

// TestProposalBodySummarisesWithoutValues covers the durable description:
// destination effects by field name, and failures without their error text.
func TestProposalBodySummarisesWithoutValues(t *testing.T) {
	secret := `"#!/bin/sh\ncurl -H 'Authorization: s3cr3t' https://example.test\n"`
	update := changeset{branch: "stemma/MacSoftware/app", key: key("app"), changes: map[string]change{key("app"): {
		kind: "MacSoftware", name: "app",
		before: map[string]source.Entry{"source": entry("a", "1", "App 1.pkg")},
		after:  map[string]source.Entry{"source": entry("b", "1", "App 2.pkg")},
	}}}
	plan := planned{report: &engine.Report{Resources: []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "consumer", Key: key("consumer"), Destinations: []engine.DestinationReport{{Name: "woodstar"}}},
		{Kind: "MacSoftware", Name: "app", Key: key("app"), Artifacts: map[string]engine.Prepared{"installer": {Filename: "App-2.pkg", Version: "2"}}, Destinations: []engine.DestinationReport{
			{Name: "woodstar", Changes: []plugin.Change{
				{Kind: "content", Field: "package.installer", Action: "upload", Filename: "App-2.pkg"},
				{Kind: "metadata", Field: "package.postinstall_script", Action: "set", Before: json.RawMessage(`"exit 0\n"`), After: json.RawMessage(secret)},
				{Kind: "metadata", Field: "package.version", Action: "set", Before: json.RawMessage(`"1"`), After: json.RawMessage(`"2"`)},
				{Kind: "retention", Field: "package", Action: "delete", Before: json.RawMessage(`"0.9"`)},
				{Kind: "metadata", Field: "token", Action: "delete", Before: json.RawMessage(`"s3cr3t"`)},
			}},
			{Name: "munki", Error: "upload rejected: Authorization: s3cr3t"},
		}},
	}}}
	text := body(update, plan)
	for _, want := range []string{
		"Stemma found an update for `MacSoftware/app`.",
		"| Source | `source`: `App 1.pkg` → `App 2.pkg` |",
		"| Prepared | `App-2.pkg` (2) |",
		"| woodstar | Upload `App-2.pkg`; update `package.postinstall_script`, `package.version`; delete `package` (retention), `token` |",
		"| munki | Planning failed |",
		"| woodstar (`MacSoftware/consumer`) | No changes |",
		"Close this PR and Stemma won't propose these inputs again.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("body lacks %q:\n%s", want, text)
		}
	}
	if strings.Index(text, "| woodstar |") > strings.Index(text, "`MacSoftware/consumer`") {
		t.Errorf("consumer listed before the proposal's own destinations:\n%s", text)
	}
	// The commit status is the verdict; a passing body has nothing to add.
	if strings.Contains(text, "report") {
		t.Errorf("passing body points at the report:\n%s", text)
	}
	plan.err = errors.New("prepare: upload rejected: Authorization: s3cr3t")
	plan.report = &engine.Report{Resources: []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "app", Key: key("app"), Error: "download: Authorization: s3cr3t"},
		{Kind: "MacSoftware", Name: "consumer", Key: key("consumer"), Error: "blocked", BlockedBy: []string{key("app")}},
	}}
	failed := body(update, plan)
	for _, want := range []string{
		"| `MacSoftware/app` | Preparation failed |",
		"| `MacSoftware/consumer` | Blocked by `MacSoftware/app` |",
		"\nThe reconcile report has the details.\n",
	} {
		if !strings.Contains(failed, want) {
			t.Errorf("failed body lacks %q:\n%s", want, failed)
		}
	}
	if summary := planSummary(update, plan); summary != "failure: MacSoftware/app could not be prepared; MacSoftware/consumer is blocked by MacSoftware/app" {
		t.Errorf("status %q", summary)
	}
	plan.report = &engine.Report{}
	if stopped := body(update, plan); !strings.Contains(stopped, "Planning stopped before any resource finished. The reconcile report has the details.") {
		t.Errorf("stopped body:\n%s", stopped)
	}
	for _, text := range []string{text, failed} {
		if strings.Contains(text, "s3cr3t") {
			t.Fatalf("durable description repeats a value:\n%s", text)
		}
	}
}

// TestRefreshBodyRowsOnlyWhatMergingAffects covers the lock refresh's
// description: rows for stale, removed, failing and publishing resources, and
// the rest by name.
func TestRefreshBodyRowsOnlyWhatMergingAffects(t *testing.T) {
	member := func(name string, before, after source.Entry) change {
		return change{kind: "MacSoftware", name: name, before: map[string]source.Entry{"source": before}, after: map[string]source.Entry{"source": after}}
	}
	upgraded := entry("b", "1", "upgraded.pkg")
	upgraded.ResolverVersion = "3"
	versioned := entry("c", "1", "versioned.pkg")
	versioned.InputVersion = "s3cr3t"
	refresh := changeset{branch: refreshBranch, changes: map[string]change{
		key("moved"):     member("moved", entry("a", "1", "moved.pkg"), entry("a", "2", "moved.pkg")),
		key("upgraded"):  member("upgraded", entry("b", "1", "upgraded.pkg"), upgraded),
		key("versioned"): member("versioned", entry("c", "1", "versioned.pkg"), versioned),
		key("quiet"):     member("quiet", entry("d", "1", "quiet-1.pkg"), entry("d", "1", "quiet.pkg")),
		key("silent"):    member("silent", entry("e", "1", "silent-1.pkg"), entry("e", "1", "silent.pkg")),
		key("retired"):   {kind: "MacSoftware", name: "retired", before: map[string]source.Entry{"source": entry("f", "1", "retired.pkg")}, removed: true},
	}}
	unchanged := []engine.DestinationReport{{Name: "munki"}, {Name: "woodstar"}}
	report := func(name string, destinations ...engine.DestinationReport) engine.ResourceReport {
		return engine.ResourceReport{Kind: "MacSoftware", Name: name, Key: key(name), Destinations: destinations}
	}
	plan := planned{err: errors.New("woodstar: Authorization: s3cr3t"), report: &engine.Report{Resources: []engine.ResourceReport{
		report("moved", engine.DestinationReport{Name: "munki", Changes: []plugin.Change{{Kind: "metadata", Field: "description", Action: "set", After: json.RawMessage(`"s3cr3t"`)}}}, engine.DestinationReport{Name: "woodstar"}),
		report("upgraded", engine.DestinationReport{Name: "munki"}, engine.DestinationReport{Name: "woodstar", Error: "Authorization: s3cr3t"}),
		report("versioned", engine.DestinationReport{Name: "woodstar", Changes: []plugin.Change{{Kind: "metadata", Field: "version", Action: "set"}}}),
		report("quiet", unchanged...),
		report("silent", unchanged...),
		report("consumer", engine.DestinationReport{Name: "woodstar", Changes: []plugin.Change{{Kind: "metadata", Field: "version", Action: "set"}}}),
		report("bystander", unchanged...),
	}}}
	text := body(refresh, plan)
	for _, want := range []string{
		"Stemma refreshed the lock for 5 resources and removed 1 the catalog no longer declares. Every input resolves to the content already locked.\n",
		"| `MacSoftware/moved` | Declaration changed | `munki`: update `description`<br>`woodstar`: no changes |",
		"| `MacSoftware/upgraded` | Resolver changed | `munki`: no changes<br>`woodstar`: planning failed |",
		"| `MacSoftware/versioned` | Metadata refreshed | `woodstar`: update `version` |",
		"| `MacSoftware/retired` | Removed |  |",
		"| `MacSoftware/consumer` | Unchanged | `woodstar`: update `version` |",
		"| 2 others | Metadata refreshed | No changes |",
		"The reviewed branch can't apply until this merges.",
		"<summary>2 others</summary>\n\n`MacSoftware/quiet`, `MacSoftware/silent`\n",
		"\nThe reconcile report has the details.\n",
		"Close this PR and Stemma won't propose it again until one of these entries changes.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("body lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "bystander") || strings.Contains(text, "s3cr3t") {
		t.Errorf("body names an unaffected consumer or repeats a value:\n%s", text)
	}
	if summary := planSummary(refresh, plan); summary != "failure: woodstar could not plan MacSoftware/upgraded" {
		t.Errorf("status %q", summary)
	}

	// Without the removal, stale entries only keep their own resources from
	// applying.
	delete(refresh.changes, key("retired"))
	if text := body(refresh, plan); !strings.Contains(text, "The reviewed branch can't apply the resources with a changed declaration or resolver until this merges.") {
		t.Errorf("stale body:\n%s", text)
	}

	// Nothing to look at: no table, every member by name.
	quiet := changeset{branch: refreshBranch, changes: map[string]change{key("quiet"): refresh.changes[key("quiet")], key("silent"): refresh.changes[key("silent")]}}
	passed := planned{report: &engine.Report{Resources: []engine.ResourceReport{report("quiet", unchanged...), report("silent", unchanged...)}}}
	text = body(quiet, passed)
	if !strings.HasPrefix(text, "Stemma refreshed the lock for 2 resources. Every input resolves to the content already locked, and no destination changes.\n\n<details>\n<summary>2 resources</summary>\n\n`MacSoftware/quiet`, `MacSoftware/silent`\n\n</details>\n\n---\n") {
		t.Errorf("quiet body:\n%s", text)
	}
	if summary := planSummary(quiet, passed); summary != "2 locks refreshed: 0 planned changes across 2 destinations" {
		t.Errorf("status %q", summary)
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

func TestDiffKeepsSkippedResourcesLocked(t *testing.T) {
	const key = "stemma/v1alpha1/MacSoftware/private"
	entries := map[string]source.Entry{"source": {Version: 1}}
	candidate := engine.Candidate{Lock: lockfile.File{Version: lockfile.Version, Inputs: map[string]map[string]source.Entry{key: entries}}, Resources: map[string]engine.CandidateResource{key: {Name: "private", Kind: "MacSoftware", Skipped: true}}}
	if changes := diff(candidate); len(changes) != 0 {
		t.Fatalf("skipped resource was proposed: %+v", changes)
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
	if err != nil || report.Apply.Skipped || report.Apply.Error != "" || len(report.Update.Proposals) != 0 || len(gh.open()) != 0 {
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
	if !errors.Is(err, ErrFailed) || report.Update.Error != "" || !report.Update.Failed() || len(report.Update.Proposals) != 3 {
		t.Fatalf("resolution failures were not reported independently: %+v, %v", report, err)
	}
	updates := map[string]Proposal{}
	for _, update := range report.Update.Proposals {
		updates[update.Name] = update
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

func TestRunRefreshesLocksInOneProposal(t *testing.T) {
	installer := buildPackage(t, "1.0")
	var release atomic.Value
	release.Store(installer)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/d.pkg" {
			_, _ = w.Write(release.Load().([]byte))
			return
		}
		_, _ = w.Write(installer)
	}))
	t.Cleanup(server.Close)
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testKey(t))
	document := func(name string) string { return strings.ReplaceAll(software(server.URL), "fixture", name) }
	o := newOrigin(t, map[string]string{
		"stemma.yaml":          project(filepath.Join(t.TempDir(), "munki")),
		"a.software.yaml":      document("a"),
		"b.software.yaml":      document("b"),
		"c.software.yaml":      document("c"),
		"d.software.yaml":      document("d"),
		"policy.software.yaml": policy,
	})
	// Record reviewed inputs using a separate cache, as another machine would.
	if _, err := engine.Run(t.Context(), engine.Options{ConfigPath: filepath.Join(o.dir, "stemma.yaml"), CacheDir: t.TempDir(), Method: "update"}); err != nil {
		t.Fatal(err)
	}
	lock, err := os.ReadFile(filepath.Join(o.dir, "stemma.lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	o.commit(t, "review inputs", map[string]string{"stemma.lock.yaml": string(lock)})
	reviewed := o.lock(t, "main")
	gh := newFakeGitHub(t, o)
	checkout := filepath.Join(t.TempDir(), "checkout")
	if _, err := gogit.PlainCloneContext(t.Context(), checkout, &gogit.CloneOptions{URL: gh.remote(), ClientOptions: []client.Option{gh.auth()}}); err != nil {
		t.Fatal(err)
	}
	opts := Options{ConfigPath: filepath.Join(checkout, "stemma.yaml"), CacheDir: t.TempDir(), StateDir: t.TempDir()}
	if report, err := Run(t.Context(), opts); err != nil || len(report.Update.Proposals) != 0 {
		t.Fatalf("reviewed catalog did not apply cleanly: %v\n%+v", err, report)
	}

	// Two downloads move without changing their bytes, one document goes and
	// one resource releases new content.
	o.commit(t, "move downloads and retire c", map[string]string{
		"a.software.yaml": strings.Replace(document("a"), "/a.pkg", "/a-moved.pkg", 1),
		"b.software.yaml": strings.Replace(document("b"), "/b.pkg", "/b-moved.pkg", 1),
		"c.software.yaml": "",
	})
	release.Store(buildPackage(t, "2.0"))
	const update = "stemma/MacSoftware/d"
	// run expects the reviewed branch to stay unappliable while its lock is
	// stale, and returns the outcome of each proposal by name.
	run := func() map[string]Proposal {
		t.Helper()
		report, err := Run(t.Context(), opts)
		if !errors.Is(err, ErrFailed) || !report.Apply.Failed() || report.Update.Failed() {
			t.Fatalf("want a stale apply beside healthy proposals: %v\n%+v", err, report)
		}
		proposals := map[string]Proposal{}
		for _, proposal := range report.Update.Proposals {
			proposals[proposal.Name] = proposal
		}
		return proposals
	}
	first := run()
	if len(first) != 2 || first["MacSoftware/d"].Action != "created" || first["MacSoftware/d"].Branch != update || first[refreshName].Action != "created" || first[refreshName].Branch != refreshBranch {
		t.Fatalf("proposals: %+v", first)
	}
	titles := map[string]*pullRequest{}
	for _, pull := range gh.open() {
		titles[pull.Title] = pull
	}
	refresh := titles["Refresh locks"]
	if len(titles) != 2 || titles["Update MacSoftware/d to 2.0"] == nil || refresh == nil || refresh.Head.Ref != refreshBranch {
		t.Fatalf("pull requests: %+v", gh.open())
	}
	// The refresh carries every difference that keeps reviewed content and
	// nothing of the release; the release carries only itself.
	refreshed, released := o.lock(t, refreshBranch), o.lock(t, update)
	for _, name := range []string{"a", "b"} {
		before, after := reviewed.Inputs[key(name)]["source"], refreshed.Inputs[key(name)]["source"]
		if after.Content.SHA256 != before.Content.SHA256 || after.Declaration == before.Declaration {
			t.Fatalf("%s was not refreshed under its reviewed content: %+v", name, after)
		}
		if !equalEntries(reviewed.Inputs[key(name)], released.Inputs[key(name)]) {
			t.Fatalf("the release changed %s", name)
		}
	}
	if _, kept := refreshed.Inputs[key("c")]; kept || !equalEntries(reviewed.Inputs[key("d")], refreshed.Inputs[key("d")]) {
		t.Fatalf("refresh lock: %+v", refreshed.Inputs)
	}
	if _, kept := released.Inputs[key("c")]; !kept || equalEntries(reviewed.Inputs[key("d")], released.Inputs[key("d")]) {
		t.Fatalf("release lock: %+v", released.Inputs)
	}
	for _, want := range []string{
		"Stemma refreshed the lock for 2 resources and removed 1 the catalog no longer declares.",
		"| `MacSoftware/a` | Declaration changed | No changes |",
		"| `MacSoftware/b` | Declaration changed | No changes |",
		"| `MacSoftware/c` | Removed |  |",
		"The reviewed branch can't apply until this merges.",
	} {
		if !strings.Contains(refresh.Body, want) {
			t.Errorf("refresh body lacks %q:\n%s", want, refresh.Body)
		}
	}
	tip := o.tip(refreshBranch)
	if status := gh.status(tip, planContext); status["state"] != "success" || status["description"] != "2 locks refreshed, 1 removed: 0 planned changes across 1 destination" {
		t.Fatalf("refresh status: %v", status)
	}

	// Nothing changed: both proposals stay exactly as pushed.
	if again := run(); again["MacSoftware/d"].Action != "unchanged" || again[refreshName].Action != "unchanged" || o.tip(refreshBranch) != tip {
		t.Fatalf("idempotent run rewrote a proposal: %+v", again)
	}

	// Closing the refresh declines the set while its entries stay the same.
	gh.decline(refreshBranch)
	if declined := run(); declined[refreshName].Action != "declined" || declined["MacSoftware/d"].Action != "unchanged" || o.tip(refreshBranch) != tip || len(gh.open()) != 1 {
		t.Fatalf("declined refresh was reopened: %+v", declined)
	}

	// Another entry changing proposes the whole set again.
	o.commit(t, "move a again", map[string]string{"a.software.yaml": strings.Replace(document("a"), "/a.pkg", "/a-again.pkg", 1)})
	revived := run()
	if revived[refreshName].Action != "updated" || o.tip(refreshBranch) == tip || len(gh.open()) != 2 {
		t.Fatalf("changed refresh stayed declined: %+v", revived)
	}

	// Merging it lets the reviewed branch apply and retires the branch, while
	// the release keeps waiting for its own review.
	for _, pull := range gh.open() {
		if pull.Head.Ref == refreshBranch {
			gh.squash(t, pull.Number)
		}
	}
	report, err := Run(t.Context(), opts)
	if err != nil || report.Apply.Failed() || gh.status(o.tip("main"), applyContext)["state"] != "success" {
		t.Fatalf("merged refresh did not apply: %v\n%+v", err, report.Apply)
	}
	merged := map[string]Proposal{}
	for _, proposal := range report.Update.Proposals {
		merged[proposal.Name] = proposal
	}
	if merged[refreshName].Action != "retired" || merged[refreshName].Summary != "merged into the reviewed branch" || o.tip(refreshBranch) != "" || merged["MacSoftware/d"].Action != "updated" || len(gh.open()) != 1 {
		t.Fatalf("merged refresh was not retired beside the release: %+v", merged)
	}
}

func TestRunRejectsShallowCheckouts(t *testing.T) {
	o := newOrigin(t, map[string]string{
		"stemma.yaml":           project(filepath.Join(t.TempDir(), "munki")),
		"fixture.software.yaml": software("https://downloads.example"),
	})
	gh := newFakeGitHub(t, o)
	checkout := filepath.Join(t.TempDir(), "checkout")
	cloned, err := gogit.PlainCloneContext(t.Context(), checkout, &gogit.CloneOptions{URL: gh.remote(), ClientOptions: []client.Option{gh.auth()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cloned.Storer.SetShallow([]plumbing.Hash{plumbing.NewHash(o.tip("main"))}); err != nil {
		t.Fatal(err)
	}
	// A shallow checkout cannot tell proposals from reviewed commits.
	if _, err := Run(t.Context(), Options{ConfigPath: filepath.Join(checkout, "stemma.yaml"), CacheDir: t.TempDir(), StateDir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "shallow") {
		t.Fatalf("shallow checkout accepted: %v", err)
	}
}

func TestRunStopsOnceWhenTheReviewedProjectDoesNotLoad(t *testing.T) {
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testKey(t))
	o := newOrigin(t, map[string]string{
		"stemma.yaml":           project(filepath.Join(t.TempDir(), "munki")),
		"fixture.software.yaml": software("https://downloads.example"),
		"stemma.lock.yaml":      "version: 2\ninputs: {}\n",
	})
	gh := newFakeGitHub(t, o)
	checkout := filepath.Join(t.TempDir(), "checkout")
	if _, err := gogit.PlainCloneContext(t.Context(), checkout, &gogit.CloneOptions{URL: gh.remote(), ClientOptions: []client.Option{gh.auth()}}); err != nil {
		t.Fatal(err)
	}
	head := o.tip("main")
	report, err := Run(t.Context(), Options{ConfigPath: filepath.Join(checkout, "stemma.yaml"), CacheDir: t.TempDir(), StateDir: t.TempDir()})
	want := "main@" + short(head) + ": lockfile: version 2 is not supported; delete it and run stemma update"
	if err == nil || errors.Is(err, ErrFailed) || err.Error() != want || report.Error != want {
		t.Fatalf("run error %v, want %q", err, want)
	}
	// Neither phase ran, so neither reports the same failure again.
	if report.Apply != nil || report.Update != nil || len(gh.open()) != 0 || o.tip("stemma/MacSoftware/fixture") != "" {
		t.Fatalf("a phase ran: %+v", report)
	}
	if status := gh.status(head, applyContext); status["state"] != "failure" || status["description"] != failedDescription {
		t.Fatalf("apply status: %v", status)
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

func TestRunPlansExactProposalsAfterLockedContentMismatch(t *testing.T) {
	var payload atomic.Value
	var invalidA atomic.Bool
	payload.Store(buildPackage(t, "1.0"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if invalidA.Load() && r.URL.Path == "/a.pkg" {
			_, _ = w.Write([]byte("not an installer"))
			return
		}
		_, _ = w.Write(payload.Load().([]byte))
	}))
	t.Cleanup(server.Close)
	t.Setenv("GITHUB_APP_CLIENT_ID", "Iv1.fixture")
	t.Setenv("GITHUB_APP_INSTALLATION_ID", "7")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", testKey(t))
	munki := filepath.Join(t.TempDir(), "munki")
	consumer := `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: consumer}
spec:
  source: {resource: {kind: MacSoftware, name: a}}
  destinations:
    repo: {pkginfo: {catalogs: [testing]}}
`
	o := newOrigin(t, map[string]string{
		"stemma.yaml":            project(munki),
		"a.software.yaml":        strings.ReplaceAll(software(server.URL), "fixture", "a"),
		"b.software.yaml":        strings.ReplaceAll(software(server.URL), "fixture", "b"),
		"consumer.software.yaml": consumer,
		"policy.software.yaml":   policy,
	})
	// Record reviewed inputs using a separate cache, as another machine would.
	_, err := engine.Run(t.Context(), engine.Options{ConfigPath: filepath.Join(o.dir, "stemma.yaml"), CacheDir: t.TempDir(), Method: "update"})
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(o.dir, "stemma.lock.yaml")
	reviewedBytes, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	reviewed, err := lockfile.Parse(reviewedBytes)
	if err != nil {
		t.Fatal(err)
	}
	o.commit(t, "review inputs", map[string]string{"stemma.lock.yaml": string(reviewedBytes)})
	gh := newFakeGitHub(t, o)
	checkout := filepath.Join(t.TempDir(), "checkout")
	if _, err := gogit.PlainCloneContext(t.Context(), checkout, &gogit.CloneOptions{URL: gh.remote(), ClientOptions: []client.Option{gh.auth()}}); err != nil {
		t.Fatal(err)
	}
	methods := map[string]int{}
	var phases []string
	opts := Options{ConfigPath: filepath.Join(checkout, "stemma.yaml"), CacheDir: t.TempDir(), StateDir: t.TempDir(),
		PhaseStarted: func(method string) error { phases = append(phases, method); return nil },
		ResourceDone: func(method string, _ engine.ResourceReport) error {
			phase := "update"
			if method == "apply" {
				phase = "apply"
			}
			if len(phases) == 0 || phases[len(phases)-1] != phase {
				t.Fatalf("%s resource reported before its phase: %v", method, phases)
			}
			methods[method]++
			return nil
		},
	}
	payload.Store(buildPackage(t, "2.0"))
	report, err := Run(t.Context(), opts)
	if !errors.Is(err, ErrFailed) || !report.Apply.Failed() || report.Update.Failed() {
		t.Fatalf("mismatch should fail publication but allow proposals: %+v, %v", report, err)
	}
	if !slices.Equal(phases, []string{"apply", "update"}) {
		t.Fatalf("phases: %v", phases)
	}
	if m, err := readMarker(opts.StateDir); err != nil || m.Applied != "" {
		t.Fatalf("partial apply advanced marker: %+v, %v", m, err)
	}
	if len(gh.open()) != 2 {
		t.Fatalf("want two proposals: %+v", gh.open())
	}
	if methods["prepare"] != 0 || methods["plan"] != 3 {
		t.Fatalf("want one plan per proposal including consumer: %v", methods)
	}
	for _, resource := range report.Apply.Report.Resources {
		switch resource.Name {
		case "a", "b":
			if !strings.Contains(resource.Error, "differs from the input lock") {
				t.Fatalf("locked mismatch missing: %+v", resource)
			}
		case "consumer":
			if len(resource.BlockedBy) == 0 {
				t.Fatalf("consumer not blocked: %+v", resource)
			}
		case "policy":
			if resource.Error != "" || len(resource.Destinations) != 1 {
				t.Fatalf("independent publication failed: %+v", resource)
			}
		}
	}
	for _, proposal := range report.Update.Proposals {
		name := strings.TrimPrefix(proposal.Name, "MacSoftware/")
		file, err := o.commitObject(t, o.tip(proposal.Branch)).File("stemma.lock.yaml")
		if err != nil {
			t.Fatal(err)
		}
		contents, err := file.Contents()
		if err != nil {
			t.Fatal(err)
		}
		locked, err := lockfile.Parse([]byte(contents))
		if err != nil {
			t.Fatal(err)
		}
		for key, entries := range reviewed.Inputs {
			if key == "stemma/v1alpha1/MacSoftware/"+name {
				if equalEntries(entries, locked.Inputs[key]) {
					t.Fatalf("proposal did not change %s", key)
				}
			} else if !equalEntries(entries, locked.Inputs[key]) {
				t.Fatalf("proposal %s changed unrelated %s", name, key)
			}
		}
		if proposal.Plan == nil || gh.status(o.tip(proposal.Branch), planContext)["state"] != "success" {
			t.Fatalf("proposal plan missing: %+v", proposal)
		}
		for _, resource := range proposal.Plan.Resources {
			if resource.Artifacts["installer"].Version != "2.0" {
				t.Fatalf("plan did not use proposed bytes: %+v", resource)
			}
		}
	}
	// Proposal planning must not have published any of the new installers.
	entries, err := os.ReadDir(filepath.Join(munki, "pkgsinfo"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("proposal wrote destinations: %v", entries)
	}
	current, err := os.ReadFile(lockPath)
	if err != nil || string(current) != string(reviewedBytes) {
		t.Fatalf("reviewed lock changed: %v", err)
	}

	// Acquisition succeeds but invalid package bytes fail planning. The PRs
	// still update, with failing statuses and partial reports including blockers.
	payload.Store(buildPackage(t, "3.0"))
	invalidA.Store(true)
	report, err = Run(t.Context(), opts)
	if !errors.Is(err, ErrFailed) || !report.Update.Failed() || len(gh.open()) != 2 {
		t.Fatalf("failed plans hid proposals: %+v, %v", report, err)
	}
	for _, proposal := range report.Update.Proposals {
		wantAction, wantState := "failed", "failure"
		if proposal.Name == "MacSoftware/b" {
			wantAction, wantState = "updated", "success"
		}
		if proposal.Action != wantAction || proposal.PullRequest == "" || proposal.Plan == nil || gh.status(o.tip(proposal.Branch), planContext)["state"] != wantState {
			t.Fatalf("failed plan not published: %+v", proposal)
		}
	}
}
