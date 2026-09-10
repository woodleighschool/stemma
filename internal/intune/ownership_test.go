package intune

import (
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestDisappearingDerivedFieldRequiresReplacementOrRelinquishment(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	const field = "msiInformation.upgradeCode"
	desired["msiInformation"] = object{"upgradeCode": "previous-upgrade-code"}
	c.derivation = &derivedOwnership{Active: true, Paths: []string{field}}
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	req.Binding = response.Binding
	if !slices.Equal(readBinding(t, req.Binding).Derived, []string{field}) {
		t.Fatal("successful metadata readback did not persist derived ownership")
	}
	delete(desired, "msiInformation")
	c.derivation.Paths = nil
	changePayload(t, &req, "new installer without upgrade code")
	for _, method := range []string{"plan", "apply"} {
		req.Method = method
		if _, err := c.handle(t.Context(), req, configuration{}, desired); err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "unmanaged") {
			t.Fatalf("%s silently retained a disappeared derived value: %v", method, err)
		}
	}
	fake.mu.Lock()
	if fake.versions != 1 {
		t.Fatal("missing derived metadata was rejected after starting an upload")
	}
	fake.mu.Unlock()
	c.derivation.Unmanaged = []string{field}
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	if len(readBinding(t, response.Binding).Derived) != 0 {
		t.Fatal("unmanaged field retained derived ownership")
	}
	fake.mu.Lock()
	if fake.app["msiInformation"].(object)["upgradeCode"] != "previous-upgrade-code" || fake.versions != 2 {
		t.Fatal("relinquishing ownership changed the remote value or blocked publication")
	}
	fake.mu.Unlock()
	// Removing derive is also an explicit relinquishment, with no inferred clear.
	b := readBinding(t, response.Binding)
	b.Derived = []string{field}
	req.Binding = raw(b)
	c.derivation = &derivedOwnership{}
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil || len(readBinding(t, response.Binding).Derived) != 0 {
		t.Fatalf("removing derive did not relinquish ownership: %v", err)
	}
	// An authored replacement satisfies stale derived ownership and takes over.
	req.Binding = raw(b)
	c.derivation = &derivedOwnership{Active: true}
	desired["msiInformation"] = object{"upgradeCode": "explicit-upgrade-code"}
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil || len(readBinding(t, response.Binding).Derived) != 0 {
		t.Fatalf("explicit replacement failed: %v", err)
	}
}

func TestUnmanagedSuppressesDerivationAndRejectsAuthoredOverlap(t *testing.T) {
	req := plugin.ReconcileRequest{
		Method: "validate", Prepared: true,
		Subjects: map[string]plugin.SubjectSelector{"installer": {Kind: "msi"}},
		Facts:    plugin.Facts{Subjects: []plugin.Subject{{Kind: "msi", MSI: &plugin.MSIFacts{ProductCode: "product-code", UpgradeCode: "upgrade-code", ProductVersion: "2.0", Manufacturer: "Publisher"}}}},
		Metadata: raw(object{"derive": object{"msi": "installer"}, "unmanaged": []any{"msiInformation.upgradeCode", "publisher"}}),
	}
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := validateMetadata(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := m["publisher"]; exists || m["msiInformation"].(object)["upgradeCode"] != nil || origins["msiInformation.upgradeCode"] != "" || origins["msiInformation.productCode"] == "" {
		t.Fatalf("unmanaged fields leaked into derived native metadata: %+v / %+v", m, origins)
	}
	for _, authored := range []object{
		{"type": "win32", "publisher": "Explicit", "unmanaged": []any{"publisher"}},
		{"type": "win32", "msiInformation": object{"upgradeCode": "Explicit"}, "unmanaged": []any{"msiInformation"}},
		{"type": "win32", "msiInformation": object{"upgradeCode": "Explicit"}, "unmanaged": []any{"msiInformation.upgradeCode"}},
		{"type": "win32", "unmanaged": []any{"misspelled"}},
	} {
		req.Metadata = raw(authored)
		if _, _, err := Derive(req); err == nil {
			t.Fatal("invalid unmanaged field or authored overlap accepted")
		}
	}
}

func TestMacDerivationAllowsExplicitReplacementAndUnmanagedOS(t *testing.T) {
	req := plugin.ReconcileRequest{
		Method: "validate", Prepared: true,
		Subjects: map[string]plugin.SubjectSelector{"main": {Kind: "app"}},
		Facts:    plugin.Facts{Subjects: []plugin.Subject{{Kind: "app", App: &plugin.AppFacts{BundleID: "org.example.app", MinimumOS: "14.1"}}}},
		Metadata: raw(object{"type": "pkg", "derive": object{"app": "main"}, "primaryBundleVersion": "explicit-version", "unmanaged": []any{"minimumSupportedOperatingSystem"}}),
	}
	derived, origins, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	m, err := validateMetadata(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if m["primaryBundleVersion"] != "explicit-version" || origins["primaryBundleVersion"] != "" || m["minimumSupportedOperatingSystem"] != nil {
		t.Fatalf("missing required fact or unrepresentable unmanaged OS defeated explicit ownership: %+v / %+v", m, origins)
	}
}
