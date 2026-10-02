package source

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

func TestUnchangedSourcesRefreshPayloadRecency(t *testing.T) {
	for _, kind := range []string{"http304", "immutable", "declared digest"} {
		t.Run(kind, func(t *testing.T) {
			server := newConditionalServer(t)
			m := manager(t)
			input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg"}}
			locked, err := m.Resolve(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "http304" {
				resolver := m.resolvers["http"]
				resolver.Discover = func(context.Context, plugin.Input) (Discovery, error) {
					found := Discovery{Observation: json.RawMessage(`{"url":"https://example.test/app.pkg"}`), Immutable: true}
					if kind == "declared digest" {
						found.Content = &locked.Content
					}
					return found, nil
				}
				m.resolvers["fixture.source"] = resolver
				input.Resolver = "fixture.source"
				// Exercise the locked-identity reuse branch without acquiring an input.
				version, declaration, err := m.Declaration(input)
				if err != nil {
					t.Fatal(err)
				}
				locked.Resolver, locked.ResolverVersion, locked.Declaration = input.Resolver, version, declaration
				locked.Observation = json.RawMessage(`{"url":"https://example.test/app.pkg"}`)
			}
			old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
			marker := filepath.Join(m.Store.Dir, "uses", "objects-"+locked.Content.SHA256)
			if err := os.Chtimes(marker, old, old); err != nil {
				t.Fatal(err)
			}
			current, cached, err := m.Refresh(t.Context(), input, locked)
			if err != nil || !cached || current.Content.SHA256 != locked.Content.SHA256 {
				t.Fatalf("reuse: %+v / %v / %v", current, cached, err)
			}
			// Another resource adds enough content for completion to exceed the budget.
			if _, err := m.Store.Import(t.Context(), strings.NewReader("new payload"), ""); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Store.Prune(t.Context(), cas.Policy{MaxSize: 1}, cas.PruneOptions{}); err != nil {
				t.Fatal(err)
			}
			if !m.Store.HasDigest(locked.Content.SHA256) {
				t.Fatal("prune removed a source just reported cached")
			}
		})
	}
}
