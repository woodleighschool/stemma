package intune

import (
	"encoding/json"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestNewAppReportsItsInitialFields(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	req.Method, req.Artifact.Version = "plan", "1.0"
	response, err := reconcileRequest(t, c, req)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := identifyArtifact(t.Context(), req.Artifact, win32Type)
	if err != nil {
		t.Fatal(err)
	}
	if response.Previous != "" || response.Version != "1.0" || !reflect.DeepEqual(response.Uploads, []plugin.Upload{{Name: plugin.UploadInstaller, Filename: "test-1.0.intunewin", After: artifact.identity}}) {
		t.Fatalf("new app publication: %+v", response)
	}
	initial := map[string]string{}
	for _, field := range response.Fields {
		if field.Before != nil {
			t.Fatalf("initial field %+v", field)
		}
		initial[field.Field] = string(field.After)
	}
	want := map[string]string{
		"display_name": `"Fixture"`, "description": `"Test app"`, "publisher": `"Fixture Publisher"`,
		"install_command": `"setup.cmd"`, "uninstall_command": `"setup.cmd /remove"`, "minimum_windows_release": `"Windows11_23H2"`,
		"architectures": `["x64"]`, "install_experience.run_as": `"system"`, "setup_file": `"setup.cmd"`,
		"detection":   `[{"product_code":"{AC01F3D3-C5D5-40DB-9E8C-ED53982E17ED}","type":"msi"}]`,
		"assignments": `[{"group":"group-1","intent":"required"}]`,
	}
	if !maps.Equal(initial, want) {
		t.Fatalf("initial fields = %v, want %v", initial, want)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.writes != 0 {
		t.Fatal("plan wrote to the tenant")
	}
}

func TestVersionChangeUpdatesTheAppInPlace(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	metadata, err := decodeObject(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadata["detection"] = []any{object{"type": "file", "path": `C:\Program Files\Fixture`, "name": "fixture.exe", "property": "version"}}
	req.Metadata, req.Artifact.Version = raw(metadata), "1.0"
	if _, err := reconcileRequest(t, c, req); err != nil {
		t.Fatal(err)
	}
	first := publishedMarker(t, fake)
	changePayload(t, &req, "second release")
	req.Artifact.Version = "1.1"
	artifact, err := identifyArtifact(t.Context(), req.Artifact, win32Type)
	if err != nil {
		t.Fatal(err)
	}
	rule := func(version string) json.RawMessage {
		return raw([]any{object{"type": "file", "path": `C:\Program Files\Fixture`, "name": "fixture.exe", "property": "version", "operator": "greater_than_or_equal", "value": version}})
	}
	want := plugin.ReconcileResponse{
		Version: "1.1", Previous: "1.0",
		Uploads: []plugin.Upload{{Name: plugin.UploadInstaller, Filename: "test-1.1.intunewin", Before: first.payload, After: artifact.identity}},
		Fields:  []plugin.FieldChange{{Field: "detection", Before: rule("1.0"), After: rule("1.1")}},
	}
	for _, method := range []string{"plan", "apply"} {
		req.Method = method
		if response, err := reconcileRequest(t, c, req); err != nil || !reflect.DeepEqual(response, want) {
			t.Fatalf("%s = %+v, %v", method, response, err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 1 || fake.versions != 2 || fake.app["displayVersion"] != "1.1" {
		t.Fatalf("version change did not update the app in place: %+v", fake.app)
	}
}

func TestMetadataChangeReportsOnlyFields(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	req.Artifact.Version = "1.0"
	if _, err := reconcileRequest(t, c, req); err != nil {
		t.Fatal(err)
	}
	metadata, err := decodeObject(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadata["description"] = "Changed"
	metadata["install_experience"] = object{"run_as": "system", "restart": "based_on_return_code"}
	req.Metadata = raw(metadata)
	want := plugin.ReconcileResponse{Version: "1.0", Previous: "1.0", Fields: []plugin.FieldChange{
		{Field: "description", Before: json.RawMessage(`"Test app"`), After: json.RawMessage(`"Changed"`)},
		{Field: "install_experience.restart", After: json.RawMessage(`"based_on_return_code"`)},
	}}
	for _, method := range []string{"plan", "apply"} {
		req.Method = method
		if response, err := reconcileRequest(t, c, req); err != nil || !reflect.DeepEqual(response, want) {
			t.Fatalf("%s = %+v, %v", method, response, err)
		}
	}
	if response, err := reconcileRequest(t, c, req); err != nil || response.Changed() {
		t.Fatalf("applied metadata did not settle: %+v, %v", response, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.versions != 1 || fake.app["description"] != "Changed" {
		t.Fatal("metadata change uploaded content or was not applied")
	}
}

func TestAssignmentsAreOneFieldInDeclaredTerms(t *testing.T) {
	_, c := newGraphFixture(t)
	req := fixtureRequest(t)
	if _, err := reconcileRequest(t, c, req); err != nil {
		t.Fatal(err)
	}
	metadata, err := decodeObject(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	assignments := []any{
		object{"intent": "available", "all_devices": true, "filter": object{"id": "filter-1", "mode": "include"}, "notifications": "hide_all"},
		object{"intent": "required", "exclude_group": "group-2"},
	}
	metadata["assignments"] = assignments
	req.Metadata, req.Method = raw(metadata), "plan"
	response, err := reconcileRequest(t, c, req)
	if err != nil {
		t.Fatal(err)
	}
	want := []plugin.FieldChange{{Field: "assignments", Before: raw([]any{object{"intent": "required", "group": "group-1"}}), After: raw(assignments)}}
	if len(response.Uploads) != 0 || !reflect.DeepEqual(response.Fields, want) {
		t.Fatalf("assignment fields = %+v", response)
	}
	if strings.Contains(string(raw(response)), "#microsoft.graph.") {
		t.Fatalf("assignments reported Graph types: %s", raw(response))
	}
}

func TestVersionClearWritesWithoutUploading(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	req.Artifact.Version = "1.0"
	if _, err := reconcileRequest(t, c, req); err != nil {
		t.Fatal(err)
	}
	writes, uploads := fake.writes, fake.blobLists
	req.Artifact.Version = ""
	for _, method := range []string{"plan", "apply"} {
		req.Method = method
		result, err := reconcileRequest(t, c, req)
		if err != nil || !result.Changed() || result.Version != "" || result.Previous != "1.0" {
			t.Fatalf("%s: %+v, %v", method, result, err)
		}
		found := false
		for _, field := range result.Fields {
			if field.Field == "display_version" && string(field.Before) == `"1.0"` && string(field.After) == `null` {
				found = true
			}
		}
		if !found {
			t.Fatalf("clear missing from %s: %+v", method, result)
		}
		if method == "plan" && fake.writes != writes {
			t.Fatal("plan wrote")
		}
	}
	if fake.app["displayVersion"] != nil || fake.writes <= writes || fake.blobLists != uploads {
		t.Fatalf("clear did not converge without upload: %+v", fake.app)
	}
	writes = fake.writes
	if result, err := reconcileRequest(t, c, req); err != nil || result.Changed() || fake.writes != writes {
		t.Fatalf("retry: %+v, %v", result, err)
	}
}
