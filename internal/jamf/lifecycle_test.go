package jamf

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	titles "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/patch_software_title_configurations"
	"github.com/woodleighschool/stemma/plugin"
)

func TestRetentionKeepsNewestFamilyMembersByNumericID(t *testing.T) {
	server, request := newNativeFixture(t)
	server.nextPackage = 7
	server.seed(map[string]any{"fileName": "other.pkg", "notes": "Administrator package"})
	server.seed(map[string]any{"fileName": "vendor-0.8.pkg", "notes": fixtureMarker})
	server.seed(map[string]any{"fileName": "vendor-0.9.pkg", "notes": "Published by another runner\n" + fixtureMarker})
	server.seed(map[string]any{"fileName": "vendor-0.7.pkg", "notes": "copied from " + fixtureMarker + " by an operator"})
	request.Metadata = raw(map[string]any{"retention": map[string]int{"keep": 2}})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil || !slices.Equal(deletions(plan), []string{"9"}) {
		t.Fatalf("retention plan: %+v: %v", plan, err)
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("plan wrote to Jamf: %v", writes)
	}
	request.Method = "apply"
	response, err := Handle(t.Context(), request)
	if err != nil || !slices.Equal(deletions(response), []string{"9"}) {
		t.Fatalf("retention apply: %+v: %v", response, err)
	}
	if server.exists("9") || !server.exists("10") || !server.exists("12") {
		t.Fatal("retention did not keep the current package and the newest other by numeric ID")
	}
	request.Metadata = raw(map[string]any{"retention": map[string]int{"keep": 1}})
	response, err = Handle(t.Context(), request)
	if err != nil || !slices.Equal(deletions(response), []string{"10"}) {
		t.Fatalf("narrowed retention: %+v: %v", response, err)
	}
	if !server.exists("8") || !server.exists("11") || !server.exists("12") {
		t.Fatal("retention deleted a package outside the family or the current record")
	}
	if response, err = Handle(t.Context(), request); err != nil || len(response.Changes) != 0 {
		t.Fatalf("retention was not idempotent: %+v: %v", response, err)
	}
}

func TestRetentionProtectsEveryNativeReference(t *testing.T) {
	for _, kind := range []string{"disabled_policy", "prestage", "patch_title"} {
		t.Run(kind, func(t *testing.T) {
			server, request := newNativeFixture(t)
			server.seed(map[string]any{"fileName": "vendor-0.8.pkg", "notes": fixtureMarker})
			server.seed(map[string]any{"fileName": "vendor-0.9.pkg", "notes": fixtureMarker})
			switch kind {
			case "disabled_policy":
				server.native.seedPolicy("20", "Administrator policy", "1")
			case "prestage":
				server.native.prestagePackages = []string{"1"}
			case "patch_title":
				server.native.titles["6"] = &titles.ResourcePatchSoftwareTitleConfiguration{ID: "6", Packages: []titles.SubsetPackage{{PackageID: "1", Version: "0.8"}}}
			}
			request.Metadata = raw(map[string]any{"retention": map[string]int{"keep": 1}})
			for _, method := range []string{"plan", "apply"} {
				request.Method = method
				response, err := Handle(t.Context(), request)
				if err != nil || !slices.Equal(deletions(response), []string{"2"}) {
					t.Fatalf("%s with a referenced package: %+v: %v", method, response, err)
				}
			}
			if !server.exists("1") || server.exists("2") || server.count("DELETE "+packagePath+"/1") != 0 {
				t.Fatalf("deleted package referenced by %s", kind)
			}
		})
	}
}

func TestRetentionBlocksWhileReferencesCannotBeRead(t *testing.T) {
	server, request := newNativeFixture(t)
	server.seed(map[string]any{"fileName": "vendor-0.9.pkg", "notes": fixtureMarker})
	server.native.denyPolicies = true
	request.Metadata = raw(map[string]any{"retention": map[string]int{"keep": 1}})
	for _, method := range []string{"plan", "apply"} {
		request.Method = method
		if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "retention blocked") {
			t.Fatalf("%s pruned without reference visibility: %v", method, err)
		}
	}
	if !server.exists("1") || server.count("DELETE "+packagePath+"/1") != 0 {
		t.Fatal("blocked retention deleted content")
	}
	if stringField(server.record("2"), "sha256") != request.Artifact.SHA256 {
		t.Fatal("blocked retention held back the publication")
	}
}

func TestPatchPublicationAdvancesAssociationAndPolicy(t *testing.T) {
	server, request := newPatchFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native := server.native
	policy := native.patchPolicies["10"]
	if policy.value("general", "name") != "Managed rollout" || policy.value("general", "enabled") != "true" || policy.value("scope", "computer_groups", "computer_group", "id") != "3" || policy.value("general", "target_version") != "1.0" {
		t.Fatal("native policy settings were not applied")
	}
	steps := server.operations()
	if slices.Index(steps, "POST "+packagePath+"/1/upload") > slices.Index(steps, "PATCH "+titlePath+"/5") {
		t.Fatal("patch association activated before package upload")
	}
	writes := len(server.writes())
	for _, method := range []string{"plan", "apply"} {
		request.Method = method
		if response, err := Handle(t.Context(), request); err != nil || len(response.Changes) != 0 {
			t.Fatalf("unchanged patch %s: %+v: %v", method, response, err)
		}
	}
	if len(server.writes()) != writes {
		t.Fatalf("unchanged patch publication wrote to Jamf: %v", server.writes()[writes:])
	}
	request.Artifact = fixtureArtifactNamed(t, "vendor-2.0.pkg", "new payload")
	request.Artifact.Version = "2.0"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if !sameAssociations(native.titles["5"].Packages, []titles.SubsetPackage{{PackageID: "1", Version: "1.0"}, {PackageID: "2", Version: "2.0"}}) {
		t.Fatalf("title associations: %+v", native.titles["5"].Packages)
	}
	if native.patchPolicies["10"].value("general", "target_version") != "2.0" || server.count("POST "+policyPath+"/softwaretitleconfig/id/5") != 1 {
		t.Fatal("new version did not advance the one policy")
	}
}

func TestPatchAssociationReplacesExistingLink(t *testing.T) {
	server, request := newPatchFixture(t)
	server.native.titles["5"].Packages = []titles.SubsetPackage{{PackageID: "99", Version: "1.0"}, {PackageID: "98", Version: "2.0"}}
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil || !slices.ContainsFunc(plan.Changes, func(c plugin.Change) bool { return c.Action == "associate" && equalJSON(c.Before, raw("99")) }) {
		t.Fatalf("plan did not report the replaced association: %+v: %v", plan, err)
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatalf("existing association was not replaced: %v", err)
	}
	if !sameAssociations(server.native.titles["5"].Packages, []titles.SubsetPackage{{PackageID: "1", Version: "1.0"}, {PackageID: "98", Version: "2.0"}}) {
		t.Fatalf("title associations: %+v", server.native.titles["5"].Packages)
	}
}

func TestPatchPolicyIdentity(t *testing.T) {
	for name, test := range map[string]struct {
		existing    []*xmlNode
		looseFilter bool
		policy      map[string]any
		wantID      string
		wantName    string
		wantErr     string
	}{
		"declared name updates in place":        {existing: []*xmlNode{testPolicy("3", "Managed rollout", "5", "0.9")}, policy: map[string]any{"name": "Managed rollout"}, wantID: "3", wantName: "Managed rollout"},
		"absent policy takes the software name": {policy: map[string]any{}, wantID: "10", wantName: "vendor"},
		"software name updates in place":        {existing: []*xmlNode{testPolicy("3", "vendor", "5", "0.9")}, policy: map[string]any{}, wantID: "3", wantName: "vendor"},
		"another title's name is free":          {existing: []*xmlNode{testPolicy("3", "Managed rollout", "6", "0.9")}, looseFilter: true, policy: map[string]any{"name": "Managed rollout"}, wantID: "10", wantName: "Managed rollout"},
		"duplicate names are ambiguous":         {existing: []*xmlNode{testPolicy("3", "Managed rollout", "5", "0.9"), testPolicy("4", "Managed rollout", "5", "0.9")}, policy: map[string]any{"name": "Managed rollout"}, wantErr: "multiple Jamf patch policies"},
	} {
		t.Run(name, func(t *testing.T) {
			server, request := newPatchFixture(t)
			server.native.loosePolicyFilter = test.looseFilter
			for _, policy := range test.existing {
				server.native.patchPolicies[policy.value("general", "id")] = policy
			}
			test.policy["enabled"] = true
			request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": test.policy}})
			_, err := Handle(t.Context(), request)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("policy error = %v, want %q", err, test.wantErr)
				}
				if writes := server.writes(); len(writes) != 0 {
					t.Fatalf("refused policy wrote to Jamf: %v", writes)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			policy := server.native.patchPolicies[test.wantID]
			if policy == nil || policy.value("general", "name") != test.wantName || policy.value("general", "target_version") != "1.0" || policy.value("general", "enabled") != "true" {
				t.Fatalf("policy %s did not converge: %+v", test.wantID, policy)
			}
			if created := server.count("POST " + policyPath + "/softwaretitleconfig/id/5"); (created == 1) != (test.wantID == "10") {
				t.Fatalf("policy creations: %d", created)
			}
			if len(server.native.patchPolicies) != len(test.existing)+server.count("POST "+policyPath+"/softwaretitleconfig/id/5") {
				t.Fatal("policy was duplicated")
			}
		})
	}
}

func TestLostPolicyCreateResponseIsFoundByName(t *testing.T) {
	server, request := newPatchFixture(t)
	server.native.failAfterPolicyCreate = true
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatalf("committed policy creation was not recovered: %v", err)
	}
	if server.count("POST "+policyPath+"/softwaretitleconfig/id/5") != 1 || server.native.patchPolicies["10"].value("general", "enabled") != "true" {
		t.Fatal("recovered policy was duplicated or left incomplete")
	}
}

func TestFailedPolicyUpdateConvergesOnRetry(t *testing.T) {
	server, request := newPatchFixture(t)
	server.native.failPolicyUpdate = true
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("unapplied policy settings were published")
	}
	server.native.failPolicyUpdate = false
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 1 || server.count("POST "+policyPath+"/softwaretitleconfig/id/5") != 1 || server.count("POST "+packagePath+"/1/upload") != 1 {
		t.Fatalf("retry replayed completed remote work: %v", server.writes())
	}
	if server.native.patchPolicies["10"].value("general", "enabled") != "true" {
		t.Fatal("retry did not finish the policy")
	}
}

func TestOmittedPatchLeavesDeploymentAlone(t *testing.T) {
	server, request := newPatchFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	writes := len(server.writes())
	request.Metadata = raw(map[string]any{})
	if response, err := Handle(t.Context(), request); err != nil || len(response.Changes) != 0 {
		t.Fatalf("omitted patch: %+v: %v", response, err)
	}
	if len(server.writes()) != writes || len(server.native.titles["5"].Packages) != 1 || server.native.patchPolicies["10"] == nil {
		t.Fatal("omitted patch changed an earlier deployment")
	}
}

func TestUndefinedPatchVersionPublishesThePackageAlone(t *testing.T) {
	server, request := newPatchFixture(t)
	request.Artifact.Version = "unpublished"
	var log bytes.Buffer
	ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewTextHandler(&log, nil)))
	if _, err := Handle(ctx, request); err != nil {
		t.Fatalf("undefined patch version: %v", err)
	}
	if !strings.Contains(log.String(), `Jamf patch title \"Test title\" does not define version unpublished`) || !strings.Contains(log.String(), "recent definitions: 1.0, 2.0") {
		t.Fatalf("waiting patch deployment was not reported: %s", log.String())
	}
	if server.count("POST "+packagePath) != 1 || len(server.native.titles["5"].Packages) != 0 || len(server.native.patchPolicies) != 0 {
		t.Fatalf("waiting patch deployment changed the title or its policies: %v", server.writes())
	}
}

func TestUndefinedPatchVersionStillUpdatesTheExistingPolicy(t *testing.T) {
	server, request := newPatchFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.Artifact = fixtureArtifactNamed(t, "vendor-3.0.pkg", "unpublished payload")
	request.Artifact.Version = "3.0"
	request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": map[string]any{"name": "Managed rollout", "enabled": false, "scope": map[string]any{"all_computers": false, "computer_groups": []string{}}}}})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	fields := map[string]plugin.Change{}
	for _, change := range plan.Changes {
		fields[change.Field] = change
	}
	if _, ok := fields["patch.policy.enabled"]; !ok || fields["patch.policy.scope.computer_groups"].Field == "" {
		t.Fatalf("waiting plan held back the policy's settings: %+v", plan.Changes)
	}
	if _, ok := fields["patch.policy.target_version"]; ok || fields["patch.packages"].Field != "" {
		t.Fatalf("waiting plan moved the target or the link: %+v", plan.Changes)
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	policy := server.native.patchPolicies["10"]
	if policy.value("general", "enabled") != "false" || len(policy.child("scope").child("computer_groups").Children) != 0 || policy.value("general", "target_version") != "1.0" {
		t.Fatalf("waiting deployment did not stop the existing policy on its target: %+v", policy)
	}
	if !sameAssociations(server.native.titles["5"].Packages, []titles.SubsetPackage{{PackageID: "1", Version: "1.0"}}) {
		t.Fatalf("waiting deployment changed the title: %+v", server.native.titles["5"].Packages)
	}
	assertSettled(t, server, request)
	request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": map[string]any{"name": "Pilot rollout", "enabled": true}}})
	if response, err := Handle(t.Context(), request); err != nil || len(response.Changes) != 0 || len(server.native.patchPolicies) != 1 {
		t.Fatalf("a new policy did not wait for the title: %+v: %v", response, err)
	}
}

func TestRetentionKeepsAPackageAWaitingLinkHolds(t *testing.T) {
	server, request := newPatchFixture(t)
	server.seed(map[string]any{"fileName": "vendor-3.0.pkg", "notes": fixtureMarker})
	server.native.titles["5"].Packages = []titles.SubsetPackage{{PackageID: "1", Version: "3.0"}}
	request.Artifact = fixtureArtifactNamed(t, "vendor-3.0-rebuilt.pkg", "rebuilt payload")
	request.Artifact.Version = "3.0"
	request.Metadata = patchMetadata(1)
	for _, method := range []string{"plan", "apply"} {
		request.Method = method
		response, err := Handle(t.Context(), request)
		if err != nil || len(deletions(response)) != 0 {
			t.Fatalf("%s retired the package a waiting link holds: %+v: %v", method, response, err)
		}
	}
	if !server.exists("1") || !sameAssociations(server.native.titles["5"].Packages, []titles.SubsetPackage{{PackageID: "1", Version: "3.0"}}) {
		t.Fatal("the waiting link lost its package")
	}
}

func TestPatchVersionIsCheckedBeforePublishing(t *testing.T) {
	server, request := newPatchFixture(t)
	request.Artifact.Version = ""
	if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "managed version") {
		t.Fatalf("versionless patch deployment: %v", err)
	}
	request.Artifact.Version = "1.0"
	for _, metadata := range []json.RawMessage{
		raw(map[string]any{"patch": map[string]any{"title": "Missing title"}}),
		raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": map[string]any{"scope": map[string]any{"computer_groups": []string{"Missing group"}}}}}),
	} {
		request.Metadata = metadata
		if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "jamf has no") {
			t.Fatalf("unresolved name in %s: %v", metadata, err)
		}
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("published before validating the patch deployment: %v", writes)
	}
}

func TestRepublishedVersionRetiresItsSupersededPackage(t *testing.T) {
	server, request := newPatchFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.Artifact = fixtureArtifactNamed(t, "vendor-rebuilt.pkg", "rebuilt payload")
	request.Artifact.Version = "1.0"
	request.Metadata = patchMetadata(1)
	for _, method := range []string{"plan", "apply"} {
		request.Method = method
		response, err := Handle(t.Context(), request)
		if err != nil || !slices.Equal(deletions(response), []string{"1"}) {
			t.Fatalf("%s kept the package its own association releases: %+v: %v", method, response, err)
		}
	}
	if server.exists("1") || !sameAssociations(server.native.titles["5"].Packages, []titles.SubsetPackage{{PackageID: "2", Version: "1.0"}}) {
		t.Fatal("superseded package outlived its association")
	}
}

func TestNativePatchPresenceAndScopeCollections(t *testing.T) {
	current := testPolicy("10", "Existing", "5", "1.0")
	current.child("general").set(xmlNode{XMLName: xml.Name{Local: "unowned_field"}, Text: "preserve"})
	current.child("scope").set(*jsonXML("computer_groups", raw([]map[string]int{{"id": 3}})))
	patch, err := decodePatch(map[string]json.RawMessage{"patch": raw(map[string]any{"title": "Test title", "policy": map[string]any{"enabled": false, "scope": map[string]any{"computer_groups": []string{}}}})})
	if err != nil {
		t.Fatal(err)
	}
	patch.titleID, patch.version, patch.scope = "5", "2.0", map[string][]string{"computer_groups": {}}
	settings := patch.settings(nil, nil)
	for _, s := range pending(current, settings) {
		mergeXML(current, s.fragment)
	}
	if current.value("general", "unowned_field") != "preserve" || current.value("general", "enabled") != "false" || len(current.child("scope").child("computer_groups").Children) != 0 {
		t.Fatal("partial native ownership or explicit empty list was lost")
	}
	if len(pending(current, settings)) != 0 {
		t.Fatal("merged XML does not satisfy desired fields")
	}
	// Jamf returns each scope object with its name, and may count the list.
	patch.scope["computer_groups"] = []string{"3"}
	settings = patch.settings(nil, nil)
	current.child("scope").set(xmlNode{XMLName: xml.Name{Local: "computer_groups"}, Children: []xmlNode{
		{XMLName: xml.Name{Local: "size"}, Text: "1"},
		{XMLName: xml.Name{Local: "computer_group"}, Children: []xmlNode{{XMLName: xml.Name{Local: "id"}, Text: "3"}, {XMLName: xml.Name{Local: "name"}, Text: "Managed Macs"}}},
	}})
	if len(pending(current, settings)) != 0 {
		t.Fatal("Jamf's readback of the declared scope did not settle")
	}
	for _, data := range []string{
		`{"title":"Test title","policy":{"scope":null}}`,
		`{"title":"Test title","policy":{"enabled":false,"enabled":true}}`,
		`{"title":"Test title","policy":{"target_version":"2.0"}}`,
		`{"title":"Test title","policy":{"id":"3"}}`,
		`{"title":"Test title","policy":{"scope":{"computer_groups":[{"id":3}]}}}`,
		`{"title":"Test title","policy":{"scope":{"computer_groups":[" "]}}}`,
		`{"title":"Test title","policy":{"distribution":"automatically"}}`,
		`{"title":"Test title","policy":{"distribution":"prompt"}}`,
		`{"title":"Test title","policy":{"grace_minutes":-1}}`,
		`{"title":"Test title","policy":{"distribution":"self_service","deadline_days":0}}`,
		`{"title":"Test title","policy":{"deadline_days":7}}`,
		`{"title":"Test title","policy":{"distribution":"automatic","deadline_days":7}}`,
		`{"title":"Test title","policy":{"distribution":"automatic","deadline_days":null}}`,
		`{"title":"Test title","policy":{"distribution":"self_service","reminder_days":0}}`,
		`{"title":"Test title","policy":{"reminder_days":1}}`,
		`{"title":"Test title","policy":{"distribution":"automatic","reminder_days":null}}`,
		`{"title":"Test title","policy":{"allow_downgrade":null}}`,
		`{"title":"Test title","policy":{"user_interaction":{"grace_period":{"duration":5}}}}`,
		`{"title":"Test title","policy":{"patch_unknown":null}}`,
		`{"title":"Test title","version":"1.0"}`,
		`{"title_configuration_id":"5"}`,
		`{"title":""}`,
	} {
		if _, err := decodePatch(map[string]json.RawMessage{"patch": json.RawMessage(data)}); err == nil {
			t.Fatalf("invalid patch metadata accepted: %s", data)
		}
	}
	for _, data := range []string{
		`{"title":"Test title","policy":{"distribution":"self_service","deadline_days":null,"reminder_days":null}}`,
		`{"title":"Test title","policy":{"distribution":"self_service","deadline_days":7,"reminder_days":3,"allow_downgrade":true}}`,
	} {
		if _, err := decodePatch(map[string]json.RawMessage{"patch": json.RawMessage(data)}); err != nil {
			t.Fatalf("valid patch metadata rejected: %s: %v", data, err)
		}
	}
}

func TestPatchDistributionSettlesAsJamfReadsIt(t *testing.T) {
	server, request := newPatchFixture(t)
	request.Inputs = map[string]plugin.Artifact{"icon": fixtureIcon(t)}
	policy := map[string]any{"name": "Managed rollout", "enabled": true, "distribution": "automatic", "grace_minutes": 60, "patch_unknown": true, "allow_downgrade": true}
	request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": policy}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native := server.native.patchPolicies["10"]
	if native.value("general", "distribution_method") != "prompt" || native.value("user_interaction", "grace_period", "grace_period_duration") != "60" || native.value("general", "patch_unknown") != "true" || native.value("general", "allow_downgrade") != "true" {
		t.Fatal("automatic distribution was not set as Jamf stores it")
	}
	if native.child("user_interaction").child("self_service_icon") != nil || len(server.native.icons) != 0 {
		t.Fatal("an automatic update uploaded a Self Service icon")
	}
	assertSettled(t, server, request)
	policy["distribution"], policy["deadline_days"] = "self_service", 7
	request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": policy}})
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
	if change := fields["patch.policy.distribution"]; !equalJSON(change.Before, raw("automatic")) || !equalJSON(change.After, raw("self_service")) {
		t.Fatalf("distribution plan is not in declaration terms: %+v", plan.Changes)
	}
	if _, ok := fields["patch.policy.self_service.icon"]; !ok || fields["patch.policy.deadline_days"].After == nil {
		t.Fatalf("self service plan: %+v", plan.Changes)
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native = server.native.patchPolicies["10"]
	if native.value("general", "distribution_method") != "selfservice" || native.value("user_interaction", "deadlines", "deadline_enabled") != "true" || native.value("user_interaction", "deadlines", "deadline_period") != "7" {
		t.Fatal("self service distribution and deadline were not set")
	}
	if native.value("user_interaction", "notifications", "notification_enabled") != "true" || native.value("user_interaction", "notifications", "reminders", "notification_reminder_frequency") != "1" {
		t.Fatal("self service update does not notify and remind daily")
	}
	if native.value("user_interaction", "self_service_icon", "filename") != fixtureIconName(t) || len(server.native.icons) != 1 {
		t.Fatal("self service update does not show the software's icon")
	}
	assertSettled(t, server, request)
	policy["reminder_days"] = 24
	request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": policy}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.native.patchPolicies["10"].value("user_interaction", "notifications", "reminders", "notification_reminder_frequency") != "24" {
		t.Fatal("declared reminder interval was not set")
	}
	assertSettled(t, server, request)
	policy["deadline_days"], policy["reminder_days"] = nil, nil
	request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": policy}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	native = server.native.patchPolicies["10"]
	if native.value("user_interaction", "deadlines", "deadline_enabled") != "false" || native.value("user_interaction", "notifications", "reminders", "notification_reminders_enabled") != "false" || native.value("user_interaction", "notifications", "notification_enabled") != "true" {
		t.Fatal("null deadline and reminders did not remove them, or removed the notification")
	}
	assertSettled(t, server, request)
	policy["distribution"] = "automatic"
	delete(policy, "deadline_days")
	delete(policy, "reminder_days")
	request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": policy}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatalf("returning to automatic updates: %v", err)
	}
	assertSettled(t, server, request)
}

func TestNewInstallReusesPatchPolicyIcon(t *testing.T) {
	server, request := newPatchFixture(t)
	request.Inputs = map[string]plugin.Artifact{"icon": fixtureIcon(t)}
	metadata := map[string]any{"patch": map[string]any{"title": "Test title", "policy": map[string]any{"name": "Managed rollout", "distribution": "self_service"}}}
	request.Metadata = raw(metadata)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	iconID := server.native.patchPolicies["10"].value("user_interaction", "self_service_icon", "id")
	metadata["policies"] = []any{map[string]any{"name": "Vendor install", "self_service": true}}
	request.Metadata = raw(metadata)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if got := server.native.policies["100"].value("self_service", "self_service_icon", "id"); got != iconID || server.count("POST /api/v1/icon") != 1 {
		t.Fatalf("new install icon %q did not reuse patch icon %q; uploads: %d", got, iconID, server.count("POST /api/v1/icon"))
	}
	assertSettled(t, server, request)
}

func TestScopeObjectsResolveByExactName(t *testing.T) {
	server, request := newPatchFixture(t)
	scope := map[string]any{
		"computers": []string{"LAB-02"}, "computer_groups": []string{"Staff Macs", "Staff Macs"}, "buildings": []string{"Senior Campus"}, "departments": []string{"Science"},
		"limitations": map[string]any{"network_segments": []string{"Library"}},
		"exclusions":  map[string]any{"computers": []string{"LAB-01"}, "computer_groups": []string{"Managed Macs"}},
	}
	request.Metadata = raw(map[string]any{"patch": map[string]any{"title": "Test title", "policy": map[string]any{"name": "Managed rollout", "enabled": true, "scope": scope}}})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	policy := server.native.patchPolicies["10"]
	if groups := policy.child("scope").child("computer_groups"); len(groups.Children) != 1 {
		t.Fatalf("repeated scope names produced %d entries", len(groups.Children))
	}
	for _, test := range []struct {
		path []string
		want string
	}{
		{[]string{"scope", "computers", "computer", "id"}, "42"},
		{[]string{"scope", "computer_groups", "computer_group", "id"}, "4"},
		{[]string{"scope", "buildings", "building", "id"}, "21"},
		{[]string{"scope", "departments", "department", "id"}, "31"},
		{[]string{"scope", "limitations", "network_segments", "network_segment", "id"}, "51"},
		{[]string{"scope", "exclusions", "computers", "computer", "id"}, "41"},
		{[]string{"scope", "exclusions", "computer_groups", "computer_group", "id"}, "3"},
	} {
		if got := policy.value(test.path...); got != test.want {
			t.Errorf("%s = %q, want %q", strings.Join(test.path, "/"), got, test.want)
		}
	}
}

func patchMetadata(keep int) json.RawMessage {
	metadata := map[string]any{"patch": map[string]any{"title": "Test title", "policy": map[string]any{"name": "Managed rollout", "enabled": true, "scope": map[string]any{"all_computers": false, "computer_groups": []string{"Managed Macs"}}}}}
	if keep > 0 {
		metadata["retention"] = map[string]int{"keep": keep}
	}
	return raw(metadata)
}

// newNativeFixture serves the deployment objects retention reads, without
// declaring patch deployment.
func newNativeFixture(t *testing.T) (*fakeServer, plugin.ReconcileRequest[Config]) {
	t.Helper()
	server, request := newFixture(t)
	server.native = &nativeServer{titles: map[string]*titles.ResourcePatchSoftwareTitleConfiguration{"5": {ID: "5", DisplayName: "Test title", SoftwareTitleID: "50", Packages: []titles.SubsetPackage{}}}, patchPolicies: map[string]*xmlNode{}, policies: map[string]*xmlNode{}, icons: map[string]string{}}
	return server, request
}
func newPatchFixture(t *testing.T) (*fakeServer, plugin.ReconcileRequest[Config]) {
	t.Helper()
	server, request := newNativeFixture(t)
	request.Metadata = patchMetadata(0)
	request.Artifact.Version = "1.0"
	return server, request
}

// deletions returns the package IDs that retention reported deleting.
func deletions(response plugin.ReconcileResponse) []string {
	var ids []string
	for _, change := range response.Changes {
		if change.Kind == "retention" && change.Action == "delete" {
			var id string
			_ = json.Unmarshal(change.Before, &id)
			ids = append(ids, id)
		}
	}
	return ids
}

type nativeServer struct {
	titles                map[string]*titles.ResourcePatchSoftwareTitleConfiguration
	patchPolicies         map[string]*xmlNode
	createdPolicies       int
	createdInstalls       int
	prestagePackages      []string
	denyPolicies          bool
	failPolicyUpdate      bool
	failAfterPolicyCreate bool
	// loosePolicyFilter lists every title's policies, as a filter the provider
	// cannot rely on would.
	loosePolicyFilter bool
	// policies are ordinary policies by ID, and icons the uploaded icon file
	// names by ID.
	policies map[string]*xmlNode
	icons    map[string]string
}

// seedPolicy stores an ordinary policy that installs packages.
func (n *nativeServer) seedPolicy(id, name string, packages ...string) {
	installs := []map[string]string{}
	for _, pkg := range packages {
		installs = append(installs, map[string]string{"id": pkg, "action": "Install"})
	}
	n.policies[id] = jsonXML("policy", raw(map[string]any{"general": map[string]any{"id": id, "name": name, "enabled": false, "frequency": "Once per computer"}, "scope": map[string]bool{"all_computers": false}, "package_configuration": map[string]any{"packages": installs}}))
}

// enrich adds the names and icon file names Jamf returns beside the IDs a
// declaration writes. Like Jamf, it shows a patch policy's Self Service
// settings only while it offers the update there.
func (n *nativeServer) enrich(s *fakeServer, doc *xmlNode) *xmlNode {
	if doc.XMLName.Local == "patch_policy" && doc.value("general", "distribution_method") != "selfservice" {
		if interaction := doc.child("user_interaction"); interaction != nil {
			doc = &xmlNode{XMLName: doc.XMLName, Children: slices.Clone(doc.Children)}
			grace := interaction.child("grace_period")
			doc.set(xmlNode{XMLName: interaction.XMLName})
			if grace != nil {
				doc.child("user_interaction").set(*grace)
			}
		}
	}
	for _, path := range [][]string{{"self_service", "self_service_icon"}, {"user_interaction", "self_service_icon"}} {
		icon := doc.child(path[0]).child(path[1])
		if icon != nil && icon.value("id") != "" {
			icon.set(xmlNode{XMLName: xml.Name{Local: "filename"}, Text: n.icons[icon.value("id")]})
		}
	}
	if category := doc.child("general").child("category"); category != nil && doc.XMLName.Local == "policy" {
		name := s.categories[category.value("id")]
		if category.value("id") == "-1" {
			name = "No category assigned"
		}
		category.set(xmlNode{XMLName: xml.Name{Local: "name"}, Text: name})
	}
	if categories := doc.child("self_service").child("self_service_categories"); categories != nil {
		for i := range categories.Children {
			if category := &categories.Children[i]; category.XMLName.Local == "category" {
				category.set(xmlNode{XMLName: xml.Name{Local: "name"}, Text: s.categories[category.value("id")]})
			}
		}
	}
	if packages := doc.child("package_configuration").child("packages"); packages != nil {
		for i := range packages.Children {
			if pkg := &packages.Children[i]; pkg.XMLName.Local == "package" {
				pkg.set(xmlNode{XMLName: xml.Name{Local: "name"}, Text: stringField(s.packages[pkg.value("id")], "fileName")})
			}
		}
	}
	return doc
}

func (n *nativeServer) handle(s *fakeServer, w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == titlePath:
		result := []*titles.ResourcePatchSoftwareTitleConfiguration{}
		for _, title := range n.titles {
			result = append(result, title)
		}
		writeJSON(s.t, w, result)
	case strings.HasPrefix(r.URL.Path, titlePath+"/"):
		path := strings.Split(strings.TrimPrefix(r.URL.Path, titlePath+"/"), "/")
		title := n.titles[path[0]]
		if title == nil {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if len(path) == 2 && path[1] == "definitions" {
			writeJSON(s.t, w, map[string]any{"totalCount": 2, "results": []map[string]string{{"version": "1.0"}, {"version": "2.0"}}})
			return true
		}
		if r.Method == http.MethodPatch {
			// Jamf answers 415 to anything but a JSON merge patch here.
			if r.Header.Get("Content-Type") != "application/merge-patch+json" {
				w.WriteHeader(http.StatusUnsupportedMediaType)
				return true
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				s.t.Error(err)
			}
			fields, err := decodeObject(data)
			if err != nil {
				s.t.Error(err)
			}
			if len(fields) != 1 || fields["packages"] == nil {
				s.t.Error("association PATCH changed unrelated title fields")
			}
			if err := json.Unmarshal(fields["packages"], &title.Packages); err != nil {
				s.t.Error(err)
			}
		}
		titleData := map[string]any{"id": title.ID, "displayName": title.DisplayName, "softwareTitleId": title.SoftwareTitleID, "packages": title.Packages}
		if title.Packages == nil {
			titleData["packages"] = []any{}
		}
		writeJSON(s.t, w, titleData)
	case r.URL.Path == "/api/v1/buildings" || r.URL.Path == "/api/v1/departments" || r.URL.Path == "/api/v4/computers-inventory":
		objects := map[string]map[string]string{
			"/api/v1/buildings":           {"21": "Senior Campus", "22": "Junior Campus"},
			"/api/v1/departments":         {"31": "Science"},
			"/api/v4/computers-inventory": {"41": "LAB-01", "42": "LAB-02"},
		}[r.URL.Path]
		field := "name"
		if r.URL.Path == "/api/v4/computers-inventory" {
			field = "general.name"
			if r.URL.Query().Get("section") != "GENERAL" {
				s.t.Error("computer lookup omitted its name section")
			}
		}
		name, _ := strings.CutPrefix(r.URL.Query().Get("filter"), field+"==")
		name, _ = strconv.Unquote(name)
		results := []map[string]any{}
		for id, object := range objects {
			if object == name {
				row := map[string]any{"id": id, "name": object}
				if field == "general.name" {
					row = map[string]any{"id": id, "general": map[string]string{"name": object}}
				}
				results = append(results, row)
			}
		}
		writeJSON(s.t, w, map[string]any{"totalCount": len(results), "results": results})
	case r.URL.Path == "/JSSResource/networksegments":
		writeXML(s.t, w, jsonXML("network_segments", raw(map[string]any{"size": 1, "network_segment": map[string]any{"id": 51, "name": "Library"}})))
	case r.URL.Path == "/api/v1/computer-groups":
		writeJSON(s.t, w, []map[string]any{{"id": "3", "name": "Managed Macs", "smartGroup": true}, {"id": "4", "name": "Staff Macs", "smartGroup": false}})
	case r.URL.Path == "/api/v2/patch-policies":
		results := []map[string]any{}
		for id, p := range n.patchPolicies {
			title := p.value("software_title_configuration_id")
			if !n.loosePolicyFilter && r.URL.Query().Get("filter") != "softwareTitleConfigurationId=="+title {
				continue
			}
			results = append(results, map[string]any{"id": id, "policyName": p.value("general", "name"), "softwareTitleConfigurationId": title, "policyTargetVersion": p.value("general", "target_version")})
		}
		writeJSON(s.t, w, map[string]any{"totalCount": len(results), "results": results})
	case strings.HasPrefix(r.URL.Path, policyPath+"/softwaretitleconfig/id/") && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		p, err := parseXML(body, "patch_policy")
		if err != nil {
			s.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		if p.value("general", "enabled") != "false" || p.value("scope", "all_computers") != "false" || len(p.child("scope").Children) != 1 {
			s.t.Error("created an active/scoped patch policy")
		}
		id := strconv.Itoa(10 + n.createdPolicies)
		n.createdPolicies++
		p.child("general").set(xmlNode{XMLName: xml.Name{Local: "id"}, Text: id})
		n.patchPolicies[id] = p
		if n.failAfterPolicyCreate {
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		w.WriteHeader(http.StatusCreated)
		writeXML(s.t, w, &xmlNode{XMLName: xml.Name{Local: "patch_policy"}, Children: []xmlNode{{XMLName: xml.Name{Local: "id"}, Text: id}}})
	case strings.HasPrefix(r.URL.Path, policyPath+"/id/"):
		id := strings.TrimPrefix(r.URL.Path, policyPath+"/id/")
		p := n.patchPolicies[id]
		if p == nil {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		if r.Method == http.MethodPut {
			if n.failPolicyUpdate {
				w.WriteHeader(http.StatusInternalServerError)
				return true
			}
			body, _ := io.ReadAll(r.Body)
			updated, err := parseXML(body, "patch_policy")
			if err != nil {
				s.t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return true
			}
			n.patchPolicies[id], p = updated, updated
		}
		writeXML(s.t, w, n.enrich(s, p))
	case r.URL.Path == "/JSSResource/policies":
		if n.denyPolicies {
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		listed := &xmlNode{XMLName: xml.Name{Local: "policies"}, Children: []xmlNode{{XMLName: xml.Name{Local: "size"}, Text: strconv.Itoa(len(n.policies))}}}
		for _, id := range slices.Sorted(maps.Keys(n.policies)) {
			listed.Children = append(listed.Children, xmlNode{XMLName: xml.Name{Local: "policy"}, Children: []xmlNode{{XMLName: xml.Name{Local: "id"}, Text: id}, {XMLName: xml.Name{Local: "name"}, Text: n.policies[id].value("general", "name")}}})
		}
		writeXML(s.t, w, listed)
	case strings.HasPrefix(r.URL.Path, "/JSSResource/policies/id/"):
		id := strings.TrimPrefix(r.URL.Path, "/JSSResource/policies/id/")
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			p, err := parseXML(body, "policy")
			if err != nil {
				s.t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return true
			}
			switch {
			case r.Method == http.MethodPost:
				if id != "0" || p.value("general", "enabled") != "false" || p.value("scope", "all_computers") != "false" || len(p.child("scope").Children) != 1 {
					s.t.Error("created an active/scoped policy")
				}
				id = strconv.Itoa(100 + n.createdInstalls)
				n.createdInstalls++
			case n.policies[id] == nil:
				w.WriteHeader(http.StatusNotFound)
				return true
			case n.failPolicyUpdate:
				w.WriteHeader(http.StatusInternalServerError)
				return true
			}
			p.child("general").set(xmlNode{XMLName: xml.Name{Local: "id"}, Text: id})
			// Jamf resolves a category reference by its name before its ID.
			if category := p.child("general").child("category"); category.value("name") != "" {
				for known, name := range s.categories {
					if name == category.value("name") {
						category.set(xmlNode{XMLName: xml.Name{Local: "id"}, Text: known})
					}
				}
			}
			n.policies[id] = p
			if r.Method == http.MethodPost {
				if n.failAfterPolicyCreate {
					w.WriteHeader(http.StatusInternalServerError)
					return true
				}
				w.WriteHeader(http.StatusCreated)
				writeXML(s.t, w, &xmlNode{XMLName: xml.Name{Local: "policy"}, Children: []xmlNode{{XMLName: xml.Name{Local: "id"}, Text: id}}})
				return true
			}
		}
		p := n.policies[id]
		if p == nil {
			w.WriteHeader(http.StatusNotFound)
			return true
		}
		writeXML(s.t, w, n.enrich(s, p))
	case r.URL.Path == "/api/v1/icon" && r.Method == http.MethodPost:
		reader, err := r.MultipartReader()
		if err != nil {
			s.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		part, err := reader.NextPart()
		if err != nil || part.FormName() != "file" {
			s.t.Error("icon upload has no file")
			w.WriteHeader(http.StatusBadRequest)
			return true
		}
		id := 500 + len(n.icons)
		n.icons[strconv.Itoa(id)] = part.FileName()
		writeJSON(s.t, w, map[string]any{"id": id, "url": "https://ics.example/icon/hash_reencoded", "name": part.FileName()})
	case r.URL.Path == "/api/v3/computer-prestages":
		results := []map[string]any{}
		if len(n.prestagePackages) > 0 {
			results = append(results, map[string]any{"id": "30", "customPackageIds": n.prestagePackages})
		}
		writeJSON(s.t, w, map[string]any{"totalCount": len(results), "results": results})
	default:
		return false
	}
	return true
}
func testPolicy(id, name, title, version string) *xmlNode {
	return jsonXML("patch_policy", raw(map[string]any{"general": map[string]any{"id": id, "name": name, "target_version": version, "enabled": false}, "scope": map[string]bool{"all_computers": false}, "software_title_configuration_id": title}))
}
func writeXML(t *testing.T, w http.ResponseWriter, value *xmlNode) {
	t.Helper()
	w.Header().Set("Content-Type", "application/xml")
	data, err := xml.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprint(w, string(data)); err != nil {
		t.Error(err)
	}
}
