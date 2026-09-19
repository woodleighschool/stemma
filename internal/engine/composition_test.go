package engine

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func TestDestinationReferencesStayOnTheirConnection(t *testing.T) {
	metadata := func(dependency string) map[string]any {
		value := map[string]any{"type": "win32"}
		if dependency != "" {
			value["dependencies"] = []any{map[string]any{"software": dependency, "auto_install": true}}
		}
		return value
	}
	project := config.Project{Project: "fixture", Destinations: map[string]config.Destination{
		"one": {Operation: "intune", Config: map[string]any{"token": "synthetic"}},
		"two": {Operation: "intune", Config: map[string]any{"token": "synthetic"}},
	}}
	plans := map[string]resourcePlan{
		"a": {Resource: config.Resource{Metadata: config.Metadata{Name: "a"}}, ResourceResult: plugin.ResourceResult{Destinations: map[string]map[string]any{"one": metadata("b"), "two": metadata("")}}},
		"b": {Resource: config.Resource{Metadata: config.Metadata{Name: "b"}}, ResourceResult: plugin.ResourceResult{Destinations: map[string]map[string]any{"one": metadata(""), "two": metadata("a")}}},
	}

	operations, err := builtins(nil)
	if err != nil {
		t.Fatal(err)
	}
	destinations, err := planDestinations(t.Context(), project, plans, operations, t.TempDir(), []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if string(destinations[destinationRef{"a", "one"}].peers["b"]) != `{"type":"win32"}` || string(destinations[destinationRef{"b", "two"}].peers["a"]) != `{"type":"win32"}` {
		t.Fatalf("peer metadata: %v", destinations)
	}
	for _, pair := range [][2]destinationRef{{{"a", "one"}, {"b", "one"}}, {{"b", "two"}, {"a", "two"}}} {
		if !slices.Equal(destinations[pair[0]].requires, []destinationRef{pair[1]}) {
			t.Fatalf("dependencies for %v: %v", pair[0], destinations[pair[0]].requires)
		}
	}
	// An unselected peer supplies metadata but adds no dependency to the run.
	destinations, err = planDestinations(t.Context(), project, plans, operations, t.TempDir(), []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	plan := destinations[destinationRef{"a", "one"}]
	if string(plan.peers["b"]) != `{"type":"win32"}` || len(plan.requires) != 0 {
		t.Fatalf("unselected peer: %+v", plan)
	}
}

func TestConnectionIdentityFollowsCredentialSchemaReferences(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(`{"$ref":"#/$defs/connection","$defs":{"connection":{"allOf":[{"properties":{"url":{"type":"string"},"accounts":{"type":"array","items":{"$ref":"#/$defs/account"}}}}]},"account":{"properties":{"id":{"type":"string"},"secret":{"writeOnly":true}}}}}`), &schema); err != nil {
		t.Fatal(err)
	}
	identity := func(secret, url string) string {
		value, _ := connectionIdentity(map[string]any{"url": url, "accounts": []any{map[string]any{"id": "fixture", "secret": secret}}}, []map[string]any{schema}, schema)
		return config.Fingerprint(value)
	}
	if identity("first", "a") != identity("rotated", "a") {
		t.Fatal("credential rotation changed connection identity")
	}
	if identity("first", "a") == identity("first", "b") {
		t.Fatal("changed connection reused old connection identity")
	}
}
