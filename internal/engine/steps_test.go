package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

func TestStepOutputsCacheAndInputOwnership(t *testing.T) {
	for _, mode := range []string{"copy", "mutate", "outside", "wrong-digest", "unsafe-name"} {
		t.Run(mode, func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			inputPath := filepath.Join(t.TempDir(), "payload.bin")
			if err := os.WriteFile(inputPath, []byte("fixture bytes"), 0o600); err != nil {
				t.Fatal(err)
			}
			ref, err := store.ImportFile(t.Context(), inputPath, "")
			if err != nil {
				t.Fatal(err)
			}
			entry := source.Entry{Artifact: ref, Filename: "payload.bin", ResolvedAt: time.Unix(12345, 0).UTC()}
			input := Prepared{Source: entry, Payload: ref, Filename: entry.Filename, Format: "bin", Path: inputPath}
			ops, err := builtins(nil)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			op := plugin.Operation{Name: "fixture.copy", Kind: "transform", SideEffects: "workspace", Methods: []string{"validate", "run"}, InputSchema: operationSchema(plugin.StepRequest{}), OutputSchema: operationSchema(plugin.StepResponse{})}
			err = ops.registry.Register(op, func(_ context.Context, envelope plugin.Request) (plugin.Response, error) {
				if envelope.Method == "validate" {
					return plugin.Response{}, nil
				}
				calls++
				var request plugin.StepRequest
				if err := json.Unmarshal(envelope.Input, &request); err != nil {
					return plugin.Response{}, err
				}
				in := request.Inputs["input"]
				if mode == "mutate" {
					if err := os.WriteFile(in.Path, []byte("mutated"), 0o600); err != nil {
						return plugin.Response{}, err
					}
				}
				data, err := os.ReadFile(in.Path)
				if err != nil {
					return plugin.Response{}, err
				}
				out := filepath.Join(request.Workspace, "output.bin")
				if mode == "outside" {
					out = inputPath
				}
				if err := os.WriteFile(out, data, 0o600); err != nil {
					return plugin.Response{}, err
				}
				artifact := plugin.Artifact{Path: out, Filename: "output.envelope", Format: "fixture-envelope"}
				if mode == "wrong-digest" {
					artifact.SHA256 = strings.Repeat("a", 64)
				}
				name := "output"
				if mode == "unsafe-name" {
					name = "../escaped"
				}
				result, err := json.Marshal(plugin.StepResponse{Artifacts: map[string]plugin.Artifact{name: artifact}})
				return plugin.Response{Output: result}, err
			})
			if err != nil {
				t.Fatal(err)
			}
			ops.identity[op.Name] = "fixture/1"
			step := config.Step{Name: "copy", Operation: op.Name}
			result, err := runStep(t.Context(), store, ops, step, map[string]Prepared{"input": input}, entry, t.TempDir())
			if mode != "copy" {
				if err == nil {
					t.Fatal("invalid operation output succeeded")
				}
				if err := store.Verify(t.Context(), ref); err != nil {
					t.Fatalf("operation corrupted cached source: %v", err)
				}
				return
			}
			if err != nil || result.Cached || result.Artifacts["output"].Payload != ref {
				t.Fatalf("first output: %+v error=%v", result, err)
			}
			second, err := runStep(t.Context(), store, ops, step, map[string]Prepared{"input": input}, entry, t.TempDir())
			if err != nil || !second.Cached || calls != 1 {
				t.Fatalf("cache: %+v error=%v calls=%d", second, err, calls)
			}
			for _, output := range []Prepared{result.Artifacts["output"], second.Artifacts["output"]} {
				if output.Format != "fixture-envelope" || output.artifact().Format != "fixture-envelope" {
					t.Fatalf("output format was lost: %+v", output)
				}
			}
			if _, err := os.Stat(second.Artifacts["output"].Path); err != nil {
				t.Fatal("cached output was not leased")
			}
			ops.identity[op.Name] = "fixture/2"
			third, err := runStep(t.Context(), store, ops, step, map[string]Prepared{"input": input}, entry, t.TempDir())
			if err != nil || third.Cached || calls != 2 {
				t.Fatalf("implementation identity failed to invalidate: %+v error=%v", third, err)
			}
			other := op
			other.Name = "fixture.other"
			if err := ops.registry.Register(other, func(_ context.Context, envelope plugin.Request) (plugin.Response, error) {
				if envelope.Method == "validate" {
					return plugin.Response{}, nil
				}
				var request plugin.StepRequest
				if err := json.Unmarshal(envelope.Input, &request); err != nil {
					return plugin.Response{}, err
				}
				path := filepath.Join(request.Workspace, "different.bin")
				if err := os.WriteFile(path, []byte("different operation"), 0o600); err != nil {
					return plugin.Response{}, err
				}
				data, err := json.Marshal(plugin.StepResponse{Artifacts: map[string]plugin.Artifact{"output": {Path: path}}})
				return plugin.Response{Output: data}, err
			}); err != nil {
				t.Fatal(err)
			}
			ops.identity[other.Name] = ops.identity[op.Name]
			step.Operation = other.Name
			fourth, err := runStep(t.Context(), store, ops, step, map[string]Prepared{"input": input}, entry, t.TempDir())
			if err != nil || fourth.Cached || fourth.Artifacts["output"].Payload == third.Artifacts["output"].Payload {
				t.Fatalf("different operations from same provider shared cache: %+v %v", fourth, err)
			}
		})
	}
}

func TestOperationPreflightRejectsRemotePreparation(t *testing.T) {
	ops, err := builtins(nil)
	if err != nil {
		t.Fatal(err)
	}
	op := plugin.Operation{Name: "fixture.write", Kind: "processor", SideEffects: "remote", Methods: []string{"validate", "run"}, InputSchema: json.RawMessage(`true`), OutputSchema: json.RawMessage(`true`)}
	if err := ops.registry.Register(op, func(context.Context, plugin.Request) (plugin.Response, error) {
		t.Fatal("preflight executed handler")
		return plugin.Response{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := ops.check(op.Name, true); err == nil {
		t.Fatal("preparation accepted remote effects")
	}
	if err := ops.registry.Register(op, func(context.Context, plugin.Request) (plugin.Response, error) { return plugin.Response{}, nil }); err == nil {
		t.Fatal("duplicate operation replaced existing handler")
	}
}
