package jamf

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
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
				server.native.regularPackages = []string{"1"}
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
	request.Metadata = patchMetadata("2.0", 0)
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
		"declared id selects and renames":       {existing: []*xmlNode{testPolicy("3", "Legacy rollout", "5", "0.9")}, policy: map[string]any{"id": "3", "name": "Managed rollout"}, wantID: "3", wantName: "Managed rollout"},
		"declared id must exist":                {policy: map[string]any{"id": "3"}, wantErr: "does not exist"},
		"declared id must belong to the title":  {existing: []*xmlNode{testPolicy("3", "Elsewhere", "6", "0.9")}, policy: map[string]any{"id": "3"}, wantErr: "different title configuration"},
	} {
		t.Run(name, func(t *testing.T) {
			server, request := newPatchFixture(t)
			server.native.loosePolicyFilter = test.looseFilter
			for _, policy := range test.existing {
				server.native.patchPolicies[policy.value("general", "id")] = policy
			}
			test.policy["enabled"] = true
			request.Metadata = raw(map[string]any{"patch": map[string]any{"title_configuration_id": "5", "version": "1.0", "policy": test.policy}})
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

func TestPatchVersionIsCheckedBeforePublishing(t *testing.T) {
	server, request := newPatchFixture(t)
	request.Metadata = patchMetadata("unpublished", 0)
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("undefined patch version accepted")
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("published before validating the patch version: %v", writes)
	}
}

func TestRepublishedVersionRetiresItsSupersededPackage(t *testing.T) {
	server, request := newPatchFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.Artifact = fixtureArtifactNamed(t, "vendor-rebuilt.pkg", "rebuilt payload")
	request.Metadata = patchMetadata("1.0", 1)
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
	patch, err := decodePatch(map[string]json.RawMessage{"patch": raw(map[string]any{"title_configuration_id": "5", "version": "2.0", "policy": map[string]any{"enabled": false, "scope": map[string]any{"computer_groups": []any{}}}})})
	if err != nil {
		t.Fatal(err)
	}
	desired := desiredPolicy(patch, "Existing", false)
	mergeXML(current, desired)
	if current.value("general", "unowned_field") != "preserve" || current.value("general", "enabled") != "false" || len(current.child("scope").child("computer_groups").Children) != 0 {
		t.Fatal("partial native ownership or explicit empty list was lost")
	}
	if !containsXML(current, desired) {
		t.Fatal("merged XML does not satisfy desired fields")
	}
	for _, data := range []string{`{"title_configuration_id":"5","version":"1.0","policy":{"scope":null}}`, `{"title_configuration_id":"5","version":"1.0","policy":{"enabled":false,"enabled":true}}`, `{"title_configuration_id":"5","version":"1.0","policy":{"target_version":"2.0"}}`} {
		if _, err := decodePatch(map[string]json.RawMessage{"patch": json.RawMessage(data)}); err == nil {
			t.Fatalf("invalid patch metadata accepted: %s", data)
		}
	}
}

func patchMetadata(version string, keep int) json.RawMessage {
	metadata := map[string]any{"patch": map[string]any{"title_configuration_id": "5", "version": version, "policy": map[string]any{"name": "Managed rollout", "enabled": true, "scope": map[string]any{"all_computers": false, "computer_groups": []map[string]int{{"id": 3}}}}}}
	if keep > 0 {
		metadata["retention"] = map[string]int{"keep": keep}
	}
	return raw(metadata)
}

// newNativeFixture serves the deployment objects retention reads, without
// declaring patch deployment.
func newNativeFixture(t *testing.T) (*fakeServer, plugin.ReconcileRequest) {
	t.Helper()
	server, request := newFixture(t)
	server.native = &nativeServer{titles: map[string]*titles.ResourcePatchSoftwareTitleConfiguration{"5": {ID: "5", DisplayName: "Test title", SoftwareTitleID: "50", Packages: []titles.SubsetPackage{}}}, patchPolicies: map[string]*xmlNode{}}
	return server, request
}
func newPatchFixture(t *testing.T) (*fakeServer, plugin.ReconcileRequest) {
	t.Helper()
	server, request := newNativeFixture(t)
	request.Metadata = patchMetadata("1.0", 0)
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
	regularPackages       []string
	prestagePackages      []string
	denyPolicies          bool
	failPolicyUpdate      bool
	failAfterPolicyCreate bool
	// loosePolicyFilter lists every title's policies, as a filter the provider
	// cannot rely on would.
	loosePolicyFilter bool
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
			fields := s.readObject(r)
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
		writeXML(s.t, w, p)
	case r.URL.Path == "/JSSResource/policies":
		if n.denyPolicies {
			w.WriteHeader(http.StatusForbidden)
			return true
		}
		policies := &xmlNode{XMLName: xml.Name{Local: "policies"}}
		count := 0
		if len(n.regularPackages) > 0 {
			count = 1
			policies.Children = append(policies.Children, xmlNode{XMLName: xml.Name{Local: "policy"}, Children: []xmlNode{{XMLName: xml.Name{Local: "id"}, Text: "20"}}})
		}
		policies.Children = append(policies.Children, xmlNode{XMLName: xml.Name{Local: "size"}, Text: strconv.Itoa(count)})
		writeXML(s.t, w, policies)
	case r.URL.Path == "/JSSResource/policies/id/20":
		packages := []map[string]string{}
		for _, id := range n.regularPackages {
			packages = append(packages, map[string]string{"id": id})
		}
		writeXML(s.t, w, jsonXML("policy", raw(map[string]any{"general": map[string]any{"id": 20, "enabled": false}, "scope": map[string]bool{"all_computers": false}, "package_configuration": map[string]any{"packages": packages}})))
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
