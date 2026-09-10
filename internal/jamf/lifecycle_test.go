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

func TestPatchPublicationAndRetention(t *testing.T) {
	server, request := newPatchFixture(t)
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var state binding
	decodeBinding(t, response.Binding, &state)
	if state.PolicyID != "10" || state.Publications.Sequence != 1 || len(state.Associations) != 1 {
		t.Fatalf("incomplete publication binding: %+v", state)
	}
	native := server.native
	if native.patchPolicies["10"].value("general", "enabled") != "true" || native.patchPolicies["10"].value("scope", "computer_groups", "computer_group", "id") != "3" {
		t.Fatal("native policy settings were not applied")
	}
	if native.patchPolicies["10"].value("general", "target_version") != "1.0" {
		t.Fatal("policy version was not linked")
	}
	before := server.operations()
	if slices.Index(before, "POST "+packagePath+"/1/upload") > slices.Index(before, "PATCH "+titlePath+"/5") {
		t.Fatal("patch association activated before package upload")
	}
	request.Binding = response.Binding
	response, err = Handle(t.Context(), request)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("unchanged patch publication: %+v: %v", response, err)
	}
	request.Artifact = fixtureArtifact(t, "new payload")
	request.Metadata = patchMetadata("2.0", 1)
	response, err = Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	decodeBinding(t, response.Binding, &state)
	if state.PolicyID != "10" || state.Publications.Sequence != 2 || len(state.Revisions) != 1 || len(state.Associations) != 1 {
		t.Fatalf("wrong retained state: %+v", state)
	}
	if _, ok := server.packages["1"]; ok {
		t.Fatal("unreferenced historical package survived retention")
	}
	if len(native.titles["5"].Packages) != 1 || native.titles["5"].Packages[0].PackageID != "2" {
		t.Fatal("historical owned association survived retention")
	}
	if native.patchPolicies["10"].value("general", "target_version") != "2.0" {
		t.Fatal("durable policy did not advance")
	}
	if server.count("POST "+policyPath+"/softwaretitleconfig/id/5") != 1 {
		t.Fatal("new version duplicated the policy")
	}
	request.Binding = response.Binding
	if response, err = Handle(t.Context(), request); err != nil || len(response.Changes) != 0 {
		t.Fatalf("cleanup was not idempotent: %+v: %v", response, err)
	}
}

func TestRetentionProtectsEveryNativeReference(t *testing.T) {
	for _, kind := range []string{"disabled_policy", "prestage", "other_title", "patch_policy"} {
		t.Run(kind, func(t *testing.T) {
			server, request := newPatchFixture(t)
			response, err := Handle(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "disabled_policy":
				server.native.regularPackages = []string{"1"}
			case "prestage":
				server.native.prestagePackages = []string{"1"}
			case "other_title":
				server.native.titles["6"] = &titles.ResourcePatchSoftwareTitleConfiguration{ID: "6", Packages: []titles.SubsetPackage{{PackageID: "1", Version: "1.0"}}}
			case "patch_policy":
				server.native.patchPolicies["11"] = testPolicy("11", "Older deployment", "5", "1.0")
			}
			request.Binding = response.Binding
			request.Artifact = fixtureArtifact(t, "second payload")
			request.Metadata = patchMetadata("2.0", 1)
			response, err = Handle(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if server.packages["1"] == nil || server.count("DELETE "+packagePath+"/1") != 0 {
				t.Fatalf("deleted package referenced by %s", kind)
			}
			if len(server.native.titles["5"].Packages) != 2 {
				t.Fatal("a protected deployment association was retired")
			}
			if !slices.ContainsFunc(response.Changes, func(c plugin.Change) bool { return c.Action == "retain_referenced" }) {
				t.Fatal("plan did not report reference protection")
			}
		})
	}
}

func TestFailedPolicyUpdateRetainsPublicationProgress(t *testing.T) {
	server, request := newPatchFixture(t)
	server.native.failPolicyUpdate = true
	response, err := Handle(t.Context(), request)
	if err == nil {
		t.Fatal("unapplied policy settings were published")
	}
	var state binding
	decodeBinding(t, response.Binding, &state)
	if state.Publications.Sequence != 0 || state.PolicyID != "10" || len(state.Associations) != 1 || state.PackageID != "1" {
		t.Fatalf("partial progress was lost: %+v", state)
	}
	request.Binding = response.Binding
	server.native.failPolicyUpdate = false
	response, err = Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	decodeBinding(t, response.Binding, &state)
	if state.Publications.Sequence != 1 || server.count("POST "+packagePath) != 1 || server.count("POST "+policyPath+"/softwaretitleconfig/id/5") != 1 || server.count("POST "+packagePath+"/1/upload") != 1 {
		t.Fatal("retry replayed completed remote work")
	}
}

func TestFailedUploadCleansStagingAndPreservesPublishedPatch(t *testing.T) {
	for _, phase := range []string{"request", "transfer"} {
		t.Run(phase, func(t *testing.T) {
			server, request := newPatchFixture(t)
			response, err := Handle(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			published := request.Artifact.SHA256
			request.Binding = response.Binding
			request.Artifact = fixtureArtifact(t, "failed second payload")
			request.Metadata = patchMetadata("2.0", 0)
			server.failBeforeUpload, server.failAfter = true, phase
			response, err = Handle(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), "failed staging package 2 deleted") {
				t.Fatalf("staging cleanup was not reported: %v", err)
			}
			var state binding
			decodeBinding(t, response.Binding, &state)
			if state.PackageID != "1" || state.PayloadSHA256 != published || state.Publications.Sequence != 1 || len(state.Revisions) != 1 {
				t.Fatalf("failed upload changed the successful publication: %+v", state)
			}
			if server.packages["1"] == nil || server.packages["2"] != nil || server.count("DELETE "+packagePath+"/2") != 1 {
				t.Fatal("failed staging cleanup affected the wrong package")
			}
			if len(server.native.titles["5"].Packages) != 1 || server.native.titles["5"].Packages[0].PackageID != "1" || server.native.patchPolicies["10"].value("general", "target_version") != "1.0" {
				t.Fatal("failed upload changed published patch links")
			}
		})
	}
}

func TestFailedUploadReportsProtectedStaging(t *testing.T) {
	for _, protection := range []string{"policy", "prestage", "title", "visibility", "delete", "unknown", "adopted", "recovered", "published"} {
		t.Run(protection, func(t *testing.T) {
			server, request := newPatchFixture(t)
			response, err := Handle(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			request.Binding = response.Binding
			first := request.Artifact
			request.Artifact = fixtureArtifact(t, "failed second payload")
			request.Metadata = patchMetadata("2.0", 0)
			server.failBeforeUpload = true
			id, reason := "2", "referenced"
			switch protection {
			case "policy":
				server.native.regularPackages = []string{id}
			case "prestage":
				server.native.prestagePackages = []string{id}
			case "title":
				server.native.titles["6"] = &titles.ResourcePatchSoftwareTitleConfiguration{ID: "6", Packages: []titles.SubsetPackage{{PackageID: id, Version: "2.0"}}}
			case "visibility":
				server.native.denyPolicies, reason = true, "visibility is incomplete"
			case "delete":
				server.denyDelete, reason = true, "deletion failed"
			case "unknown":
				server.uploadStatus, reason = "UNKNOWN", "not definitively failed"
			case "recovered":
				server.failAfter, reason = "create", "creation ownership is unknown"
			case "adopted":
				content, err := inspectPayload(t.Context(), request.Identity, request.Artifact)
				if err != nil {
					t.Fatal(err)
				}
				pkg := defaults(content.filename, request.Artifact.Filename)
				pkg["id"], pkg["cloudTransferStatus"] = raw(id), raw("AWAITING_UPLOAD")
				server.packages[id], server.nextPackage = pkg, 2
				metadata, err := decodeObject(request.Metadata)
				if err != nil {
					t.Fatal(err)
				}
				metadata["package_id"] = raw(id)
				request.Metadata, reason = raw(metadata), "adopted"
			case "published":
				request.Artifact, request.Metadata = first, patchMetadata("1.0", 0)
				server.set("cloudTransferStatus", raw("FAILED"))
				id, reason = "1", "successful publication"
			}
			response, err = Handle(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), "staging package "+id+" retained:") || !strings.Contains(err.Error(), reason) {
				t.Fatalf("retained package ID and reason were not reported: %v", err)
			}
			var state binding
			decodeBinding(t, response.Binding, &state)
			if state.PackageID != "1" || state.PayloadSHA256 != first.SHA256 || state.Publications.Sequence != 1 || state.Revisions[request.Artifact.SHA256].PackageID != id || server.packages[id] == nil {
				t.Fatalf("staging or prior publication was lost: %+v", state)
			}
			if protection != "delete" && server.count("DELETE "+packagePath+"/"+id) != 0 {
				t.Fatal("attempted to delete protected content")
			}
			if protection == "recovered" && state.Revisions[request.Artifact.SHA256].Owned {
				t.Fatal("ambiguous creation established deletion ownership")
			}
		})
	}
}

func TestPruningBlocksOnIncompleteVisibilityAndUnknownOrder(t *testing.T) {
	for _, kind := range []string{"visibility", "binding_loss", "changed_association"} {
		t.Run(kind, func(t *testing.T) {
			server, request := newPatchFixture(t)
			response, err := Handle(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			request.Binding = response.Binding
			request.Artifact = fixtureArtifact(t, "second payload")
			request.Metadata = patchMetadata("2.0", 1)
			switch kind {
			case "visibility":
				server.native.denyPolicies = true
			case "binding_loss":
				request.Binding = nil
				request.Metadata = raw(map[string]any{"retention": map[string]int{"keep": 1}})
			case "changed_association":
				server.native.titles["5"].Packages[0].PackageID = "99"
			}
			response, err = Handle(t.Context(), request)
			if err == nil || !strings.Contains(err.Error(), "pruning blocked") {
				t.Fatalf("unsafe prune did not block: %v", err)
			}
			if server.count("DELETE "+packagePath+"/1") != 0 || server.packages["1"] == nil {
				t.Fatal("blocked prune deleted content")
			}
			var state binding
			decodeBinding(t, response.Binding, &state)
			if state.PayloadSHA256 != request.Artifact.SHA256 || state.Publications.Current != request.Artifact.SHA256 {
				t.Fatal("cleanup error discarded successful publication")
			}
		})
	}
}

func TestPatchVersionAndOwnershipAreCheckedBeforeUploading(t *testing.T) {
	server, request := newPatchFixture(t)
	request.Metadata = patchMetadata("unpublished", 0)
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("undefined patch version accepted")
	}
	if server.count("POST "+packagePath) != 0 {
		t.Fatal("created a package before validating patch version")
	}
	server.native.titles["5"].Packages = []titles.SubsetPackage{{Version: "1.0", PackageID: "99"}}
	request.Metadata = patchMetadata("1.0", 0)
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("administrator association was replaced")
	}
	if server.count("POST "+packagePath) != 0 {
		t.Fatal("created a package before checking association ownership")
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
	desired := desiredPolicy(patch, &binding{}, false)
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
func newPatchFixture(t *testing.T) (*fakeServer, plugin.ReconcileRequest) {
	server, request := newFixture(t)
	server.native = &nativeServer{titles: map[string]*titles.ResourcePatchSoftwareTitleConfiguration{"5": {ID: "5", DisplayName: "Test title", SoftwareTitleID: "50", Packages: []titles.SubsetPackage{}}}, patchPolicies: map[string]*xmlNode{}}
	request.Metadata = patchMetadata("1.0", 0)
	return server, request
}
func decodeBinding(t *testing.T, data json.RawMessage, target *binding) {
	t.Helper()
	*target = binding{}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

type nativeServer struct {
	titles                 map[string]*titles.ResourcePatchSoftwareTitleConfiguration
	patchPolicies          map[string]*xmlNode
	regularPackages        []string
	prestagePackages       []string
	denyPolicies           bool
	failPolicyUpdate       bool
	hideAtPackageCount     int
	hideTitle              bool
	targetDuringRetirement bool
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
		if title == nil || n.hideTitle {
			w.WriteHeader(404)
			return true
		}
		if len(path) == 2 && path[1] == "definitions" {
			writeJSON(s.t, w, map[string]any{"totalCount": 2, "results": []map[string]string{{"version": "1.0"}, {"version": "2.0"}}})
			return true
		}
		if r.Method == http.MethodPatch {
			oldLinks := len(title.Packages)
			fields := s.readObject(r)
			if len(fields) != 1 || fields["packages"] == nil {
				s.t.Error("association PATCH changed unrelated title fields")
			}
			if err := json.Unmarshal(fields["packages"], &title.Packages); err != nil {
				s.t.Error(err)
			}
			if n.targetDuringRetirement && len(title.Packages) < oldLinks {
				n.patchPolicies["11"] = testPolicy("11", "Concurrent deployment", "5", "1.0")
			}
			if n.hideAtPackageCount > 0 && len(title.Packages) == n.hideAtPackageCount {
				n.hideTitle = true
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
			if filter := r.URL.Query().Get("filter"); filter != "" && filter != "softwareTitleConfigurationId=="+title {
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
			w.WriteHeader(400)
			return true
		}
		if p.value("general", "enabled") != "false" || p.value("scope", "all_computers") != "false" || len(p.child("scope").Children) != 1 {
			s.t.Error("created an active/scoped patch policy")
		}
		id := "10"
		p.child("general").set(xmlNode{XMLName: xml.Name{Local: "id"}, Text: id})
		n.patchPolicies[id] = p
		w.WriteHeader(http.StatusCreated)
		writeXML(s.t, w, &xmlNode{XMLName: xml.Name{Local: "patch_policy"}, Children: []xmlNode{{XMLName: xml.Name{Local: "id"}, Text: id}}})
	case strings.HasPrefix(r.URL.Path, policyPath+"/id/"):
		id := strings.TrimPrefix(r.URL.Path, policyPath+"/id/")
		p := n.patchPolicies[id]
		if p == nil {
			w.WriteHeader(404)
			return true
		}
		if r.Method == http.MethodPut {
			if n.failPolicyUpdate {
				w.WriteHeader(500)
				return true
			}
			body, _ := io.ReadAll(r.Body)
			updated, err := parseXML(body, "patch_policy")
			if err != nil {
				s.t.Error(err)
				w.WriteHeader(400)
				return true
			}
			n.patchPolicies[id], p = updated, updated
		}
		writeXML(s.t, w, p)
	case r.URL.Path == "/JSSResource/policies":
		if n.denyPolicies {
			w.WriteHeader(403)
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

func TestAmbiguousTitleWritesRetainAssociationOwnership(t *testing.T) {
	for _, phase := range []string{"association", "retirement"} {
		t.Run(phase, func(t *testing.T) {
			server, request := newPatchFixture(t)
			if phase == "retirement" {
				response, err := Handle(t.Context(), request)
				if err != nil {
					t.Fatal(err)
				}
				request.Binding = response.Binding
				request.Artifact = fixtureArtifact(t, "second payload")
				request.Metadata = patchMetadata("2.0", 1)
			}
			server.native.hideAtPackageCount = 1
			response, err := Handle(t.Context(), request)
			if err == nil {
				t.Fatal("unverified title mutation unexpectedly succeeded")
			}
			var state binding
			decodeBinding(t, response.Binding, &state)
			if len(state.Associations) == 0 {
				t.Fatal("ambiguous title mutation lost durable ownership")
			}
			request.Binding = response.Binding
			server.native.hideAtPackageCount = 0
			server.native.hideTitle = false
			response, err = Handle(t.Context(), request)
			if err != nil {
				t.Fatalf("title recovery failed: %v", err)
			}
			decodeBinding(t, response.Binding, &state)
			if len(state.Associations) != 1 || state.Associations[0].Pending || state.Associations[0].Removing {
				t.Fatalf("title progress remained unresolved: %+v", state.Associations)
			}
			if phase == "retirement" && server.packages["1"] != nil {
				t.Fatal("retired revision was not pruned on retry")
			}
			if server.count("POST "+policyPath+"/softwaretitleconfig/id/5") != 1 {
				t.Fatal("title recovery duplicated the patch policy")
			}
		})
	}
}

func TestConcurrentPatchReferenceRestoresRetiringAssociation(t *testing.T) {
	server, request := newPatchFixture(t)
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Binding = response.Binding
	request.Artifact = fixtureArtifact(t, "second payload")
	request.Metadata = patchMetadata("2.0", 1)
	server.native.targetDuringRetirement = true
	response, err = Handle(t.Context(), request)
	if err == nil || !strings.Contains(err.Error(), "association was restored") {
		t.Fatalf("concurrent patch reference was not protected: %v", err)
	}
	if server.packages["1"] == nil || len(server.native.titles["5"].Packages) != 2 {
		t.Fatal("concurrent deployment lost its package or association")
	}
	request.Binding = response.Binding
	if _, err = Handle(t.Context(), request); err != nil {
		t.Fatalf("retained concurrent deployment could not reconcile: %v", err)
	}
}
