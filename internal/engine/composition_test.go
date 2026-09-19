package engine

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func TestDestinationReferencesStayOnTheirConnection(t *testing.T) {
	metadata := func(dependency string) map[string]any {
		value := map[string]any{"type": "win32"}
		if dependency != "" {
			value["dependencies"] = []any{map[string]any{"resource": map[string]any{"kind": "WindowsSoftware", "name": dependency}, "auto_install": true}}
		}
		return value
	}
	project := config.Project{Project: "fixture", Destinations: map[string]config.Destination{
		"one": {Operation: "intune", Config: map[string]any{"token": "synthetic"}},
		"two": {Operation: "intune", Config: map[string]any{"token": "synthetic"}},
	}}
	a, b := "stemma/v1alpha1/WindowsSoftware/a", "stemma/v1alpha1/WindowsSoftware/b"
	plans := map[string]resourcePlan{
		a: {Resource: config.Resource{APIVersion: "stemma/v1alpha1", Kind: "WindowsSoftware", Metadata: config.Metadata{Name: "a"}}, ResourceResult: plugin.ResourceResult{Destinations: map[string]map[string]any{"one": metadata("b"), "two": metadata("")}}},
		b: {Resource: config.Resource{APIVersion: "stemma/v1alpha1", Kind: "WindowsSoftware", Metadata: config.Metadata{Name: "b"}}, ResourceResult: plugin.ResourceResult{Destinations: map[string]map[string]any{"one": metadata(""), "two": metadata("a")}}},
	}
	operations, err := builtins(nil)
	if err != nil {
		t.Fatal(err)
	}
	destinations, err := planDestinations(t.Context(), project, plans, operations, t.TempDir(), []string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if string(destinations[destinationRef{a, "one"}].peers[b]) != `{"type":"win32"}` || string(destinations[destinationRef{b, "two"}].peers[a]) != `{"type":"win32"}` {
		t.Fatalf("peer metadata: %v", destinations)
	}
	for _, pair := range [][2]destinationRef{{{a, "one"}, {b, "one"}}, {{b, "two"}, {a, "two"}}} {
		if !slices.Equal(destinations[pair[0]].requires, []destinationRef{pair[1]}) {
			t.Fatalf("dependencies for %v: %v", pair[0], destinations[pair[0]].requires)
		}
	}
	destinations, err = planDestinations(t.Context(), project, plans, operations, t.TempDir(), []string{a})
	if err != nil {
		t.Fatal(err)
	}
	plan := destinations[destinationRef{a, "one"}]
	if string(plan.peers[b]) != `{"type":"win32"}` || len(plan.requires) != 0 || len(destinations) != 2 {
		t.Fatalf("unselected peer: %+v", destinations)
	}
}

func TestPublicationReferencesValidateExactResources(t *testing.T) {
	const consumer = "stemma/v1alpha1/MacSoftware/consumer"
	const peer = "stemma/v1alpha1/MacSoftware/peer"
	for _, test := range []struct {
		name, relationship, want string
		otherDestination, cycle  bool
	}{
		{name: "resource", relationship: `{"resource":{"kind":"MacSoftware","name":"peer"}}`},
		{name: "explicit version", relationship: `{"resource":{"apiVersion":"stemma/v1alpha1","kind":"MacSoftware","name":"peer"},"version":"1.2.3"}`},
		{name: "native string", relationship: `"peer"`},
		{name: "missing resource", relationship: `{"resource":{"kind":"MacSoftware","name":"missing"}}`, want: "unknown resource"},
		{name: "different kind", relationship: `{"resource":{"kind":"WindowsSoftware","name":"peer"}}`, want: "unknown resource"},
		{name: "different API", relationship: `{"resource":{"apiVersion":"other/v1","kind":"MacSoftware","name":"peer"}}`, want: "unknown resource"},
		{name: "missing kind", relationship: `{"resource":{"name":"peer"}}`, want: "requires kind and name"},
		{name: "output forbidden", relationship: `{"resource":{"kind":"MacSoftware","name":"peer","output":"installer"}}`, want: "unknown field"},
		{name: "wrong connection", relationship: `{"resource":{"kind":"MacSoftware","name":"peer"}}`, otherDestination: true, want: "does not publish to destination"},
		{name: "self cycle", relationship: `{"resource":{"kind":"MacSoftware","name":"consumer"}}`, want: "publication reference cycle"},
		{name: "cycle through unselected peer", relationship: `{"resource":{"kind":"MacSoftware","name":"peer"}}`, cycle: true, want: "publication reference cycle"},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata := func(relationship string) map[string]any {
				var result map[string]any
				data := `{"pkginfo":{"installer_type":"nopkg","version":"1","requires":[` + relationship + `]}}`
				if err := json.Unmarshal([]byte(data), &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			plans := map[string]resourcePlan{
				consumer: {Resource: config.Resource{APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Metadata: config.Metadata{Name: "consumer"}}, ResourceResult: plugin.ResourceResult{Destinations: map[string]map[string]any{"one": metadata(test.relationship)}}},
				peer:     {Resource: config.Resource{APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Metadata: config.Metadata{Name: "peer"}}, ResourceResult: plugin.ResourceResult{Destinations: map[string]map[string]any{"one": metadata(`"External"`)}}},
			}
			if test.otherDestination {
				plans[peer].Destinations["two"] = plans[peer].Destinations["one"]
				delete(plans[peer].Destinations, "one")
			}
			if test.cycle {
				plans[peer].Destinations["one"] = metadata(`{"resource":{"kind":"MacSoftware","name":"consumer"}}`)
			}
			project := config.Project{Project: "fixture", Destinations: map[string]config.Destination{"one": {Operation: "munki", Config: map[string]any{"path": "repo"}}}}
			ops, err := builtins(nil)
			if err != nil {
				t.Fatal(err)
			}
			destinations, err := planDestinations(t.Context(), project, plans, ops, t.TempDir(), []string{consumer})
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("want %q, got %v", test.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "native string" && len(destinations[destinationRef{consumer, "one"}].peers) != 0 {
				t.Fatal("native name was interpreted as a resource")
			}
		})
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
