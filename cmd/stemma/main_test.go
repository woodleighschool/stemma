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
	var out bytes.Buffer
	for _, resource := range report.Resources {
		if err := printResource(&out, "apply", resource); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []string{
		"missing: failed",
		"source unavailable",
		"MacSoftware/Example",
		"unavailable: failed: remote unavailable",
		"local: unchanged",
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
	var out bytes.Buffer
	for _, resource := range report.Resources {
		if err := printResource(&out, "icon", resource); err != nil {
			t.Fatal(err)
		}
	}
	if err := printSummary(&out, "icon", report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"MacSoftware/word: created glassy",
		"WindowsSoftware/chrome: created raw",
		"MacSoftware/teams: unchanged",
		"MacSoftware/rosetta: no artwork",
		"MacSoftware/zoom: failed",
		"quick look icon rendering: timed out",
		"Icons: 2 created, 2 unchanged, 1 failed.",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report missing %q: %s", want, out.String())
		}
	}
}

func TestIconRunsShowCreatedIconsAndMissingArtwork(t *testing.T) {
	var out bytes.Buffer
	output := &commandOutput{}
	for _, resource := range []engine.ResourceReport{
		{Name: "word", Kind: "MacSoftware", Icon: "created glassy"},
		{Name: "chrome", Kind: "WindowsSoftware", Icon: "no artwork"},
		{Name: "teams", Kind: "MacSoftware", Icon: "unchanged"},
		{Name: "fonts", Kind: "MacSoftware", Icon: "no icon declared"},
	} {
		if err := output.resourceDone(&out, false, "icon", resource); err != nil {
			t.Fatal(err)
		}
	}
	if got := out.String(); !strings.Contains(got, "MacSoftware/word: created glassy") || !strings.Contains(got, "WindowsSoftware/chrome: no artwork") || strings.Contains(got, "teams") || strings.Contains(got, "fonts") {
		t.Fatalf("icon run output: %s", got)
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
		output := invoke(success, append(args, "--json")...)
		var report engine.Report
		if len(output) > 0 {
			if err := json.Unmarshal(output, &report); err != nil {
				t.Fatalf("decode %s: %v", output, err)
			}
		}
		return report
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
}
