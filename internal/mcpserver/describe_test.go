package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/plugin"
)

func TestKindRequiresAnAPIVersionWhenAmbiguous(t *testing.T) {
	registry := plugin.New("fixture", "1")
	for _, version := range []string{"v1", "v2"} {
		operation := plugin.Operation{
			Name: "fixture." + version, Kind: "resource", SideEffects: "workspace",
			Resource: &plugin.ResourceKind{APIVersion: "example/" + version, Kind: "Fixture"},
			Methods:  []string{"discover", "run"},
		}
		err := plugin.Register(registry, operation, func(context.Context, struct{}) (struct{}, error) {
			return struct{}{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		contracts := engine.Contracts{Operations: registry.Descriptor().Operations}
		if got, err := kind(contracts, "example/"+version+"/Fixture"); err != nil || got.Name != operation.Name {
			t.Fatalf("qualified kind = %s, %v; want %s", got.Name, err, operation.Name)
		}
		if version == "v1" {
			if got, err := kind(contracts, "Fixture"); err != nil || got.Name != operation.Name {
				t.Fatalf("unambiguous kind = %s, %v; want %s", got.Name, err, operation.Name)
			}
		}
	}
	contracts := engine.Contracts{Operations: registry.Descriptor().Operations}
	if _, err := kind(contracts, "Fixture"); err == nil || !strings.Contains(err.Error(), "example/v1/Fixture or example/v2/Fixture") {
		t.Fatalf("ambiguous kind: %v", err)
	}
}
