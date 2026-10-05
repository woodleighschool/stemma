package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	"github.com/woodleighschool/stemma/internal/pkgbuild"
)

const changedProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: changes}
spec:
  imports: ['software/*.yaml']
  components:
    app:
      destinations:
        repo: {pkginfo: {catalogs: [testing]}}
    build:
      package: {identifier: com.example.build}
  destinations:
    repo: {operation: munki, config: {path: repo}}
`

var changedResources = map[string]string{
	"app.yaml": `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: app}
spec:
  extends: app
  source: {path: app.pkg}
  signatures: [{signer: apple:developer-id:SMLKBTR495}]
  destinations:
    repo: {pkginfo: {description: App}}
`,
	"other.yaml": `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: other}
spec:
  extends: app
  source: {path: other.pkg}
`,
	"build.yaml": `apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata: {name: build}
spec:
  extends: build
  payload:
    /Library/Example/build.txt: {content: build}
  package: {version: "1.0"}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: consumer}
spec:
  extends: app
  source: {resource: {kind: BuildMacPkg, name: build}}
`,
	"paused.yaml": `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: paused}
suspend: true
spec:
  extends: app
  source: {path: paused.pkg}
`,
}

// changedCatalog commits a locked catalog as the Git revision HEAD.
func changedCatalog(t *testing.T) (root string, update, prepare func() (Report, error)) {
	t.Helper()
	root = t.TempDir()
	writeFileText(t, filepath.Join(root, "stemma.yaml"), changedProject)
	for name, document := range changedResources {
		writeFileText(t, filepath.Join(root, "software", name), document)
	}
	fixture, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"app", "other", "paused"} {
		writeFile(t, filepath.Join(root, "software", name+".pkg"), fixture)
	}
	writeFile(t, filepath.Join(root, ".gitignore"), []byte(".stemma/\n"))
	if _, err := gogit.PlainInit(root, false); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	run := func(method, changedSince string) (Report, error) {
		return Run(t.Context(), Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: cache, Method: method, ChangedSince: changedSince})
	}
	update = func() (Report, error) { return run("update", "") }
	prepare = func() (Report, error) { return run("prepare", "HEAD") }
	if _, err := update(); err != nil {
		t.Fatal(err)
	}
	commit(t, root)
	return root, update, prepare
}

func commit(t *testing.T, root string) {
	t.Helper()
	repo, err := gogit.PlainOpen(root)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	signature := &object.Signature{Name: "Human", Email: "human@example.com", When: time.Now()}
	if _, err := tree.Commit("catalog", &gogit.CommitOptions{Author: signature, Committer: signature}); err != nil {
		t.Fatal(err)
	}
}

func writeFileText(t *testing.T, name, text string) {
	t.Helper()
	writeFile(t, name, []byte(text))
}

func writeFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// testPackage builds a small payload package that no other identifier shares.
func testPackage(t *testing.T, identifier string) []byte {
	t.Helper()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(source, "payload", identifier+".txt"), []byte(identifier))
	output := filepath.Join(t.TempDir(), identifier+".pkg")
	if err := pkgbuild.Build(t.Context(), source, output, pkgbuild.Options{Identifier: identifier, Version: "1.0", Payload: "payload", Compression: pkgbuild.Gzip}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func preparedKeys(report Report) []string {
	var keys []string
	for _, resource := range report.Resources {
		keys = append(keys, strings.TrimPrefix(resource.Key, "stemma/v1alpha1/"))
	}
	return keys
}

func TestChangedSincePreparesWhatChangesPreparation(t *testing.T) {
	edit := func(t *testing.T, root, name, old, replacement string) {
		t.Helper()
		filename := filepath.Join(root, "software", name)
		data, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), old) {
			t.Fatalf("%s lacks %q", name, old)
		}
		writeFile(t, filename, []byte(strings.Replace(string(data), old, replacement, 1)))
	}
	for _, test := range []struct {
		name   string
		change func(t *testing.T, root string, update func() (Report, error))
		want   []string
	}{
		{"unchanged", func(*testing.T, string, func() (Report, error)) {}, nil},
		{"destination metadata", func(t *testing.T, root string, _ func() (Report, error)) {
			t.Helper()
			edit(t, root, "app.yaml", "description: App", "description: Edited")
			project := strings.Replace(changedProject, "catalogs: [testing]", "catalogs: [production]", 1)
			writeFileText(t, filepath.Join(root, "stemma.yaml"), project)
		}, nil},
		{"component", func(t *testing.T, root string, _ func() (Report, error)) {
			t.Helper()
			project := strings.Replace(changedProject, "com.example.build", "com.example.renamed", 1)
			writeFileText(t, filepath.Join(root, "stemma.yaml"), project)
		}, []string{"BuildMacPkg/build", "MacSoftware/consumer"}},
		{"reviewed input", func(t *testing.T, root string, update func() (Report, error)) {
			t.Helper()
			writeFile(t, filepath.Join(root, "software", "other.pkg"), testPackage(t, "com.example.other"))
			if _, err := update(); err != nil {
				t.Fatal(err)
			}
		}, []string{"MacSoftware/other"}},
		{"new resource", func(t *testing.T, root string, update func() (Report, error)) {
			t.Helper()
			writeFileText(t, filepath.Join(root, "software", "new.yaml"), strings.ReplaceAll(changedResources["other.yaml"], "other", "new"))
			writeFile(t, filepath.Join(root, "software", "new.pkg"), testPackage(t, "com.example.new"))
			if _, err := update(); err != nil {
				t.Fatal(err)
			}
		}, []string{"MacSoftware/new"}},
		{"removed resource", func(t *testing.T, root string, update func() (Report, error)) {
			t.Helper()
			if err := os.Remove(filepath.Join(root, "software", "other.yaml")); err != nil {
				t.Fatal(err)
			}
			if _, err := update(); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"suspended resource", func(t *testing.T, root string, _ func() (Report, error)) {
			t.Helper()
			edit(t, root, "paused.yaml", "paused.pkg", "renamed.pkg")
		}, nil},
		{"resumed resource", func(t *testing.T, root string, update func() (Report, error)) {
			t.Helper()
			edit(t, root, "paused.yaml", "suspend: true\n", "")
			if _, err := update(); err != nil {
				t.Fatal(err)
			}
		}, []string{"MacSoftware/paused"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, update, prepare := changedCatalog(t)
			test.change(t, root, update)
			report, err := prepare()
			if err != nil {
				t.Fatal(err)
			}
			if got := preparedKeys(report); !slices.Equal(got, test.want) {
				t.Fatalf("prepared %v, want %v", got, test.want)
			}
		})
	}
}

func TestChangedSinceChecksTheLockfileWhereItReadsValues(t *testing.T) {
	// A resource the catalog no longer declares has nothing to prepare, so
	// the whole lockfile's shape is checked first.
	root, _, prepare := changedCatalog(t)
	if err := os.Remove(filepath.Join(root, "software", "other.yaml")); err != nil {
		t.Fatal(err)
	}
	report, err := prepare()
	if err == nil || !strings.Contains(err.Error(), "lockfile: stemma/v1alpha1/MacSoftware/other is no longer declared; run stemma update") || len(report.Resources) != 0 {
		t.Fatalf("leftover lock entries passed: %v %+v", err, report)
	}
	// A changed declaration is prepared, which compares it with its entry.
	root, _, prepare = changedCatalog(t)
	filename := filepath.Join(root, "software", "other.yaml")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filename, []byte(strings.Replace(string(data), "other.pkg", "app.pkg", 1)))
	report, err = prepare()
	if err == nil || !strings.Contains(err.Error(), "input is missing or stale in the lockfile; run stemma update") || !slices.Equal(preparedKeys(report), []string{"MacSoftware/other"}) {
		t.Fatalf("stale entry passed: %v %v", err, preparedKeys(report))
	}
}

func TestChangedSinceChecksEditedLocalInputs(t *testing.T) {
	for _, test := range []struct {
		name      string
		committed bool
	}{{"working tree", false}, {"committed", true}} {
		t.Run(test.name, func(t *testing.T) {
			root, _, _ := changedCatalog(t)
			repo, err := gogit.PlainOpen(root)
			if err != nil {
				t.Fatal(err)
			}
			base, err := repo.Head()
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, "software", "other.pkg"), testPackage(t, "com.example.edited"))
			if test.committed {
				commit(t, root)
			}
			report, err := Run(t.Context(), Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: t.TempDir(), Method: "prepare", ChangedSince: base.Hash().String()})
			if err == nil || !strings.Contains(err.Error(), "local input content changed") || !slices.Equal(preparedKeys(report), []string{"MacSoftware/other"}) {
				t.Fatalf("edited input bypassed frozen preparation: %v, prepared %v", err, preparedKeys(report))
			}
		})
	}
}

func TestChangedSincePropagatesEditedBuildInputs(t *testing.T) {
	root, update, prepare := changedCatalog(t)
	document := strings.Replace(changedResources["build.yaml"], "  payload:", "  inputs:\n    script: {path: postinstall}\n  scripts:\n    postinstall: {$input: script}\n  payload:", 1)
	writeFileText(t, filepath.Join(root, "software", "build.yaml"), document)
	script := filepath.Join(root, "software", "postinstall")
	writeFileText(t, script, "#!/bin/sh\necho original\n")
	if _, err := update(); err != nil {
		t.Fatal(err)
	}
	commit(t, root)
	writeFileText(t, script, "#!/bin/sh\necho edited\n")
	report, err := prepare()
	if err == nil || !strings.Contains(err.Error(), "local input content changed") || !slices.Equal(preparedKeys(report), []string{"BuildMacPkg/build", "MacSoftware/consumer"}) {
		t.Fatalf("edited script bypassed its build and consumer: %v, prepared %v", err, preparedKeys(report))
	}
}

func TestChangedSinceReadsEnvironmentOnlyForWhatItPrepares(t *testing.T) {
	fixture, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(fixture) }))
	defer server.Close()
	root, update, prepare := changedCatalog(t)
	private := `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: private}
spec:
  extends: app
  source: {url: "{{ env.STEMMA_TEST_PRIVATE_URL }}"}
`
	writeFileText(t, filepath.Join(root, "software", "private.yaml"), private)
	privateLocal := strings.ReplaceAll(private, "name: private", "name: private-local")
	privateLocal = strings.Replace(privateLocal, `url: "{{ env.STEMMA_TEST_PRIVATE_URL }}"`, `path: "{{ env.STEMMA_TEST_PRIVATE_PATH }}"`, 1)
	writeFileText(t, filepath.Join(root, "software", "private-local.yaml"), privateLocal)
	local := filepath.Join(t.TempDir(), "private.pkg")
	writeFile(t, local, fixture)
	t.Setenv("STEMMA_TEST_PRIVATE_URL", server.URL+"/private.pkg")
	t.Setenv("STEMMA_TEST_PRIVATE_PATH", local)
	if _, err := update(); err != nil {
		t.Fatal(err)
	}
	commit(t, root)
	if err := os.Unsetenv("STEMMA_TEST_PRIVATE_URL"); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv("STEMMA_TEST_PRIVATE_PATH"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(local); err != nil {
		t.Fatal(err)
	}
	// An unrelated change prepares without the unchanged resource's value.
	writeFileText(t, filepath.Join(root, "stemma.yaml"), strings.Replace(changedProject, "com.example.build", "com.example.renamed", 1))
	report, err := prepare()
	if got := preparedKeys(report); err != nil || !slices.Equal(got, []string{"BuildMacPkg/build", "MacSoftware/consumer"}) {
		t.Fatalf("unrelated change prepared %v: %v", got, err)
	}
	writeFileText(t, filepath.Join(root, "software", "private.yaml"), strings.Replace(private, "extends: app", "extends: app\n  application: {version_key: CFBundleVersion}", 1))
	if _, err := prepare(); err == nil || !strings.Contains(err.Error(), "required reference is missing") {
		t.Fatalf("a changed resource prepared without its environment value: %v", err)
	}
}

func TestChangedSinceRefusesWhatItCannotCompare(t *testing.T) {
	t.Run("unreadable catalog", func(t *testing.T) {
		root, _, prepare := changedCatalog(t)
		filename := filepath.Join(root, "stemma.lock.yaml")
		reviewed, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filename, []byte("version: 2\ninputs: {}\n"))
		commit(t, root)
		writeFile(t, filename, reviewed)
		if _, err := prepare(); err == nil || !strings.Contains(err.Error(), "verify this change with an explicit preparation run") {
			t.Fatalf("an unreadable catalog was compared: %v", err)
		}
	})
}

func TestChangedSinceSelectsForPrepareAlone(t *testing.T) {
	root, _, _ := changedCatalog(t)
	for _, opts := range []Options{
		{Method: "plan", ChangedSince: "HEAD"},
		{Method: "prepare", ChangedSince: "HEAD", Resources: []string{"MacSoftware/app"}},
	} {
		opts.ConfigPath, opts.CacheDir = filepath.Join(root, "stemma.yaml"), t.TempDir()
		if _, err := Run(t.Context(), opts); err == nil || !strings.Contains(err.Error(), "changed-since") {
			t.Fatalf("%s with %v: %v", opts.Method, opts.Resources, err)
		}
	}
}

func TestChangedSinceComparesTheSelectedProfile(t *testing.T) {
	root, _, prepare := changedCatalog(t)
	cache := t.TempDir()
	run := func(method, changedSince string, profiles ...string) (Report, error) {
		return Run(t.Context(), Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: cache, Method: method, ChangedSince: changedSince, Profiles: profiles})
	}
	document := filepath.Join(root, "software", "captured.yaml")
	captured := strings.Replace(strings.ReplaceAll(changedResources["other.yaml"], "other", "captured"), "spec:", "profiles: [capture]\nspec:", 1)
	writeFileText(t, document, captured)
	// A profiled resource needs lock entries only where its profile runs.
	if report, err := prepare(); err != nil || len(report.Resources) != 0 {
		t.Fatalf("a run without the profile compared a profiled resource: %v, prepared %v", err, preparedKeys(report))
	}
	if _, err := run("prepare", "HEAD", "capture"); err == nil || !strings.Contains(err.Error(), "lockfile: stemma/v1alpha1/MacSoftware/captured input source is missing; run stemma update") {
		t.Fatalf("the profile's run accepted an unlocked resource: %v", err)
	}
	writeFile(t, filepath.Join(root, "software", "captured.pkg"), testPackage(t, "com.example.captured"))
	if _, err := run("update", "", "capture"); err != nil {
		t.Fatal(err)
	}
	if report, err := run("prepare", "HEAD", "capture"); err != nil || !slices.Equal(preparedKeys(report), []string{"MacSoftware/captured"}) {
		t.Fatalf("the profile's run prepared %v: %v", preparedKeys(report), err)
	}
	commit(t, root)
	// A change outside the profile belongs to the run that takes it.
	writeFileText(t, filepath.Join(root, "stemma.yaml"), strings.Replace(changedProject, "com.example.build", "com.example.renamed", 1))
	if report, err := run("prepare", "HEAD", "capture"); err != nil || len(report.Resources) != 0 {
		t.Fatalf("the profile's run prepared %v: %v", preparedKeys(report), err)
	}
	if report, err := prepare(); err != nil || !slices.Equal(preparedKeys(report), []string{"BuildMacPkg/build", "MacSoftware/consumer"}) {
		t.Fatalf("a run without the profile prepared %v: %v", preparedKeys(report), err)
	}
	commit(t, root)
	// Leaving the profile brings an unchanged resource into runs that never
	// prepared it.
	writeFileText(t, document, strings.Replace(captured, "profiles: [capture]\n", "", 1))
	if report, err := prepare(); err != nil || !slices.Equal(preparedKeys(report), []string{"MacSoftware/captured"}) {
		t.Fatalf("a resource that left its profile prepared %v: %v", preparedKeys(report), err)
	}
}
