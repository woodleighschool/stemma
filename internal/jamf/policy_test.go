package jamf

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestInstallPolicyInstallsTheCurrentPackage(t *testing.T) {
	server, request := newNativeFixture(t)
	server.categories = map[string]string{"7": "Productivity", "8": "Applications"}
	request.Artifact.Evidence = selectedApplication("/Applications/Vendor App.app")
	request.Inputs = map[string]plugin.Artifact{"icon": fixtureIcon(t)}
	policy := map[string]any{"name": "Install Vendor", "enabled": true, "category": "Applications", "triggers": []string{"enrollment_complete", "checkin"}, "frequency": "once_per_computer", "retries": 3, "event": "vendor", "self_service": true, "scope": map[string]any{"computer_groups": []string{"Managed Macs"}}}
	request.Metadata = raw(map[string]any{"category": "Productivity", "policies": []any{policy}})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil || !slices.ContainsFunc(plan.Changes, func(c plugin.Change) bool { return c.Field == "policies[Install Vendor]" && c.Action == "create" }) {
		t.Fatalf("new policy plan: %+v: %v", plan, err)
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("plan wrote to Jamf: %v", writes)
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native := server.native.policies["100"]
	for _, test := range []struct {
		path []string
		want string
	}{
		{[]string{"general", "name"}, "Install Vendor"},
		{[]string{"general", "enabled"}, "true"},
		{[]string{"general", "category", "id"}, "8"},
		{[]string{"general", "frequency"}, "Once per computer"},
		{[]string{"general", "trigger_checkin"}, "true"},
		{[]string{"general", "trigger_enrollment_complete"}, "true"},
		{[]string{"general", "trigger_login"}, "false"},
		{[]string{"general", "retry_event"}, "check-in"},
		{[]string{"general", "retry_attempts"}, "3"},
		{[]string{"general", "trigger_other"}, "vendor"},
		{[]string{"package_configuration", "packages", "package", "id"}, "1"},
		{[]string{"package_configuration", "packages", "package", "action"}, "Install"},
		{[]string{"maintenance", "recon"}, "true"},
		{[]string{"self_service", "use_for_self_service"}, "true"},
		{[]string{"self_service", "self_service_display_name"}, "Vendor App"},
		{[]string{"self_service", "self_service_categories", "category", "id"}, "7"},
		{[]string{"self_service", "self_service_icon", "filename"}, fixtureIconName(t)},
		{[]string{"scope", "computer_groups", "computer_group", "id"}, "3"},
	} {
		if got := native.value(test.path...); got != test.want {
			t.Errorf("%s = %q, want %q", strings.Join(test.path, "/"), got, test.want)
		}
	}
	steps := server.operations()
	if slices.Index(steps, "POST "+packagePath+"/1/upload") > slices.Index(steps, "POST /JSSResource/policies/id/0") {
		t.Fatal("policy was created before its package's content")
	}
	assertSettled(t, server, request)
	request.Artifact = fixtureArtifactNamed(t, "vendor-2.0.pkg", "new payload")
	request.Artifact.Evidence = selectedApplication("/Applications/Vendor App.app")
	request.Metadata = raw(map[string]any{"category": "Productivity", "policies": []any{policy}, "retention": map[string]int{"keep": 1}})
	for _, method := range []string{"plan", "apply"} {
		request.Method = method
		response, err := Handle(t.Context(), request)
		if err != nil || !slices.Equal(deletions(response), []string{"1"}) {
			t.Fatalf("%s kept the package the policy moves off: %+v: %v", method, response, err)
		}
	}
	if server.native.policies["100"].value("package_configuration", "packages", "package", "id") != "2" || server.exists("1") || len(server.native.icons) != 1 {
		t.Fatal("new version did not move the one policy to its package without uploading the icon again")
	}
	assertSettled(t, server, request)
}

func TestSeveralPoliciesInstallOnePackage(t *testing.T) {
	server, request := newNativeFixture(t)
	available := map[string]any{"name": "ALL - Vendor - ALL", "frequency": "ongoing", "triggers": []string{}, "self_service": true, "scope": map[string]any{"all_computers": true}}
	required := map[string]any{"name": "ALL - Vendor - Staff", "frequency": "once_per_computer", "triggers": []string{"checkin"}, "retries": 3, "scope": map[string]any{"all_computers": false, "computer_groups": []string{"Staff Macs"}}}
	request.Metadata = raw(map[string]any{"policies": []any{available, required}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(server.native.policies) != 2 {
		t.Fatalf("policies: %d, want 2", len(server.native.policies))
	}
	for id, want := range map[string]string{"100": "ALL - Vendor - ALL", "101": "ALL - Vendor - Staff"} {
		if native := server.native.policies[id]; native.value("general", "name") != want || native.value("package_configuration", "packages", "package", "id") != "1" {
			t.Fatalf("policy %s does not install the package as %s: %+v", id, want, native)
		}
	}
	if server.native.policies["101"].value("self_service", "use_for_self_service") == "true" || server.native.policies["100"].value("general", "trigger_checkin") != "false" {
		t.Fatal("one policy's settings reached the other")
	}
	assertSettled(t, server, request)
	request.Artifact = fixtureArtifactNamed(t, "vendor-2.0.pkg", "new payload")
	request.Metadata = raw(map[string]any{"policies": []any{available, required}, "retention": map[string]int{"keep": 1}})
	for _, method := range []string{"plan", "apply"} {
		request.Method = method
		response, err := Handle(t.Context(), request)
		if err != nil || !slices.Equal(deletions(response), []string{"1"}) {
			t.Fatalf("%s kept the package both policies move off: %+v: %v", method, response, err)
		}
	}
	for _, id := range []string{"100", "101"} {
		if server.native.policies[id].value("package_configuration", "packages", "package", "id") != "2" {
			t.Fatalf("policy %s did not move to the new package", id)
		}
	}
	assertSettled(t, server, request)
}

func TestNewPolicyReusesAnExistingIcon(t *testing.T) {
	server, request := newNativeFixture(t)
	request.Inputs = map[string]plugin.Artifact{"icon": fixtureIcon(t)}
	existing := map[string]any{"name": "Install Vendor", "self_service": true}
	request.Metadata = raw(map[string]any{"policies": []any{existing}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	iconID := server.native.policies["100"].value("self_service", "self_service_icon", "id")
	// The new policy comes first so reuse cannot depend on apply order.
	request.Metadata = raw(map[string]any{"policies": []any{map[string]any{"name": "Install Vendor for staff", "self_service": true}, existing}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if len(server.native.icons) != 1 || server.native.policies["101"].value("self_service", "self_service_icon", "id") != iconID {
		t.Fatal("adding a policy uploaded another copy of the existing icon")
	}
	assertSettled(t, server, request)
}

func TestInstallPolicyDefaults(t *testing.T) {
	server, request := newNativeFixture(t)
	request.Metadata = raw(map[string]any{"policies": []any{map[string]any{"name": "Vendor"}}})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	for _, change := range plan.Changes {
		if strings.HasPrefix(change.Field, "policies") {
			fields = append(fields, change.Field)
		}
	}
	if !slices.Equal(fields, []string{"policies[Vendor]", "policies[Vendor].package"}) {
		t.Fatalf("default policy plan: %v", fields)
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native := server.native.policies["100"]
	if native.value("general", "name") != "Vendor" || native.value("general", "enabled") != "false" || native.value("general", "frequency") != "Ongoing" || native.value("general", "trigger_other") != "" || native.value("maintenance", "recon") != "true" {
		t.Fatalf("default policy: %+v", native)
	}
	if native.child("self_service") != nil || len(native.child("scope").Children) != 1 {
		t.Fatal("a policy without Self Service or scope was offered or scoped")
	}
	assertSettled(t, server, request)
}

func TestSelfServiceSettings(t *testing.T) {
	server, request := newNativeFixture(t)
	server.categories = map[string]string{"7": "Productivity", "8": "Applications"}
	policy := map[string]any{"name": "Vendor", "event": "", "update_inventory": false, "self_service": true}
	request.Metadata = raw(map[string]any{"policies": []any{policy}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native := server.native.policies["100"]
	if native.value("self_service", "self_service_display_name") != "vendor" || native.value("general", "trigger_other") != "" || native.value("maintenance", "recon") != "false" {
		t.Fatal("Self Service name did not fall back to the software name, or the event or inventory update remained")
	}
	if native.child("self_service").child("self_service_categories") != nil {
		t.Fatal("an undeclared category was managed")
	}
	policy["self_service"] = map[string]any{"display_name": "Vendor Suite", "description": "Install **Vendor**.", "category": "Applications", "featured": true}
	request.Metadata = raw(map[string]any{"category": "Productivity", "policies": []any{policy}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native = server.native.policies["100"]
	if native.value("self_service", "self_service_display_name") != "Vendor Suite" || native.value("self_service", "self_service_description") != "Install **Vendor**." || native.value("self_service", "feature_on_main_page") != "true" || native.value("self_service", "self_service_categories", "category", "id") != "8" {
		t.Fatalf("declared Self Service settings: %+v", native.child("self_service"))
	}
	assertSettled(t, server, request)
	policy["self_service"] = map[string]any{"display_name": "Vendor Suite", "category": nil}
	request.Metadata = raw(map[string]any{"category": "Productivity", "policies": []any{policy}})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	assertTruthful(t, plan)
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if categories := server.native.policies["100"].child("self_service").child("self_service_categories"); categories == nil || len(categories.Children) != 0 {
		t.Fatal("a null Self Service category left the policy in a category")
	}
	assertSettled(t, server, request)
	policy["self_service"] = false
	request.Metadata = raw(map[string]any{"policies": []any{policy}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if native = server.native.policies["100"]; native.value("self_service", "use_for_self_service") != "false" || native.value("self_service", "self_service_display_name") != "Vendor Suite" {
		t.Fatal("removing the policy from Self Service changed its Self Service settings")
	}
	assertSettled(t, server, request)
}

func TestPolicyCategoryIsReplacedWhole(t *testing.T) {
	server, request := newNativeFixture(t)
	server.categories = map[string]string{"7": "Productivity", "8": "Applications"}
	for _, test := range []struct {
		metadata map[string]any
		want     string
	}{
		{map[string]any{"category": "Productivity", "policies": []any{map[string]any{"name": "Vendor"}}}, "7"},
		{map[string]any{"category": "Productivity", "policies": []any{map[string]any{"name": "Vendor", "category": "Applications"}}}, "8"},
		{map[string]any{"category": "Productivity", "policies": []any{map[string]any{"name": "Vendor", "category": nil}}}, "-1"},
		{map[string]any{"category": "Productivity", "policies": []any{map[string]any{"name": "Vendor"}}}, "7"},
		{map[string]any{"category": nil, "policies": []any{map[string]any{"name": "Vendor"}}}, "-1"},
	} {
		request.Metadata = raw(test.metadata)
		if _, err := Handle(t.Context(), request); err != nil {
			t.Fatalf("category %s: %v", test.want, err)
		}
		if got := server.native.policies["100"].value("general", "category", "id"); got != test.want {
			t.Fatalf("policy category = %s, want %s", got, test.want)
		}
		assertSettled(t, server, request)
	}
}

func TestPolicyPlanShowsWhatChanges(t *testing.T) {
	server, request := newNativeFixture(t)
	server.categories = map[string]string{"7": "Productivity"}
	server.seed(map[string]any{"fileName": "vendor-config.pkg", "notes": "Administrator package"})
	server.native.seedPolicy("20", "ALL - Vendor - ALL", "1")
	admin := server.native.policies["20"]
	admin.child("package_configuration").child("packages").Children[0].set(xmlNode{XMLName: xml.Name{Local: "action"}, Text: "Cache"})
	for name, value := range map[string]string{"trigger_checkin": "true", "frequency": "Ongoing", "retry_event": "none", "retry_attempts": "-1"} {
		admin.child("general").set(xmlNode{XMLName: xml.Name{Local: name}, Text: value})
	}
	admin.set(*jsonXML("self_service", raw(map[string]any{"use_for_self_service": true, "self_service_display_name": "vendor", "self_service_categories": []map[string]any{{"id": 7, "display_in": false, "feature_in": false}}})))
	policy := map[string]any{"name": "ALL - Vendor - ALL", "triggers": []string{"checkin", "enrollment_complete"}, "frequency": "once_per_computer", "retries": 3, "self_service": map[string]any{"category": "Productivity"}}
	request.Metadata = raw(map[string]any{"policies": []any{policy}})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	assertTruthful(t, plan)
	fields := map[string]plugin.Change{}
	for _, change := range plan.Changes {
		fields[change.Field] = change
	}
	for field, want := range map[string][2]any{
		"triggers":              {[]string{"checkin"}, []string{"checkin", "enrollment_complete"}},
		"frequency":             {"ongoing", "once_per_computer"},
		"retries":               {0, 3},
		"package":               {[]string{"vendor-config.pkg (Cache)"}, []string{"vendor.pkg"}},
		"self_service.category": {"Productivity (hidden)", "Productivity"},
	} {
		change := fields["policies[ALL - Vendor - ALL]."+field]
		if !equalJSON(change.Before, raw(want[0])) || !equalJSON(change.After, raw(want[1])) {
			t.Errorf("%s: %s → %s, want %s → %s", field, change.Before, change.After, raw(want[0]), raw(want[1]))
		}
	}
}

func TestInstallPolicyFoundByNameKeepsUnownedSettings(t *testing.T) {
	server, request := newNativeFixture(t)
	server.native.seedPolicy("20", "ALL - Vendor - ALL", "99")
	admin := server.native.policies["20"]
	admin.child("general").set(xmlNode{XMLName: xml.Name{Local: "trigger_other"}, Text: "vendor_install"})
	admin.set(*jsonXML("scripts", raw(map[string]any{"size": 1, "script": map[string]any{"id": 4, "priority": "After"}})))
	admin.set(*jsonXML("self_service", raw(map[string]any{"use_for_self_service": false, "self_service_description": "Written in Jamf"})))
	request.Metadata = raw(map[string]any{"policies": []any{map[string]any{"name": "ALL - Vendor - ALL", "event": "vendor", "self_service": true}}})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	assertTruthful(t, plan)
	fields := map[string]plugin.Change{}
	for _, change := range plan.Changes {
		fields[change.Field] = change
	}
	if _, created := fields["policies[ALL - Vendor - ALL]"]; created {
		t.Fatalf("existing policy planned for creation: %+v", plan.Changes)
	}
	if change := fields["policies[ALL - Vendor - ALL].event"]; !equalJSON(change.Before, raw("vendor_install")) || !equalJSON(change.After, raw("vendor")) {
		t.Fatalf("event change is not in declaration terms: %+v", change)
	}
	if change := fields["policies[ALL - Vendor - ALL].self_service"]; !equalJSON(change.Before, raw(false)) || !equalJSON(change.After, raw(true)) {
		t.Fatalf("Self Service change is not in declaration terms: %+v", change)
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native := server.native.policies["20"]
	if server.native.createdInstalls != 0 || native.value("package_configuration", "packages", "package", "id") != "1" {
		t.Fatal("the named policy was not updated in place")
	}
	if native.value("scripts", "script", "id") != "4" || native.value("self_service", "self_service_description") != "Written in Jamf" || native.value("general", "frequency") != "Once per computer" {
		t.Fatal("unowned policy settings changed")
	}
	assertSettled(t, server, request)
	server.native.seedPolicy("21", "ALL - Vendor - ALL")
	writes := len(server.writes())
	if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "multiple Jamf policies") {
		t.Fatalf("duplicate policy names: %v", err)
	}
	if len(server.writes()) != writes {
		t.Fatal("ambiguous policy wrote to Jamf")
	}
}

func TestLostInstallPolicyCreateResponseIsFound(t *testing.T) {
	server, request := newNativeFixture(t)
	server.native.failAfterPolicyCreate = true
	request.Metadata = raw(map[string]any{"policies": []any{map[string]any{"name": "Vendor", "enabled": true}}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatalf("committed policy creation was not recovered: %v", err)
	}
	if server.native.createdInstalls != 1 || server.native.policies["100"].value("general", "enabled") != "true" {
		t.Fatal("recovered policy was duplicated or left incomplete")
	}
}

func TestPolicyNamesMatchExactly(t *testing.T) {
	for _, name := range []string{" Vendor ", "Vendor"} {
		t.Run(name, func(t *testing.T) {
			server, request := newNativeFixture(t)
			server.native.seedPolicy("20", " Vendor ", "99")
			request.Metadata = raw(map[string]any{"policies": []any{map[string]any{"name": name}}})
			if _, err := Handle(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if name == " Vendor " {
				if server.native.createdInstalls != 0 || server.native.policies["20"].value("package_configuration", "packages", "package", "id") != "1" {
					t.Fatal("the exact native name did not find the existing policy")
				}
			} else if server.native.createdInstalls != 1 || server.native.policies["20"].value("package_configuration", "packages", "package", "id") != "99" {
				t.Fatal("a different native name was adopted")
			}
			assertSettled(t, server, request)
		})
	}
}

func TestPolicyCreationRejectsIncompleteEnumeration(t *testing.T) {
	for name, response := range map[string]string{
		"missing count": `<policies/>`,
		"partial list":  `<policies><size>1</size></policies>`,
		"duplicate IDs": `<policies><size>2</size><policy><id>1</id><name>Other</name></policy><policy><id>1</id><name>Other</name></policy></policies>`,
		"invalid ID":    `<policies><size>1</size><policy><id>0</id><name>Other</name></policy></policies>`,
		"missing name":  `<policies><size>1</size><policy><id>1</id></policy></policies>`,
	} {
		t.Run(name, func(t *testing.T) {
			fake, request := newNativeFixture(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Path == "/JSSResource/policies" {
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(response))
					return
				}
				fake.handle(w, r)
			}))
			t.Cleanup(server.Close)
			request.Config.URL = server.URL
			request.Metadata = raw(map[string]any{"policies": []any{map[string]any{"name": "Vendor"}}})
			if _, err := Handle(t.Context(), request); err == nil {
				t.Fatal("incomplete policy enumeration was accepted")
			}
			if writes := fake.writes(); len(writes) != 0 {
				t.Fatalf("incomplete policy enumeration wrote to Jamf: %v", writes)
			}
		})
	}
}

func TestPolicyDeclarationValidation(t *testing.T) {
	_, request := newFixture(t)
	request.Method = "validate"
	for _, data := range []string{
		`[]`,
		`[{"name":"Vendor"}]`,
		`[{"name":"Vendor","self_service":true},{"name":"Vendor for staff","self_service":false}]`,
		`[{"name":"Vendor","self_service":{"display_name":"Vendor","description":"","category":"Applications","featured":false}}]`,
		`[{"name":"Vendor","category":null,"self_service":{"category":null}}]`,
		`[{"name":"Vendor","event":"","update_inventory":false}]`,
		`[{"name":"Vendor","triggers":[],"frequency":"ongoing","retries":0}]`,
		`[{"name":"Vendor","triggers":["checkin","enrollment_complete","login","startup","network_state_change"],"frequency":"once_per_computer","retries":10}]`,
		`[{"name":"Vendor","enabled":true,"scope":{"all_computers":false,"computer_groups":[]}}]`,
	} {
		request.Metadata = json.RawMessage(`{"policies":` + data + `}`)
		if _, err := Handle(t.Context(), request); err != nil {
			t.Fatalf("valid policies rejected: %s: %v", data, err)
		}
	}
	for _, data := range []string{
		`null`,
		`{"name":"Vendor"}`,
		`[null]`,
		`[{}]`,
		`[{"name":" "}]`,
		`[{"name":"Vendor"},{"name":"Vendor"}]`,
		`[{"name":"Vendor","self_service":null}]`,
		`[{"name":"Vendor","enabled":null}]`,
		`[{"name":"Vendor","category":" "}]`,
		`[{"name":"Vendor","automatic":true}]`,
		`[{"name":"Vendor","triggers":["check-in"]}]`,
		`[{"name":"Vendor","triggers":["checkin","checkin"]}]`,
		`[{"name":"Vendor","triggers":"checkin"}]`,
		`[{"name":"Vendor","frequency":"Ongoing"}]`,
		`[{"name":"Vendor","retries":3}]`,
		`[{"name":"Vendor","frequency":"ongoing","retries":3}]`,
		`[{"name":"Vendor","frequency":"once_per_computer","retries":11}]`,
		`[{"name":"Vendor","frequency":"once_per_computer","retries":-1}]`,
		`[{"name":"Vendor","self_service":{"icon":"vendor.png"}}]`,
		`[{"name":"Vendor","self_service":{"display_name":" "}}]`,
		`[{"name":"Vendor","self_service":{"category":""}}]`,
		`[{"name":"Vendor","self_service":"true"}]`,
		`[{"name":"Vendor","scope":{"computer_groups":[""]}}]`,
	} {
		request.Metadata = json.RawMessage(`{"policies":` + data + `}`)
		if _, err := Handle(t.Context(), request); err == nil {
			t.Fatalf("invalid policies accepted: %s", data)
		}
	}
	request.Metadata = json.RawMessage(`{"policy":{}}`)
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("a single policy object was accepted")
	}
}

// assertSettled checks that plan and apply find nothing to change and write nothing.
func assertSettled(t *testing.T, server *fakeServer, request plugin.ReconcileRequest[Config]) {
	t.Helper()
	writes := len(server.writes())
	for _, method := range []string{"plan", "apply"} {
		request.Method = method
		if response, err := Handle(t.Context(), request); err != nil || len(response.Changes) != 0 {
			t.Fatalf("unchanged %s: %+v: %v", method, response, err)
		}
	}
	if len(server.writes()) != writes {
		t.Fatalf("settled publication wrote to Jamf: %v", server.writes()[writes:])
	}
}

// assertTruthful checks that every setting a plan changes shows a different
// value before and after.
func assertTruthful(t *testing.T, plan plugin.ReconcileResponse) {
	t.Helper()
	for _, change := range plan.Changes {
		if change.Action == "set" && equalJSON(change.Before, change.After) {
			t.Errorf("%s changes but shows %s on both sides", change.Field, change.Before)
		}
	}
}

func selectedApplication(installed string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"macos.application": raw(plugin.Subject{ID: "app", Kind: "app", Path: "payload" + installed, InstalledPath: installed, App: &plugin.AppFacts{BundleID: "com.example.vendor", Name: "Vendor", Version: "1.0"}})}
}

func fixtureIcon(t *testing.T) plugin.Artifact {
	t.Helper()
	var image bytes.Buffer
	if err := png.Encode(&image, newImage()); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "icon")
	if err := os.WriteFile(file, image.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(image.Bytes())
	return plugin.Artifact{Path: file, SHA256: hex.EncodeToString(digest[:]), Size: int64(image.Len()), Filename: "icon.png", Format: "png"}
}

func fixtureIconName(t *testing.T) string {
	t.Helper()
	return fixtureIcon(t).SHA256 + ".png"
}

func newImage() image.Image { return image.NewNRGBA(image.Rect(0, 0, 2, 2)) }
