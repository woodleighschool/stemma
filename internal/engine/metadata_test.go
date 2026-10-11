package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
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
		t.Fatal("literal provenance or declared metadata changed")
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
			t.Fatalf("provider received declared peer metadata: %s", request.Peers[peer])
		}
		return plugin.ReconcileResponse{}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planDestinations(t.Context(), project, plans, ops, t.TempDir(), []string{consumer}, true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("provider validations: %d", calls)
	}
	calls = 0
	if destinations, err := planDestinations(t.Context(), project, plans, ops, t.TempDir(), []string{consumer}, false); err != nil || calls != 0 || !destinations[destinationRef{consumer, "repo"}].environment {
		t.Fatalf("environment peer was validated without the environment: calls=%d, %v", calls, err)
	}
	plans[peer].Destinations["repo"]["pkginfo"].(map[string]any)["name"] = "{{ facts.application.app.name }}"
	if _, err := planDestinations(t.Context(), project, plans, ops, t.TempDir(), []string{consumer}, true); err == nil || !strings.Contains(err.Error(), "requires preparation") {
		t.Fatalf("unselected peer's required facts were deferred: %v", err)
	}
	calls = 0
	if _, err := planDestinations(t.Context(), project, plans, ops, t.TempDir(), []string{consumer, peer}, true); err != nil {
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

func TestPrepareReportsWhatEachDestinationDerives(t *testing.T) {
	root := t.TempDir()
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.pkg"), installer, 0o644); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: publications}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: "{{ env.STEMMA_TEST_UNSET_REPOSITORY }}"}}
    staging: {operation: munki, config: {path: staging}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: app}
spec:
  source: {path: app.pkg}
  destinations:
    repo:
      pkginfo:
        catalogs: [testing]
        display_name: "Fixture {{ facts['PackageInfo'].package.version }}"
    staging:
      pkginfo:
        catalogs: ["{{ env.STEMMA_TEST_UNSET_CATALOG }}"]
`)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "prepare"
	report, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	publications := report.Resources[0].Publications
	if len(publications) != 2 || publications[0].Destination != "repo" || publications[1].Destination != "staging" {
		t.Fatalf("publications = %+v", publications)
	}
	var derived struct {
		Pkginfo map[string]any `json:"pkginfo"`
	}
	if err := json.Unmarshal(publications[0].Metadata, &derived); err != nil {
		t.Fatal(err)
	}
	pkginfo := derived.Pkginfo
	if pkginfo["name"] != "app" || pkginfo["version"] != "1.2.3" || pkginfo["display_name"] != "Fixture 1.2.3" || pkginfo["uninstall_method"] != "removepackages" || len(pkginfo["receipts"].([]any)) != 1 || len(pkginfo["installer_item_hash"].(string)) != 64 {
		t.Fatalf("derived pkginfo = %+v", pkginfo)
	}
	if _, named := pkginfo["installer_item_location"]; named {
		t.Fatal("reported a location the repository assigns")
	}
	for field, origin := range map[string]string{"pkginfo.catalogs": "explicit", "pkginfo.display_name": "expression", "pkginfo.name": "software.name", "pkginfo.version": "artifact.version", "pkginfo.receipts": "installer.receipts"} {
		if publications[0].Origins[field] != origin {
			t.Errorf("origin of %s = %q, want %q", field, publications[0].Origins[field], origin)
		}
	}
	if len(publications[1].Metadata) != 0 || !strings.Contains(publications[1].Deferred, "environment values") {
		t.Fatalf("metadata reading the environment = %+v", publications[1])
	}
	if _, err := os.Stat(filepath.Join(root, "staging")); !os.IsNotExist(err) {
		t.Fatalf("preparation reached a repository: %v", err)
	}
	options.Handlers = map[string]reconcileHandler{"munki": func(context.Context, plugin.ReconcileRequest[json.RawMessage]) (plugin.ReconcileResponse, error) {
		return plugin.ReconcileResponse{Origins: map[string]string{"pkginfo.name": "software.name"}}, nil
	}}
	report, err = Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	publication := report.Resources[0].Publications[0]
	if len(publication.Metadata) != 0 || publication.Deferred == "" || publication.Origins["pkginfo.name"] != "software.name" {
		t.Fatalf("origins without metadata = %+v", publication)
	}
}

func TestPublicationIsWhatPlanAndApplyPublish(t *testing.T) {
	root := t.TempDir()
	suitePackage(t, filepath.Join(root, "suite.pkg"), map[string]string{"Draw": "26.1", "Graph": "8.2"})
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: publication}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: suite}
spec:
  source: {path: suite.pkg}
  subjects:
    draw: {bundle_id: org.example.suite.draw}
    graph: {bundle_id: org.example.suite.graph}
  destinations:
    repo:
      pkginfo:
        catalogs: [production]
        category: Media
        uninstallable: false
        version: "{{ facts.draw.app.version }}"
        installs: ["{{ facts.draw }}", "{{ facts.graph }}"]
`)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir()}
	run := func(method string) ResourceReport {
		t.Helper()
		options.Method = method
		report, err := Run(t.Context(), options)
		if err != nil {
			t.Fatalf("%s: %v", method, err)
		}
		return report.Resources[0]
	}
	run("update")
	prepared := run("prepare").Publications[0]
	var reported struct {
		Pkginfo map[string]any `json:"pkginfo"`
	}
	if err := json.Unmarshal(prepared.Metadata, &reported); err != nil {
		t.Fatal(err)
	}
	pkginfo := reported.Pkginfo
	installs, _ := pkginfo["installs"].([]any)
	if pkginfo["version"] != "26.1" || pkginfo["category"] != "Media" || pkginfo["uninstallable"] != false || len(installs) != 2 || len(pkginfo["receipts"].([]any)) != 1 {
		t.Fatalf("publication = %s", prepared.Metadata)
	}
	for _, field := range []string{"RestartAction", "items_to_copy", "minimum_os_version", "uninstall_method"} {
		if value, listed := pkginfo[field]; !listed || value != nil {
			t.Errorf("%s = %v (listed %t), want null", field, value, listed)
		}
	}
	for field, origin := range map[string]string{"pkginfo.category": "explicit", "pkginfo.uninstallable": "explicit", "pkginfo.version": "expression", "pkginfo.installs": "expression", "pkginfo.receipts": "installer.receipts"} {
		if prepared.Origins[field] != origin {
			t.Errorf("origin of %s = %q, want %q", field, prepared.Origins[field], origin)
		}
	}
	// published compares a pkginfo document with the publication, apart from
	// the fields the repository holds without the declaration managing them.
	published := func(document map[string]any, unmanaged ...string) {
		t.Helper()
		for field, want := range pkginfo {
			if got, written := document[field]; want == nil && written || want != nil && !reflect.DeepEqual(got, want) {
				t.Errorf("%s is %v, reported as %v", field, got, want)
			}
		}
		for field := range document {
			if _, known := pkginfo[field]; !known && !slices.Contains(unmanaged, field) {
				t.Errorf("%s is published without being reported", field)
			}
		}
	}
	// document decodes a native value as the publication's JSON decodes.
	document := func(value any) map[string]any {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(data, &fields); err != nil {
			t.Fatal(err)
		}
		return fields
	}

	planned := run("plan")
	if !reflect.DeepEqual(planned.Publications[0], prepared) || !reflect.DeepEqual(planned.Destinations[0].Origins, prepared.Origins) {
		t.Fatalf("plan reports %+v with origins %v, preparation %+v", planned.Publications[0], planned.Destinations[0].Origins, prepared)
	}
	var created map[string]any
	for _, change := range planned.Destinations[0].Changes {
		if change.Field == "pkginfo" && change.Action == "create" {
			created = document(change.After)
		}
	}
	if created == nil {
		t.Fatalf("plan = %+v", planned.Destinations[0].Changes)
	}
	published(created, "installer_item_location")
	run("apply")

	// An item that holds fields derivation clears loses them, and keeps one
	// the declaration never managed.
	stored := filepath.Join(root, "repo", "pkgsinfo", "suite-26.1.plist")
	read := func() map[string]any {
		t.Helper()
		data, err := os.ReadFile(stored)
		if err != nil {
			t.Fatal(err)
		}
		var native map[string]any
		if _, err := plist.Unmarshal(data, &native); err != nil {
			t.Fatal(err)
		}
		return native
	}
	native := read()
	published(document(native), "installer_item_location")
	native["RestartAction"], native["minimum_os_version"], native["notes"] = "RequireRestart", "12.0", "Kept by hand"
	data, err := plist.MarshalIndent(native, plist.XMLFormat, "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stored, data, 0o644); err != nil {
		t.Fatal(err)
	}
	var cleared []string
	for _, change := range run("plan").Destinations[0].Changes {
		if change.Action == "clear" {
			cleared = append(cleared, change.Field)
		}
	}
	if slices.Sort(cleared); !slices.Equal(cleared, []string{"RestartAction", "minimum_os_version"}) {
		t.Fatalf("plan clears %v", cleared)
	}
	run("apply")
	published(document(read()), "installer_item_location", "notes")
}

func TestJamfPatchVersionExpressionKeepsTheArtifactVersion(t *testing.T) {
	root := t.TempDir()
	suitePackage(t, filepath.Join(root, "suite.pkg"), map[string]string{"Draw": "26.1"})
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: publication}
spec:
  imports: ['*.software.yaml']
  destinations:
    jamf:
      operation: jamf
      config:
        url: "{{ env.STEMMA_TEST_UNSET_JAMF_URL }}"
        client_id: "{{ env.STEMMA_TEST_UNSET_JAMF_CLIENT }}"
        client_secret: "{{ env.STEMMA_TEST_UNSET_JAMF_SECRET }}"
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: suite}
spec:
  source: {path: suite.pkg}
  subjects:
    draw: {bundle_id: org.example.suite.draw}
  destinations:
    jamf:
      patch:
        title: Draw
        version: "{{ facts.draw.app.version }}"
`)
	options := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "update"}
	if _, err := Run(t.Context(), options); err != nil {
		t.Fatal(err)
	}
	options.Method = "prepare"
	report, err := Run(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	resource := report.Resources[0]
	artifact, publication := resource.Artifacts["installer"], resource.Publications[0]
	if artifact.Version != "1.0" || artifact.Filename != "suite-1.0.pkg" {
		t.Fatalf("patch version changed the artifact: %+v", artifact)
	}
	var metadata struct {
		Filename string `json:"filename"`
		Patch    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"patch"`
	}
	if err := json.Unmarshal(publication.Metadata, &metadata); err != nil {
		t.Fatal(err)
	}
	if publication.Deferred != "" || metadata.Filename != artifact.Filename || metadata.Patch.Title != "Draw" || metadata.Patch.Version != "26.1" || publication.Origins["patch.version"] != "expression" {
		t.Fatalf("patch publication = %+v", publication)
	}
}

// suitePackage builds a vendor suite: one component package, always version
// 1.0, that installs each named application at its own version.
func suitePackage(t *testing.T, filename string, versions map[string]string) {
	t.Helper()
	source := t.TempDir()
	for name, version := range versions {
		contents := filepath.Join(source, "payload", name+".app", "Contents")
		if err := os.MkdirAll(contents, 0o755); err != nil {
			t.Fatal(err)
		}
		info := fmt.Sprintf(`<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.suite.%s</string><key>CFBundleName</key><string>%s</string><key>CFBundleShortVersionString</key><string>%s</string><key>CFBundleVersion</key><string>%s</string></dict></plist>`, strings.ToLower(name), name, version, version)
		if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(info), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(filename)
	if err := pkgbuild.Build(t.Context(), source, filename, pkgbuild.Options{Identifier: "org.example.suite", Version: "1.0", Payload: "payload", InstallLocation: "/Applications", Compression: pkgbuild.Gzip}); err != nil {
		t.Fatal(err)
	}
}
