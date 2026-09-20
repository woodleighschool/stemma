package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

// closureSpec declares one fixture resource: needs names the resources whose
// outputs it consumes, and invalid makes the kind reject the declaration,
// standing in for any operation-level validation failure.
type closureSpec struct {
	Needs   []string `json:"needs,omitempty"`
	Invalid bool     `json:"invalid,omitempty"`
}

func closureKey(name string) string { return "fixture/v1/Fixture/" + name }

// closureFixture builds a project of one fixture kind alongside the operations
// that evaluate it, recording how often each resource is evaluated.
func closureFixture(t *testing.T, specs map[string]closureSpec) (config.Project, *operations, map[string]int) {
	t.Helper()
	evaluated := map[string]int{}
	ops := &operations{registry: plugin.New("fixture", "1"), identity: map[string]string{}}
	operation := plugin.Operation{
		Name: "fixture", Kind: "resource", Resource: &plugin.ResourceKind{APIVersion: "fixture/v1", Kind: "Fixture"},
		SideEffects: "workspace", Methods: []string{"discover", "run"},
	}
	err := plugin.Register(ops.registry, operation, func(_ context.Context, request plugin.ResourceRequest[closureSpec]) (plugin.ResourceResult, error) {
		evaluated[request.Identity.Name]++
		if request.Config.Invalid {
			return plugin.ResourceResult{}, errors.New("operation rejected the declaration")
		}
		result := plugin.ResourceResult{Config: json.RawMessage(`{}`), Inputs: map[string]plugin.Input{}}
		for _, name := range request.Config.Needs {
			reference := plugin.ResourceReference{APIVersion: "fixture/v1", Kind: "Fixture", Name: name}
			result.Inputs[name] = plugin.Input{Resource: &plugin.ResourceOutputReference{ResourceReference: reference}}
		}
		return result, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	project := config.Project{Project: "fixture", Resources: map[string]config.Resource{}}
	for name, spec := range specs {
		declaration := map[string]any{}
		if len(spec.Needs) > 0 {
			needs := make([]any, len(spec.Needs))
			for i, need := range spec.Needs {
				needs[i] = need
			}
			declaration["needs"] = needs
		}
		if spec.Invalid {
			declaration["invalid"] = true
		}
		project.Resources[closureKey(name)] = config.Resource{
			APIVersion: "fixture/v1", Kind: "Fixture", Metadata: config.Metadata{Name: name}, Spec: declaration,
		}
	}
	return project, ops, evaluated
}

// closure resolves selectors and evaluates the resulting closure, as every run
// method does before it acquires anything.
func closure(t *testing.T, project config.Project, ops *operations, selectors ...string) ([]string, error) {
	t.Helper()
	roots, err := selectResources(project.Resources, selectors)
	if err != nil {
		return nil, err
	}
	_, selected, err := discoverClosure(t.Context(), project, ops, roots)
	return selected, err
}

func TestScopedRunEvaluatesOnlyItsDependencyClosure(t *testing.T) {
	// A depends on B, which depends on C; D and E are unrelated and broken.
	project, ops, evaluated := closureFixture(t, map[string]closureSpec{
		"a": {Needs: []string{"b"}}, "b": {Needs: []string{"c"}}, "c": {},
		"d": {Invalid: true}, "e": {Invalid: true},
	})
	selected, err := closure(t, project, ops, "Fixture/a")
	if err != nil {
		t.Fatalf("a broken unrelated resource blocked a scoped run: %v", err)
	}
	if want := []string{closureKey("c"), closureKey("b"), closureKey("a")}; !slices.Equal(selected, want) {
		t.Fatalf("closure order: %v", selected)
	}
	if len(evaluated) != 3 || evaluated["a"] != 1 || evaluated["b"] != 1 || evaluated["c"] != 1 {
		t.Fatalf("resources evaluated outside the closure: %v", evaluated)
	}
}

func TestScopedRunFailsOnAnInvalidDependency(t *testing.T) {
	project, ops, evaluated := closureFixture(t, map[string]closureSpec{
		"a": {Needs: []string{"b"}}, "b": {Invalid: true}, "c": {Invalid: true},
	})
	_, err := closure(t, project, ops, "Fixture/a")
	if err == nil || !strings.Contains(err.Error(), closureKey("b")) || !strings.Contains(err.Error(), "rejected the declaration") {
		t.Fatalf("invalid dependency did not fail the closure: %v", err)
	}
	if evaluated["c"] != 0 {
		t.Fatalf("failure reached outside the closure: %v", evaluated)
	}
}

func TestSharedDependencyIsEvaluatedOnce(t *testing.T) {
	project, ops, evaluated := closureFixture(t, map[string]closureSpec{
		"a": {Needs: []string{"b"}}, "b": {}, "c": {Needs: []string{"b"}},
	})
	selected, err := closure(t, project, ops, "Fixture/a", "Fixture/c")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{closureKey("b"), closureKey("a"), closureKey("c")}; !slices.Equal(selected, want) {
		t.Fatalf("shared dependency ordering: %v", selected)
	}
	if evaluated["b"] != 1 {
		t.Fatalf("shared dependency evaluated %d times", evaluated["b"])
	}
}

func TestDependencyCycleFailsClearly(t *testing.T) {
	project, ops, _ := closureFixture(t, map[string]closureSpec{
		"a": {Needs: []string{"b"}}, "b": {Needs: []string{"a"}},
	})
	_, err := closure(t, project, ops, "Fixture/a")
	if err == nil || !strings.Contains(err.Error(), "build input cycle at "+closureKey("a")) {
		t.Fatalf("dependency cycle: %v", err)
	}
}

func TestSelectionResolvesIdentityWithoutEvaluation(t *testing.T) {
	project, ops, evaluated := closureFixture(t, map[string]closureSpec{"a": {Invalid: true}, "b": {Invalid: true}})
	if _, err := closure(t, project, ops, "Fixture/missing"); err == nil || !strings.Contains(err.Error(), `matches 0 resources`) {
		t.Fatalf("unknown selection: %v", err)
	}
	if len(evaluated) != 0 {
		t.Fatalf("selection evaluated resources: %v", evaluated)
	}
	if _, err := closure(t, project, ops, closureKey("a")); err == nil || !strings.Contains(err.Error(), "rejected the declaration") {
		t.Fatalf("full identity selection: %v", err)
	}
}

// selectionProject publishes one resolvable vendor package beside a resource
// whose GitHub source is incomplete, so the broken resource fails its
// operation contract rather than the project's structure.
const selectionProject = `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: selection
spec:
  imports:
    - '*.software.yaml'
  destinations:
    repo:
      operation: munki
      config:
        path: repo
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: vendor
spec:
  source:
    path: vendor.pkg
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata:
  name: broken
spec:
  source:
    resolver: github
    repository: example/app
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
`

func TestBrokenResourcesBlockOnlyTheRunsThatReachThem(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vendor.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	testproject.Write(t, filename, selectionProject)
	const broken = "resource stemma/v1alpha1/MacSoftware/broken input source"
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	options.Resources = []string{"MacSoftware/vendor"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatalf("a broken unrelated resource blocked a scoped run: %v", err)
	}
	options.Resources = nil
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), broken) {
		t.Fatalf("unscoped run skipped a broken root: %v", err)
	}
	if _, err := ValidateProject(t.Context(), options); err == nil || !strings.Contains(err.Error(), broken) {
		t.Fatalf("validation skipped a broken resource: %v", err)
	}
	// Suspension keeps the broken resource out of every run that does not
	// select it, and validation still reports it.
	testproject.Write(t, filename, strings.Replace(selectionProject, "  name: broken\nspec:", "  name: broken\nsuspend: true\nspec:", 1))
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatalf("an unselected suspended resource blocked a run: %v", err)
	}
	options.Resources = []string{"MacSoftware/vendor"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatalf("an unselected suspended resource blocked a scoped run: %v", err)
	}
	options.Resources = []string{"MacSoftware/broken"}
	if _, err := Run(t.Context(), options); err == nil || !strings.Contains(err.Error(), broken) {
		t.Fatalf("explicitly selected suspended resource was not validated: %v", err)
	}
	options.Resources = nil
	if _, err := ValidateProject(t.Context(), options); err == nil || !strings.Contains(err.Error(), broken) {
		t.Fatalf("validation skipped a suspended resource: %v", err)
	}
}
