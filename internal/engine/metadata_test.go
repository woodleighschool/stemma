package engine

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

func TestMetadataExpressionsPreserveNativeValuesAndOrigins(t *testing.T) {
	software := plugin.ResourceResult{Subjects: map[string]plugin.SubjectSelector{"application": {Kind: "app"}}}
	facts := plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{
		{ID: "bundle", Kind: "app", App: &plugin.AppFacts{Version: "2.4"}},
		{ID: "package", Kind: "package", Package: &plugin.PackageFacts{InstalledSize: 2048, HasPayload: false}},
	}}
	native := map[string]any{"pkginfo": map[string]any{
		"version": "{{ facts.application.app.version }}", "description": "Release {{ facts.application.app.version }}",
		"installed_size": "{{ facts.package.package.installed_size }}", "enabled": "{{ !facts.package.package.has_payload }}",
		"catalogs": "{{ ['release', 'preview'] }}", "optional": "{{ null }}", "literal": false,
		"settings": "{{ {'enabled': true, 'count': 2} }}",
	}}
	resolved, origins, err := resolveMetadata(software, native, facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"pkginfo":{"type":"object","required":["version","description","installed_size","enabled","catalogs","optional","literal","settings"],"properties":{"version":{"const":"2.4"},"description":{"const":"Release 2.4"},"installed_size":{"type":"integer","const":2048},"enabled":{"const":true},"catalogs":{"type":"array","items":{"type":"string"}},"optional":{"type":"null"},"literal":{"const":false},"settings":{"type":"object","properties":{"enabled":{"const":true},"count":{"type":"integer"}}}}}}}`)
	if err := plugin.ValidateSchema(schema, data); err != nil {
		t.Fatalf("metadata did not retain native values: %v", err)
	}
	for _, field := range []string{"version", "description", "installed_size", "enabled", "catalogs", "optional", "settings.enabled", "settings.count"} {
		if origins["pkginfo."+field] != "expression" {
			t.Fatalf("field %s origin: %q", field, origins["pkginfo."+field])
		}
	}
	if origins["pkginfo.literal"] != "explicit" || native["pkginfo"].(map[string]any)["version"] != "{{ facts.application.app.version }}" {
		t.Fatal("literal provenance or authored metadata changed")
	}
}

func TestMetadataEnvironmentExpressionsDoNotRequireArtifactSubjects(t *testing.T) {
	t.Setenv("STEMMA_METADATA_TEXT", "{{ literal output }}")
	software := plugin.ResourceResult{Subjects: map[string]plugin.SubjectSelector{"application": {Kind: "app"}}}
	metadata, origins, err := resolveMetadata(software, map[string]any{"name": "{{ env.STEMMA_METADATA_TEXT }}", "escaped": `\{{ untouched }}`}, plugin.Facts{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if metadata["name"] != "{{ literal output }}" || metadata["escaped"] != "{{ untouched }}" || origins["name"] != "expression" || origins["escaped"] != "explicit" {
		t.Fatalf("environment and escaped text were not rendered once: %#v, %#v", metadata, origins)
	}
}

func TestMetadataExpressionsCannotChangePublicationGraph(t *testing.T) {
	reference := map[string]any{"resource": map[string]any{"kind": "MacSoftware", "name": "peer"}}
	for name, test := range map[string]struct {
		metadata map[string]any
		valid    bool
	}{
		"declared reference": {map[string]any{"requires": []any{reference}, "name": "{{ 'Consumer' }}"}, true},
		"injected object":    {map[string]any{"requires": []any{"{{ {'resource': {'kind': 'MacSoftware', 'name': 'peer'}} }}"}}, false},
		"injected list":      {map[string]any{"requires": "{{ [{'resource': {'kind': 'MacSoftware', 'name': 'peer'}}] }}"}, false},
		"computed reference": {map[string]any{"requires": []any{map[string]any{"resource": map[string]any{"kind": "MacSoftware", "name": "{{ 'peer' }}"}}}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := resolveMetadata(plugin.ResourceResult{}, test.metadata, plugin.Facts{}, nil)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, expected %v: %v", err == nil, test.valid, err)
			}
		})
	}
}

func TestPeerMetadataUsesPreparedDestinationOutputAndNativeSchema(t *testing.T) {
	const peer = "stemma/v1alpha1/MacSoftware/peer"
	plans := map[string]resourcePlan{peer: {ResourceResult: plugin.ResourceResult{
		Subjects:     map[string]plugin.SubjectSelector{"application": {Kind: "app"}},
		Destinations: map[string]map[string]any{"repo": {"installer": "selected", "name": "{{ facts.application.app.name }}", "enabled": "{{ evidence.release.enabled }}"}},
	}}}
	prepared := map[string]preparedResource{peer: {ready: true, outputs: map[string]Prepared{"selected": {
		SuppliedFacts: true,
		Facts:         plugin.Facts{Subjects: []plugin.Subject{{ID: "app", Kind: "app", App: &plugin.AppFacts{Name: "Prepared Peer"}}}},
		Evidence:      map[string]json.RawMessage{"release": json.RawMessage(`{"enabled":true}`)},
	}}}}
	peers := map[string]json.RawMessage{peer: nil}
	schema := json.RawMessage(`{"type":"object","additionalProperties":false,"required":["name","enabled"],"properties":{"name":{"type":"string"},"enabled":{"type":"boolean"}}}`)
	resolved, err := resolvePeerMetadata(t.Context(), plans, "repo", peers, prepared, schema)
	if err != nil {
		t.Fatal(err)
	}
	if string(resolved[peer]) != `{"enabled":true,"name":"Prepared Peer"}` {
		t.Fatalf("peer expressions were not resolved from the selected output: %s", resolved[peer])
	}
	if _, err := resolvePeerMetadata(t.Context(), plans, "repo", peers, nil, schema); err == nil || !strings.Contains(err.Error(), "requires preparation") {
		t.Fatalf("unprepared peer's facts were accepted: %v", err)
	}
	plans[peer].Destinations["repo"]["enabled"] = "{{ 'yes' }}"
	if _, err := resolvePeerMetadata(t.Context(), plans, "repo", peers, prepared, schema); err == nil {
		t.Fatal("resolved peer bypassed its native schema")
	}
}

func TestDestinationPreflightResolvesUnselectedPeersBeforeProviderValidation(t *testing.T) {
	t.Setenv("STEMMA_PEER_NAME", "Environment Peer")
	const consumer = "stemma/v1alpha1/MacSoftware/consumer"
	const peer = "stemma/v1alpha1/MacSoftware/peer"
	consumerMetadata := map[string]any{"pkginfo": map[string]any{
		"installer_type": "nopkg", "version": "1",
		"requires": []any{map[string]any{"resource": map[string]any{"kind": "MacSoftware", "name": "peer"}}},
	}}
	peerMetadata := map[string]any{"pkginfo": map[string]any{"installer_type": "nopkg", "version": "1", "name": "{{ env.STEMMA_PEER_NAME }}"}}
	plans := map[string]resourcePlan{
		consumer: {Resource: config.Resource{Kind: "MacSoftware", Metadata: config.Metadata{Name: "consumer"}}, ResourceResult: plugin.ResourceResult{Destinations: map[string]map[string]any{"repo": consumerMetadata}}},
		peer:     {Resource: config.Resource{Kind: "MacSoftware", Metadata: config.Metadata{Name: "peer"}}, ResourceResult: plugin.ResourceResult{Subjects: map[string]plugin.SubjectSelector{"application": {Kind: "app"}}, Destinations: map[string]map[string]any{"repo": peerMetadata}}},
	}
	project := config.Project{Project: "fixture", Destinations: map[string]config.Destination{"repo": {Operation: "munki", Config: map[string]any{"path": "repo"}}}}
	calls := 0
	ops, err := builtins(map[string]reconcileHandler{"munki": func(_ context.Context, request plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		calls++
		if strings.Contains(string(request.Peers[peer]), "{{") || !strings.Contains(string(request.Peers[peer]), "Environment Peer") {
			t.Fatalf("provider received authored peer metadata: %s", request.Peers[peer])
		}
		return plugin.ReconcileResponse{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planDestinations(t.Context(), project, plans, ops, t.TempDir(), []string{consumer}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("provider validations: %d", calls)
	}
	plans[peer].Destinations["repo"]["pkginfo"].(map[string]any)["name"] = "{{ facts.application.app.name }}"
	if _, err := planDestinations(t.Context(), project, plans, ops, t.TempDir(), []string{consumer}); err == nil || !strings.Contains(err.Error(), "requires preparation") {
		t.Fatalf("unselected peer's required facts were deferred: %v", err)
	}
	calls = 0
	if _, err := planDestinations(t.Context(), project, plans, ops, t.TempDir(), []string{consumer, peer}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("provider was called before selected peer facts were available")
	}
}

func TestResolvedExpressionMustSatisfyProviderSchema(t *testing.T) {
	called := false
	ops, err := builtins(map[string]reconcileHandler{"munki": func(_ context.Context, _ plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		called = true
		return plugin.ReconcileResponse{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	facts := plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: "installer", Kind: "package", Package: &plugin.PackageFacts{Version: "2.3"}}}}
	metadata, _, err := resolveMetadata(plugin.ResourceResult{}, map[string]any{"pkginfo": map[string]any{"unattended_install": "{{ facts.installer.package.version }}"}}, facts, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	request := plugin.ReconcileRequest[json.RawMessage]{Identity: plugin.Identity{Project: "fixture", Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "fixture"}, Destination: "repo"}, Config: json.RawMessage(`{"path":"repo"}`), Metadata: data, Artifact: plugin.Artifact{Facts: facts}, Prepared: true}
	err = ops.call(t.Context(), "munki", "validate", request, nil)
	if err == nil || called || !strings.Contains(err.Error(), "boolean") {
		t.Fatalf("resolved string reached boolean provider field: called=%v err=%v", called, err)
	}
}
