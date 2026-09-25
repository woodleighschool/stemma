package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestReportRetainsIndependentDestinationResults(t *testing.T) {
	report := engine.Report{Resources: []engine.ResourceReport{
		{Name: "missing", Error: "source unavailable"},
		{
			Name: "Example", Error: "one destination failed", Kind: "MacSoftware",
			Destinations: []engine.DestinationReport{
				{Name: "unavailable", Error: "remote unavailable"},
				{Name: "local", Applied: true},
			},
		},
	}}
	var out strings.Builder
	for _, resource := range report.Resources {
		out.WriteString(renderResource(textStyle{}, "apply", resource))
	}
	for _, want := range []string{
		"missing: failed\n  error: source unavailable\n",
		"MacSoftware/Example: failed\n",
		"  unavailable: failed\n    error: remote unavailable\n",
		"  local: unchanged\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report missing %q: %s", want, out.String())
		}
	}
}

func TestIconReportNamesEachOutcome(t *testing.T) {
	report := engine.Report{Resources: []engine.ResourceReport{
		{Name: "word", Kind: "MacSoftware", Icon: "created glassy"},
		{Name: "chrome", Kind: "WindowsSoftware", Icon: "created raw"},
		{Name: "teams", Kind: "MacSoftware", Icon: "unchanged"},
		{Name: "rosetta", Kind: "MacSoftware", Icon: "no artwork"},
		{Name: "zoom", Kind: "MacSoftware", Error: "quick look icon rendering: timed out"},
	}}
	var out strings.Builder
	for _, resource := range report.Resources {
		out.WriteString(renderResource(textStyle{}, "icon", resource))
	}
	report.Summarize("icon")
	out.WriteString(renderSummary(textStyle{}, "icon", report, nil))
	for _, want := range []string{
		"MacSoftware/word: created glassy\n",
		"WindowsSoftware/chrome: created raw\n",
		"MacSoftware/teams: unchanged\n",
		"MacSoftware/rosetta: no artwork\n",
		"MacSoftware/zoom: failed\n  error: quick look icon rendering: timed out\n",
		"Icons: 2 created, 2 unchanged, 1 failed.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report missing %q: %s", want, out.String())
		}
	}
}

func TestIconRunsShowCreatedIconsAndMissingArtwork(t *testing.T) {
	var out bytes.Buffer
	output := newCommandOutput(&out, &out)
	for _, resource := range []engine.ResourceReport{
		{Name: "word", Kind: "MacSoftware", Icon: "created glassy"},
		{Name: "chrome", Kind: "WindowsSoftware", Icon: "no artwork"},
		{Name: "teams", Kind: "MacSoftware", Icon: "unchanged"},
		{Name: "fonts", Kind: "MacSoftware", Icon: "no icon declared"},
	} {
		if err := output.resourceDone("icon", resource); err != nil {
			t.Fatal(err)
		}
	}
	if got := out.String(); !strings.Contains(got, "MacSoftware/word: created glassy\n") || !strings.Contains(got, "WindowsSoftware/chrome: no artwork\n") || strings.Contains(got, "teams") || strings.Contains(got, "fonts") {
		t.Fatalf("icon output: %s", got)
	}
}

func TestCompiledProjectLifecycle(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "stemma")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	installer, err := os.ReadFile("../../internal/apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { downloads.Add(1); _, _ = w.Write(installer) }))
	defer server.Close()
	project := t.TempDir()
	cache := t.TempDir()
	manifest := fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: test-apps
spec:
  destinations:
    first: {operation: munki, config: {path: first}}
    second: {operation: munki, config: {path: second}}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: fixture
spec:
  source: {url: %s/fixture.pkg}
  signature: {signer: apple:developer-id:SMLKBTR495}
  destinations:
    first: {pkginfo: {description: original, unattended_install: false, catalogs: [testing]}}
    second: {pkginfo: {catalogs: [testing]}}
`, server.URL)
	write := func(text string) {
		t.Helper()
		testproject.Write(t, filepath.Join(project, "stemma.yaml"), text)
	}
	write(manifest)
	invoke := func(success bool, args ...string) []byte {
		t.Helper()
		arguments := append([]string{"--root", project, "--cache-dir", cache}, args...)
		cmd := exec.CommandContext(t.Context(), binary, arguments...)
		cmd.Env = append(os.Environ(), "CI=true")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if (err == nil) != success {
			t.Fatalf("%v: err=%v stderr=%s output=%s", args, err, stderr.String(), output)
		}
		return output
	}
	run := func(success bool, args ...string) engine.Report {
		t.Helper()
		output := invoke(success, append(args, "--json", "--all")...)
		var report engine.Report
		if len(output) > 0 {
			if err := json.Unmarshal(output, &report); err != nil {
				t.Fatalf("decode %s: %v", output, err)
			}
		}
		return report
	}
	invoke(false, "schema")
	invoke(false, "schema", "-o", "-")
	if _, err := os.Stat(filepath.Join(project, "stemma.schema.json")); !os.IsNotExist(err) {
		t.Fatal("schema without --output-file wrote an implicit catalog file")
	}
	stdoutSchema := invoke(true, "schema", "--offline", "--output-file", "-")
	if !json.Valid(stdoutSchema) {
		t.Fatal("stdout schema is not JSON")
	}
	filename := filepath.Join(t.TempDir(), "editor", "schema.json")
	for range 2 {
		if output := invoke(true, "schema", "--offline", "--output-file", filename); len(output) != 0 {
			t.Fatalf("file output also wrote stdout: %s", output)
		}
		data, err := os.ReadFile(filename)
		if err != nil || !bytes.Equal(stdoutSchema, data) {
			t.Fatalf("file and stdout schemas differ: %v", err)
		}
	}
	invoke(true, "validate", "--resolved", "--offline")
	var descriptor plugin.Descriptor
	if err := json.Unmarshal(invoke(true, "operations", "--offline"), &descriptor); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateDescriptor(descriptor); err != nil {
		t.Fatal(err)
	}
	operations := map[string]string{}
	for _, operation := range descriptor.Operations {
		operations[operation.Name] = operation.Kind
	}
	if operations["software.mac"] != "resource" || operations["build.mac.pkg"] != "resource" || operations["munki"] != "reconcile" {
		t.Fatalf("missing operation roles: %v", operations)
	}
	if downloads.Load() != 0 {
		t.Fatal("validation or catalog acquired software input")
	}
	var inspected engine.Prepared
	fixture, err := filepath.Abs("../../internal/apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(invoke(true, "inspect", fixture), &inspected); err != nil {
		t.Fatal(err)
	}
	if inspected.Facts.Version != plugin.FactsVersion || len(inspected.Facts.Subjects) < 2 {
		t.Fatalf("inspection lost container/receipt facts: %+v", inspected.Facts)
	}
	if output := invoke(false, "artifact", "fixture"); len(output) != 0 || downloads.Load() != 0 {
		t.Fatalf("artifact without a lockfile printed %q or acquired input", output)
	}
	firstPrepare := run(true, "prepare")
	if firstPrepare.LockChanged == nil || !*firstPrepare.LockChanged {
		t.Fatal("first preparation did not record inputs")
	}
	secondPrepare := run(true, "prepare")
	if secondPrepare.LockChanged == nil || *secondPrepare.LockChanged {
		t.Fatal("locked preparation changed inputs")
	}
	if downloads.Load() != 1 {
		t.Fatal("unexpected acquisition count")
	}
	materialized, found := strings.CutSuffix(string(invoke(true, "artifact", "MacSoftware/fixture")), "\n")
	if !found || !strings.HasPrefix(materialized, filepath.Join(cache, "materialized")+string(filepath.Separator)) {
		t.Fatalf("artifact printed %q", materialized)
	}
	if err := json.Unmarshal(invoke(true, "inspect", materialized), &inspected); err != nil || len(inspected.Facts.Subjects) < 2 || downloads.Load() != 1 {
		t.Fatalf("inspecting the artifact: %+v %v", inspected.Facts, err)
	}
	derived := run(true, "signature")
	var signer struct {
		Signer string `json:"signer"`
	}
	if len(derived.Resources) != 1 || json.Unmarshal(derived.Resources[0].Artifacts["installer"].Evidence["signature"], &signer) != nil || signer.Signer != "apple:developer-id:SMLKBTR495" {
		t.Fatalf("signature derivation: %+v", derived.Resources)
	}
	plan := run(true, "plan")
	if len(plan.Resources) != 1 || len(plan.Resources[0].Destinations) != 2 {
		t.Fatalf("incomplete plan: %#v", plan)
	}
	for _, name := range []string{"first", "second"} {
		if _, err := os.Stat(filepath.Join(project, name)); !os.IsNotExist(err) {
			t.Fatal("plan mutated destination")
		}
	}
	applied := run(true, "apply")
	for _, destination := range applied.Resources[0].Destinations {
		if !destination.Applied {
			t.Fatal("destination not applied")
		}
	}
	warm := run(true, "apply", "--offline")
	if !warm.Resources[0].Cached || downloads.Load() != 1 {
		t.Fatal("warm run repeated preparation or acquisition")
	}
	for _, destination := range warm.Resources[0].Destinations {
		if len(destination.Changes) != 0 {
			t.Fatalf("unchanged run made changes: %#v", destination.Changes)
		}
	}
	write(strings.Replace(manifest, "description: original", "description: edited", 1))
	metadata := run(true, "apply")
	if !metadata.Resources[0].Cached || downloads.Load() != 1 {
		t.Fatal("metadata change invalidated preparation")
	}
	for _, destination := range metadata.Resources[0].Destinations {
		for _, change := range destination.Changes {
			if change.Kind == "content" {
				t.Fatal("metadata edit uploaded content")
			}
		}
	}
	if err := os.RemoveAll(cache); err != nil {
		t.Fatal(err)
	}
	cold := run(true, "apply")
	for _, destination := range cold.Resources[0].Destinations {
		if len(destination.Changes) != 0 {
			t.Fatalf("cold cache replayed publication: %#v", destination.Changes)
		}
	}
	if downloads.Load() != 2 {
		t.Fatal("cold cache did not reacquire once")
	}
	// A runner keeps nothing about a destination: the repository identifies its
	// own publications, so a fresh checkout converges without replaying them.
	if err := os.RemoveAll(filepath.Join(project, ".stemma")); err != nil {
		t.Fatal(err)
	}
	fresh := run(true, "apply")
	for _, destination := range fresh.Resources[0].Destinations {
		if !destination.Applied || len(destination.Changes) != 0 {
			t.Fatalf("a runner without local state replayed publication: %+v", destination)
		}
	}
	materialized = strings.TrimSpace(string(invoke(true, "artifact", "--offline", "MacSoftware/fixture")))
	lockPath := filepath.Join(project, "stemma.lock.yaml")
	reviewed, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// The source still serves the reviewed bytes, so both runs name one copy.
	if unlocked := strings.TrimSpace(string(invoke(true, "artifact", "--no-input-lock", "MacSoftware/fixture"))); unlocked != materialized {
		t.Fatalf("artifact --no-input-lock printed %s, want %s", unlocked, materialized)
	}
	if current, err := os.ReadFile(lockPath); err != nil || !bytes.Equal(current, reviewed) {
		t.Fatalf("artifact --no-input-lock changed the lockfile: %v", err)
	}
	invoke(false, "artifact", "--offline", "--no-input-lock", "MacSoftware/fixture")
	invoke(true, "cache", "prune")
	if _, err := os.Stat(materialized); !os.IsNotExist(err) {
		t.Fatalf("cache prune kept %s: %v", materialized, err)
	}
}

func TestBuiltinSchemaWithoutProject(t *testing.T) {
	t.Chdir(t.TempDir())
	var out, stderr bytes.Buffer
	cmd, finish := command(&out, &stderr)
	cmd.SetArgs([]string{"schema", "--builtins", "--output-file", "-"})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
	if err != nil {
		t.Fatalf("schema without a project: %v; %s", err, stderr.String())
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("schema output is not JSON: %s", out.String())
	}
}

func TestUpdateWritesSuccessfulLocksAndReportsFailures(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprintf("json=%t", asJSON), func(t *testing.T) {
			var refresh atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if refresh.Load() && r.URL.Path == "/broken.pkg" {
					http.NotFound(w, r)
					return
				}
				_, _ = fmt.Fprintf(w, "release refreshed=%t", refresh.Load())
			}))
			t.Cleanup(server.Close)
			project := t.TempDir()
			filename := filepath.Join(project, "stemma.yaml")
			testproject.Write(t, filename, fmt.Sprintf(`apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: failures}
spec: {imports: ['*.software.yaml']}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: broken}
spec:
  source: {url: %s/broken.pkg}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: consumer}
spec:
  source: {resource: {kind: MacSoftware, name: broken}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: healthy}
spec:
  source: {url: %s/healthy.pkg}
`, server.URL, server.URL))
			cache := t.TempDir()
			if _, err := engine.Run(t.Context(), engine.Options{ConfigPath: filename, CacheDir: cache, Method: "update"}); err != nil {
				t.Fatal(err)
			}
			before, err := lockfile.Load(lockfile.Filename(project))
			if err != nil {
				t.Fatal(err)
			}
			refresh.Store(true)
			var out, logs bytes.Buffer
			cmd, finish := command(&out, &logs)
			args := []string{"update", "--root", project, "--cache-dir", cache}
			if asJSON {
				args = append(args, "--json")
			}
			cmd.SetArgs(args)
			err = cmd.ExecuteContext(t.Context())
			finish(err)
			if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
				t.Fatalf("failed refresh silently reused its reviewed lock: %v", err)
			}
			after, err := lockfile.Load(lockfile.Filename(project))
			if err != nil {
				t.Fatal(err)
			}
			const broken, healthy = "stemma/v1alpha1/MacSoftware/broken", "stemma/v1alpha1/MacSoftware/healthy"
			if !before.Inputs[broken]["source"].Equal(after.Inputs[broken]["source"]) || before.Inputs[healthy]["source"].Equal(after.Inputs[healthy]["source"]) {
				t.Fatalf("partial update wrote the wrong locks: %+v", after)
			}
			if asJSON {
				var report engine.Report
				if err := json.Unmarshal(out.Bytes(), &report); err != nil {
					t.Fatal(err)
				}
				if report.Error == "" || report.LockChanged == nil || !*report.LockChanged || len(report.Resources) != 3 || report.Resources[2].Error != "" || len(report.Resources[2].Inputs) != 1 || report.Summary.InputChanges != 1 || len(report.Resources[1].BlockedBy) != 1 || report.Resources[1].BlockedBy[0] != broken {
					t.Fatalf("incomplete JSON report: %+v", report)
				}
			} else {
				for _, want := range []string{"MacSoftware/broken: failed\n", "MacSoftware/consumer: blocked\n  blocked by MacSoftware/broken\n", "MacSoftware/healthy: 1 input changed\n  source (content changed): healthy.pkg\n", "Update incomplete: 1 input change, 3 resources checked, 1 failed, 1 blocked.", "Lockfile updated."} {
					if !strings.Contains(out.String(), want) {
						t.Fatalf("report missing %q: %s", want, out.String())
					}
				}
			}
			// The report shows each failure; stderr repeats none of them.
			if logs.Len() != 0 {
				t.Fatalf("stderr repeated the report: %s", logs.String())
			}
		})
	}
}
