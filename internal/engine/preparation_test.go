package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
)

func TestParallelPreparationAcquiresSharedColdInputOnce(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_, _ = w.Write([]byte("shared payload"))
	}))
	defer server.Close()
	var manifest strings.Builder
	manifest.WriteString("apiVersion: stemma/v1alpha1\nkind: Project\nmetadata: {name: fixture}\nspec:\n  imports: ['*.software.yaml']\n")
	for _, name := range []string{"a", "b", "c"} {
		fmt.Fprintf(&manifest, `---
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata: {name: %s}
spec:
  inputs:
    data: {url: %q}
  payload:
    /Library/Example/data:
      $input: data
  package:
    identifier: org.example.%s
    version: '1.0'
`, name, server.URL+"/payload", name)
	}
	project := filepath.Join(t.TempDir(), "stemma.yaml")
	testproject.Write(t, project, manifest.String())
	options := Options{ConfigPath: project, CacheDir: t.TempDir(), Method: "update", Jobs: 2}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method, options.CacheDir = "prepare", t.TempDir()
	requests.Store(0)
	cold, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || len(cold.Resources) != 3 {
		t.Fatalf("cold acquisition: %d requests, %+v", requests.Load(), cold)
	}
	server.Close()
	options.Lock.Offline = true
	warm, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(warm.Resources) != len(cold.Resources) {
		t.Fatalf("warm resources: %+v", warm)
	}
	for i, item := range warm.Resources {
		if !item.Cached || item.Artifacts["installer"].Payload != cold.Resources[i].Artifacts["installer"].Payload {
			t.Fatalf("warm preparation rebuilt: %+v", item)
		}
	}
}

func TestParallelPreparationDependenciesAndFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started := make(chan string, 5)
	completed := make(chan string, 5)
	gates := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{})}
	var active, peak atomic.Int32
	handle := func(ctx context.Context, request plugin.ResourceRequest[struct{}]) (plugin.ResourceResult, error) {
		count := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); count > old; old = peak.Load() {
			if peak.CompareAndSwap(old, count) {
				break
			}
		}
		name := request.Identity.Name
		started <- name
		if gate := gates[name]; gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				return plugin.ResourceResult{}, ctx.Err()
			}
		}
		switch name {
		case "d":
			return plugin.ResourceResult{}, errors.New("build failed")
		case "e":
			return plugin.ResourceResult{}, errors.New("blocked consumer ran")
		case "c":
			data, err := os.ReadFile(request.Inputs["source"].Path)
			if err != nil {
				return plugin.ResourceResult{}, err
			}
			if string(data) != "a" {
				return plugin.ResourceResult{}, fmt.Errorf("producer output = %q, want a", data)
			}
		}
		path := filepath.Join(request.Workspace, name)
		if err := os.WriteFile(path, []byte(name), 0o644); err != nil {
			return plugin.ResourceResult{}, err
		}
		return plugin.ResourceResult{Artifacts: map[string]plugin.Artifact{"installer": {Path: path}}}, nil
	}
	e := preparationFixture(t, map[string]string{"a": "", "b": "", "c": "a", "d": "", "e": "d"}, handle)
	e.opts.ResourceDone = func(item ResourceReport) error { completed <- item.Name; return nil }
	done := make(chan error, 1)
	go func() { done <- e.prepareAll(ctx) }()
	for range 2 {
		select {
		case name := <-started:
			if name != "a" && name != "b" {
				t.Fatalf("started %s before a worker was free", name)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(gates["b"])
	for _, want := range []string{"b", "d", "e"} {
		select {
		case name := <-completed:
			if name != want {
				t.Fatalf("completed %s, want %s", name, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	close(gates["a"])
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() != 2 || active.Load() != 0 {
		t.Fatalf("worker bound: peak %d, active %d", peak.Load(), active.Load())
	}
	if len(e.failures) != 1 || e.report.Resources[3].Error == "" || !slices.Equal(e.report.Resources[4].BlockedBy, []string{e.selected[3]}) {
		t.Fatalf("failed branch: %+v, %v", e.report.Resources, e.failures)
	}
	for i, item := range e.report.Resources {
		if item.Key != e.selected[i] || e.prepared[item.Key].report != i {
			t.Fatalf("unstable final report: %+v", e.report.Resources)
		}
		if i < 3 && (item.Error != "" || len(item.Artifacts) != 1) {
			t.Fatalf("independent resource failed: %+v", item)
		}
	}
}

func TestParallelPreparationWaitsForWorkers(t *testing.T) {
	for _, callback := range []bool{false, true} {
		t.Run(fmt.Sprintf("callback=%t", callback), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			started := make(chan string, 2)
			release := make(chan struct{})
			var stopped atomic.Int32
			e := preparationFixture(t, map[string]string{"a": "", "b": ""}, func(ctx context.Context, request plugin.ResourceRequest[struct{}]) (plugin.ResourceResult, error) {
				defer stopped.Add(1)
				started <- request.Identity.Name
				if callback && request.Identity.Name == "a" {
					select {
					case <-release:
						return plugin.ResourceResult{}, nil
					case <-ctx.Done():
						return plugin.ResourceResult{}, ctx.Err()
					}
				}
				<-ctx.Done()
				if _, err := os.Stat(request.Workspace); err != nil {
					t.Errorf("workspace removed before worker stopped: %v", err)
				}
				return plugin.ResourceResult{}, ctx.Err()
			})
			want := context.Canceled
			if callback {
				want = errors.New("report writer failed")
				e.opts.ResourceDone = func(ResourceReport) error { return want }
			}
			done := make(chan error, 1)
			go func() { done <- e.prepareAll(ctx) }()
			for range 2 {
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			if callback {
				close(release)
			} else {
				cancel()
			}
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if stopped.Load() != 2 {
				t.Fatal("returned before every worker stopped")
			}
			entries, err := os.ReadDir(e.session.work)
			if err != nil || len(entries) != 0 {
				t.Fatalf("retained preparation workspaces: %v, %v", entries, err)
			}
		})
	}
}

func preparationFixture(t *testing.T, dependencies map[string]string, handle func(context.Context, plugin.ResourceRequest[struct{}]) (plugin.ResourceResult, error)) *execution {
	t.Helper()
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	ops := &operations{registry: plugin.New("fixture", "1")}
	if err := plugin.Register(ops.registry, resourceOperation("fixture", "Fixture"), handle); err != nil {
		t.Fatal(err)
	}
	e := &execution{opts: Options{Method: "prepare", Jobs: 2}, report: &Report{}, plans: map[string]resourcePlan{}, prepared: map[string]preparedResource{}, pending: map[string]int{}, session: &session{store: store, ops: ops, work: t.TempDir()}}
	for _, name := range sortedKeys(dependencies) {
		resource := config.Resource{APIVersion: "stemma/v1alpha1", Kind: "Fixture", Metadata: config.Metadata{Name: name}}
		key := resource.Reference().Key()
		plan := resourcePlan{Operation: "fixture", Resource: resource}
		if producer := dependencies[name]; producer != "" {
			ref := resource.Reference()
			ref.Name = producer
			plan.Inputs = map[string]plugin.Input{"source": {Resource: &plugin.ResourceOutputReference{ResourceReference: ref}}}
		}
		e.plans[key] = plan
		e.selected = append(e.selected, key)
	}
	e.locked, err = lockfile.Begin(t.Context(), root, declarations(e.plans, e.selected), nil, source.New(store, root, false), lockfile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
