package engine

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

func TestResolverAcquisitionContract(t *testing.T) {
	for _, kind := range []string{"download", "missing", "both", "discovery"} {
		t.Run(kind, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Authorization") != "Bearer runtime-only" {
					t.Error("missing runtime authentication")
				}
				_, _ = io.WriteString(w, "installer")
			}))
			defer server.Close()
			ops, err := builtins(nil)
			if err != nil {
				t.Fatal(err)
			}
			discoveries := 0
			digest := fmt.Sprintf("%x", sha256.Sum256([]byte("installer")))
			operation := plugin.Operation{Name: "fixture.release", Kind: "resolve", Resolver: &plugin.ResolverKind{Version: "1"}, Methods: []string{"validate", "discover", "run"}, SideEffects: "none"}
			err = plugin.Register(ops.registry, operation, func(_ context.Context, r plugin.ResolveRequest[struct{}]) (plugin.ResolveResponse, error) {
				download := &plugin.Download{URL: server.URL + "/app.pkg?signature=runtime-only", Headers: map[string]string{"Authorization": "Bearer runtime-only"}, Filename: "app.pkg", SHA256: digest}
				if r.Method == "discover" {
					discoveries++
					result := plugin.ResolveResponse{Observation: json.RawMessage(`{"release":"1"}`), Content: &plugin.SourceContent{SHA256: digest, Filename: "app.pkg", Mode: 0o644}}
					if kind == "discovery" {
						result.Download = download
					}
					return result, nil
				}
				result := plugin.ResolveResponse{}
				if kind != "missing" {
					result.Download = download
				}
				if kind == "both" {
					result.Artifact = plugin.Artifact{Path: "unused.pkg", Filename: "app.pkg"}
				}
				return result, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			manager := source.New(store, t.TempDir(), false)
			if err := registerResolvers(manager, ops, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			input := plugin.Input{Resolver: operation.Name, Config: map[string]any{}}
			entry, err := manager.Resolve(t.Context(), input)
			if kind == "discovery" {
				if err == nil || !strings.Contains(err.Error(), "discovery must not return") {
					t.Fatalf("discovery accepted acquisition: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			encoded, _ := json.Marshal(entry)
			if strings.Contains(string(encoded), "runtime-only") {
				t.Fatal("request credentials entered lock")
			}
			_, err = manager.FetchLocked(t.Context(), input, entry)
			if kind != "download" {
				if err == nil || !strings.Contains(err.Error(), "exactly one") || requests != 0 {
					t.Fatalf("invalid acquisition: %v requests=%d", err, requests)
				}
				return
			}
			if err != nil || discoveries != 1 || requests != 1 {
				t.Fatalf("replay: %v discoveries=%d requests=%d", err, discoveries, requests)
			}
		})
	}
}
