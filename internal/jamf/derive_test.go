package jamf

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestValidatePreviewsLocalManagedValuesWithoutCredentials(t *testing.T) {
	server, request := newFixture(t)
	request.Method, request.Prepared, request.Config = "validate", true, Config{}
	request.Artifact.Version = "2.3"
	request.Artifact.Evidence = selectedApplication("/Applications/Vendor App.app")
	request.Metadata = json.RawMessage(`{
		"category":"Applications", "info":null, "notes":"", "priority":0, "reboot_required":false,
		"policies":[
			{"name":"Install Vendor", "enabled":false, "self_service":true, "scope":{"computer_groups":["Managed Macs"], "computers":[]}},
			{"name":"Custom Vendor", "category":null, "self_service":{"display_name":"Custom app", "description":"", "category":null, "featured":false}},
			{"name":"Hidden Vendor", "self_service":false},
			{"name":"Minimal Vendor"}
		],
		"patch":{"title":"Vendor title", "policy":{"distribution":"self_service", "deadline_days":null, "enabled":false, "grace_minutes":0, "scope":{"all_computers":false}}},
		"retention":{"keep":2}, "package_id":7
	}`)
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(`{
		"filename":"vendor.pkg", "category":"Applications", "info":null, "notes":"", "priority":0, "reboot_required":false,
		"policies":[
			{"name":"Install Vendor", "category":"Applications", "enabled":false, "self_service":{"display_name":"Vendor App", "category":"Applications"}, "scope":{"computer_groups":["Managed Macs"], "computers":[]}},
			{"name":"Custom Vendor", "category":null, "self_service":{"display_name":"Custom app", "description":"", "category":null, "featured":false}},
			{"name":"Hidden Vendor", "category":"Applications", "self_service":false},
			{"name":"Minimal Vendor", "category":"Applications"}
		],
		"patch":{"title":"Vendor title", "version":"2.3", "policy":{"name":"vendor", "distribution":"self_service", "deadline_days":null, "reminder_days":1, "enabled":false, "grace_minutes":0, "scope":{"all_computers":false}}},
		"retention":{"keep":2}, "package_id":7
	}`)
	assertMetadata(t, response.Metadata, want)
	for field, want := range map[string]string{
		"filename": "artifact.filename", "patch.version": "artifact.version",
		"policies.0.category": "category", "policies.0.self_service.display_name": "app.installed_path", "policies.0.self_service.category": "category",
		"patch.policy.name": "software.name", "patch.policy.reminder_days": "default",
	} {
		if got := response.Origins[field]; got != want {
			t.Errorf("origin for %s = %q, want %q", field, got, want)
		}
	}
	if len(response.Changes) != 0 || len(server.operations()) != 0 {
		t.Fatalf("local validation planned changes or contacted Jamf: %+v, %v", response.Changes, server.operations())
	}
}

func TestValidatePreviewPreservesOmissionsAndExplicitPatchSettings(t *testing.T) {
	_, request := newFixture(t)
	request.Method, request.Config = "validate", Config{}
	request.Artifact.Version = "1.0"
	request.Metadata = json.RawMessage(`{"policies":[{"name":"Install Vendor","self_service":true}],"patch":{"title":"Vendor title","policy":{"name":"Pilot","distribution":"self_service","reminder_days":null}}}`)
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	want := json.RawMessage(`{"filename":"vendor.pkg","policies":[{"name":"Install Vendor","self_service":{"display_name":"vendor"}}],"patch":{"title":"Vendor title","version":"1.0","policy":{"name":"Pilot","distribution":"self_service","reminder_days":null}}}`)
	assertMetadata(t, response.Metadata, want)
	if response.Origins["policies.0.self_service.display_name"] != "software.name" {
		t.Fatalf("default origin: %v", response.Origins)
	}
}

func TestValidatePatchNeedsVersionOnlyAfterPreparation(t *testing.T) {
	request := plugin.ReconcileRequest[Config]{
		Method: "validate", Metadata: json.RawMessage(`{"patch":{"title":"Vendor title"},"policies":[]}`),
	}
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	assertMetadata(t, response.Metadata, request.Metadata)
	request.Prepared = true
	if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "managed version") {
		t.Fatalf("prepared validation accepted a missing patch version: %v", err)
	}
	request.Metadata = json.RawMessage(`{"patch":{"title":"Vendor title","version":"3.0"}}`)
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("accepted a patch version override")
	}
}

func assertMetadata(t *testing.T, got, want json.RawMessage) {
	t.Helper()
	var left, right any
	if err := json.Unmarshal(got, &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &right); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(left, right) {
		t.Fatalf("managed metadata = %s, want %s", got, want)
	}
}
