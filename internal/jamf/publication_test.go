package jamf

import (
	"reflect"
	"slices"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestPublicationUsesPackageFilenames(t *testing.T) {
	server, request := newNativeFixture(t)
	request.Artifact = fixtureArtifactNamed(t, "vendor-1.0.pkg", "first payload")
	request.Artifact.Version = "1.0"
	request.Metadata = raw(map[string]any{"priority": 5, "notes": "Operator note", "retention": map[string]int{"keep": 1}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	first := request.Artifact.SHA256
	request.Artifact = fixtureArtifactNamed(t, "vendor-2.0.pkg", "second payload")
	request.Artifact.Version = "2.0"
	request.Method = "plan"
	writes := len(server.writes())
	plan, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(server.writes()) != writes {
		t.Fatal("plan wrote to Jamf")
	}
	if plan.Version != "vendor-2.0.pkg" || plan.Previous != "vendor-1.0.pkg" || !slices.Equal(plan.Removed, []string{"vendor-1.0.pkg"}) {
		t.Fatalf("publication: %+v", plan)
	}
	if want := (plugin.Upload{Name: plugin.UploadInstaller, Filename: "vendor-2.0.pkg", Before: first, After: request.Artifact.SHA256}); len(plan.Uploads) != 1 || plan.Uploads[0] != want {
		t.Fatalf("upload: %+v", plan.Uploads)
	}
	if len(plan.Fields) != 1 || plan.Fields[0].Field != "filename" || !equalJSON(plan.Fields[0].Before, raw("vendor-1.0.pkg")) || !equalJSON(plan.Fields[0].After, raw("vendor-2.0.pkg")) {
		t.Fatalf("fields: %+v", plan.Fields)
	}
	request.Method = "apply"
	applied, err := Handle(t.Context(), request)
	if err != nil || !reflect.DeepEqual(plan, applied) {
		t.Fatalf("plan=%+v apply=%+v: %v", plan, applied, err)
	}
	if server.exists("1") || stringField(server.record("2"), "notes") != "Operator note\n"+fixtureMarker {
		t.Fatal("retention or ownership notes differ from the publication")
	}
	assertSettled(t, server, request)
}

func TestPublicationWithCustomFilename(t *testing.T) {
	server, request := newFixture(t)
	request.Artifact.Version = "1.0"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.Artifact = fixtureArtifact(t, "replacement payload")
	request.Artifact.Version = "2.0"
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil || plan.Version != "vendor.pkg" || plan.Previous != "vendor.pkg" || len(plan.Uploads) != 1 {
		t.Fatalf("in-place publication: %+v: %v", plan, err)
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 1 || stringField(server.record("1"), "notes") != fixtureMarker {
		t.Fatalf("publication duplicated the package or changed its marker: %v", server.writes())
	}
	assertSettled(t, server, request)
}

func TestNewSoftwareReportsInitialFields(t *testing.T) {
	server, request := newNativeFixture(t)
	server.categories = map[string]string{"7": "Productivity"}
	request.Artifact.Version = "1.0"
	request.Metadata = raw(map[string]any{"category": "Productivity", "priority": 5, "policies": []any{map[string]any{"name": "Install Vendor", "enabled": true}}})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if want := (plugin.Upload{Name: plugin.UploadInstaller, Filename: "vendor.pkg", After: request.Artifact.SHA256}); plan.Version != "vendor.pkg" || plan.Previous != "" || len(plan.Uploads) != 1 || plan.Uploads[0] != want {
		t.Fatalf("new software plan: %+v", plan)
	}
	want := map[string]any{
		"category":            "Productivity",
		"filename":            "vendor.pkg",
		"priority":            5,
		"policies.0.name":     "Install Vendor",
		"policies.0.enabled":  true,
		"policies.0.category": "Productivity",
		"policies.0.package":  []string{"vendor.pkg"},
	}
	if len(plan.Fields) != len(want) {
		t.Fatalf("initial fields: %v", fieldPaths(plan))
	}
	for _, field := range plan.Fields {
		if w, ok := want[field.Field]; !ok || field.Before != nil || !equalJSON(field.After, raw(w)) {
			t.Errorf("initial field %s: %+v", field.Field, field)
		}
	}
}
