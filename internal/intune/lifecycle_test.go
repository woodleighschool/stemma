package intune

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func changePayload(t *testing.T, req *plugin.ReconcileRequest, content string) {
	t.Helper()
	data := []byte("@echo off\r\necho " + content + "\r\n")
	if err := os.WriteFile(req.Artifact.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	req.Artifact.SHA256 = hex.EncodeToString(digest[:])
	req.Artifact.Size = int64(len(data))
}

// retentionChanges lists, in order, the content versions a response deletes.
func retentionChanges(response plugin.ReconcileResponse) []string {
	var versions []string
	for _, change := range response.Changes {
		if change.Kind == "retention" {
			var version string
			_ = json.Unmarshal(change.Before, &version)
			versions = append(versions, version)
		}
	}
	return versions
}

func committedFile() object { return object{"id": "file-1", "isCommitted": true} }

func TestRetentionKeepsActiveAndNewestPublicationsByVersionNumber(t *testing.T) {
	fake, c := newGraphFixture(t)
	// The tenant already holds publications from before this run. Numbering new
	// versions from 10 makes a lexical order disagree with the numeric one.
	fake.versionBase = 9
	fake.files["8"], fake.files["9"] = committedFile(), committedFile()
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	desired["retention"] = object{"keep": float64(3)}
	response, err := c.handle(t.Context(), req, desired)
	if err != nil {
		t.Fatal(err)
	}
	if deleted := retentionChanges(response); len(deleted) != 0 {
		t.Fatalf("retention deleted publications within its limit: %v", deleted)
	}
	desired["retention"] = object{"keep": float64(2)}
	changePayload(t, &req, "second release")
	fake.mu.Lock()
	writes := fake.writes
	fake.mu.Unlock()
	req.Method = "plan"
	response, err = c.handle(t.Context(), req, desired)
	if err != nil {
		t.Fatal(err)
	}
	// Publishing makes version 10 the newest earlier publication.
	if planned := retentionChanges(response); !slices.Equal(planned, []string{"8", "9"}) {
		t.Fatalf("plan predicted deleting %v", planned)
	}
	fake.mu.Lock()
	if fake.writes != writes || len(fake.files) != 3 {
		t.Fatal("planning mutated content")
	}
	fake.mu.Unlock()
	req.Method = "apply"
	response, err = c.handle(t.Context(), req, desired)
	if err != nil {
		t.Fatal(err)
	}
	if deleted := retentionChanges(response); !slices.Equal(deleted, []string{"8", "9"}) {
		t.Fatalf("apply deleted %v", deleted)
	}
	fake.mu.Lock()
	if !slices.Equal(fake.deletedVersions, []string{"8", "9"}) || fake.app["committedContentVersion"] != "11" || len(fake.files) != 2 || fake.creates != 1 {
		t.Fatalf("retention kept the wrong publications: deleted %v, remaining %v", fake.deletedVersions, fake.files)
	}
	writes = fake.writes
	fake.mu.Unlock()
	response, err = c.handle(t.Context(), req, desired)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("settled retention: %+v, %v", response.Changes, err)
	}
	fake.mu.Lock()
	if fake.writes != writes {
		t.Fatal("settled retention wrote to the tenant")
	}
	fake.mu.Unlock()
	// Tightening retention prunes without publishing anything.
	desired["retention"] = object{"keep": float64(1)}
	for _, method := range []string{"plan", "apply"} {
		req.Method = method
		response, err = c.handle(t.Context(), req, desired)
		if err != nil || !slices.Equal(retentionChanges(response), []string{"10"}) {
			t.Fatalf("%s with tighter retention: %+v, %v", method, response.Changes, err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.versions != 2 || len(fake.files) != 1 || fake.app["committedContentVersion"] != "11" {
		t.Fatalf("tighter retention published or kept content: %v", fake.files)
	}
}

func TestInterruptedUploadIsAbandonedAndPruned(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	desired["retention"] = object{"keep": float64(2)}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	first := publishedMarker(t, fake)
	changePayload(t, &req, "second release")
	fake.mu.Lock()
	fake.failBlob = true
	fake.mu.Unlock()
	if _, err := c.handle(t.Context(), req, desired); err == nil {
		t.Fatal("expected interrupted upload")
	}
	// The tenant now holds uncommitted version 2 while the app still runs version 1.
	if published := publishedMarker(t, fake); published != first {
		t.Fatalf("interrupted upload changed the marker: %+v", published)
	}
	fake.mu.Lock()
	if fake.app["committedContentVersion"] != "1" || fake.files["2"]["isCommitted"] == true || len(fake.deletedVersions) != 0 {
		t.Fatalf("interrupted upload changed active content: %+v", fake.app)
	}
	fake.mu.Unlock()
	req.Method = "plan"
	response, err := c.handle(t.Context(), req, desired)
	if err != nil || !slices.Equal(retentionChanges(response), []string{"2"}) {
		t.Fatalf("plan after interruption: %+v, %v", response.Changes, err)
	}
	req.Method = "apply"
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	if published := publishedMarker(t, fake); published.content != "3" || published.payload == first.payload {
		t.Fatalf("retry did not publish fresh content: %+v", published)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// An abandoned upload is never kept in place of the earlier publication.
	if !slices.Equal(fake.deletedVersions, []string{"2"}) || len(fake.files) != 2 || fake.versions != 3 || fake.creates != 1 {
		t.Fatalf("abandoned upload was not pruned: deleted %v, remaining %v", fake.deletedVersions, fake.files)
	}
}

func TestRetentionRefusesUnorderedContentVersions(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	desired["retention"] = object{"keep": float64(1)}
	fake.files["draft"] = committedFile()
	if _, err := c.handle(t.Context(), req, desired); err == nil || !strings.Contains(err.Error(), `"draft" is not a version number`) {
		t.Fatalf("ordered content by an ID that is not a version number: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deletedVersions) != 0 {
		t.Fatal("deleted content without a publication order")
	}
}

func TestRetentionWaitsForReferencesAndRetryDoesNotUploadAgain(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	desired["retention"] = object{"keep": float64(1)}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	changePayload(t, &req, "second")
	desired["dependencies"] = []any{object{"software": "runtime", "auto_install": true}}
	req.Peers = map[string]json.RawMessage{"runtime": raw(object{"type": "win32", "app_id": "runtime-app"})}
	fake.mu.Lock()
	fake.relatedApps["runtime-app"] = object{"id": "runtime-app", "@odata.type": win32Type, "publishingState": "published"}
	fake.failRelationships = true
	fake.mu.Unlock()
	if _, err := c.handle(t.Context(), req, desired); err == nil {
		t.Fatal("expected relationship failure")
	}
	fake.mu.Lock()
	if len(fake.deletedVersions) != 0 || fake.versions != 2 {
		t.Fatal("failed reference update pruned content")
	}
	fake.failRelationships = false
	fake.mu.Unlock()
	// The tenant records the activated content, so the retry only finishes the rest.
	response, err := c.handle(t.Context(), req, desired)
	if err != nil || slices.ContainsFunc(response.Changes, func(change plugin.Change) bool { return change.Kind == "content" }) {
		t.Fatalf("reference retry: %+v, %v", response.Changes, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.versions != 2 || !slices.Equal(fake.deletedVersions, []string{"1"}) || fake.relationshipWrites != 2 {
		t.Fatal("retry repeated content upload or failed to finish retention")
	}
}

func TestRelationshipsPreserveOmittedCategoriesAndInboundReferences(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	desired["dependencies"] = []any{object{"software": "runtime", "auto_install": false}}
	fake.mu.Lock()
	fake.relatedApps["runtime-app"] = object{"id": "runtime-app", "@odata.type": win32Type, "publishingState": "published", "notes": peerNotes(req, "runtime")}
	fake.relations["app-1"] = []object{
		{"@odata.type": supersedenceType, "targetType": "child", "targetId": "older", "supersedenceType": "replace"},
		{"@odata.type": dependencyType, "targetType": "parent", "targetId": "consumer", "dependencyType": "autoInstall"},
	}
	lists := fake.appLists
	fake.mu.Unlock()
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	if fake.appLists != lists+1 {
		t.Fatalf("finding the app and its peer listed the tenant %d times", fake.appLists-lists)
	}
	if !slices.ContainsFunc(fake.relations["app-1"], func(item object) bool {
		return item["@odata.type"] == dependencyType && item["targetId"] == "runtime-app" && item["dependencyType"] == "detect"
	}) {
		t.Fatalf("dependency did not resolve to the peer's app: %+v", fake.relations["app-1"])
	}
	fake.mu.Unlock()
	response, err := c.handle(t.Context(), req, desired)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("unchanged references did not settle: %+v, %v", response.Changes, err)
	}
	desired["dependencies"] = []any{}
	if _, err = c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.relations["app-1"]) != 2 || fake.relationshipWrites != 2 || fake.assigns != 0 || fake.versions != 1 {
		t.Fatal("category clear changed inbound references, supersedence, assignments or content")
	}
}

// peerNotes are the notes of the app another software document publishes to the
// same destination.
func peerNotes(req plugin.ReconcileRequest, software string) string {
	return withMarker("", publication{identity: markerIdentity(plugin.Identity{Project: req.Identity.Project, Software: software, Destination: req.Identity.Destination})})
}

func TestRelationshipPeersResolveByExplicitAppOrMarker(t *testing.T) {
	published := func(id, notes string) object {
		return object{"id": id, "@odata.type": win32Type, "publishingState": "published", "notes": notes}
	}
	for _, test := range []struct {
		name     string
		software string
		peers    map[string]json.RawMessage
		apps     func(plugin.ReconcileRequest) []object
		target   string
		failure  string
	}{
		{name: "declared app_id", software: "runtime", peers: map[string]json.RawMessage{"runtime": raw(object{"type": "win32", "app_id": "pinned"})}, apps: func(req plugin.ReconcileRequest) []object {
			return []object{published("pinned", ""), published("marked", peerNotes(req, "runtime"))}
		}, target: "pinned"},
		{name: "marker of declared peer", software: "runtime", peers: map[string]json.RawMessage{"runtime": raw(object{"type": "win32"})}, apps: func(req plugin.ReconcileRequest) []object {
			return []object{published("other", peerNotes(req, "unrelated")), published("marked", peerNotes(req, "runtime"))}
		}, target: "marked"},
		{name: "marker of undeclared peer", software: "runtime", apps: func(req plugin.ReconcileRequest) []object {
			return []object{published("marked", peerNotes(req, "runtime"))}
		}, target: "marked"},
		{name: "another destination", software: "runtime", apps: func(req plugin.ReconcileRequest) []object {
			req.Identity.Destination = "intune-staging"
			return []object{published("elsewhere", peerNotes(req, "runtime"))}
		}, failure: `"runtime" is not published to this destination yet`},
		{name: "not published", software: "runtime", apps: func(plugin.ReconcileRequest) []object { return nil }, failure: `"runtime" is not published to this destination yet`},
		{name: "ambiguous", software: "runtime", apps: func(req plugin.ReconcileRequest) []object {
			return []object{published("first-copy", peerNotes(req, "runtime")), published("second-copy", peerNotes(req, "runtime"))}
		}, failure: `"runtime": multiple Intune apps carry this Stemma identity: first-copy, second-copy`},
		{name: "itself", software: "test", apps: func(plugin.ReconcileRequest) []object { return nil }, failure: "cannot depend on or supersede itself"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			c.appType = win32Type
			req := fixtureRequest(t)
			req.Peers = test.peers
			for _, app := range test.apps(req) {
				fake.relatedApps[text(app["id"])] = app
			}
			relationships, err := c.desiredRelationships(t.Context(), req, &tenantApps{client: c}, lifecycle{Supersedes: []relationshipReference{{Software: test.software}}}, "")
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("resolution error: %v", err)
				}
				return
			}
			if err != nil || len(relationships) != 1 || relationships[0]["targetId"] != test.target {
				t.Fatalf("resolved %+v, %v", relationships, err)
			}
		})
	}
}

func TestRelationshipGraphRejectsCyclesAndCountsInboundApps(t *testing.T) {
	if err := validateRelationshipGraph("a", []relationshipEdge{{"a", "b", "dependencies"}, {"b", "a", "supersedes"}}); err == nil {
		t.Fatal("mixed relationship cycle was accepted")
	}
	var edges []relationshipEdge
	for _, parent := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"} {
		edges = append(edges, relationshipEdge{parent, "root", "supersedes"})
	}
	if err := validateRelationshipGraph("root", edges); err == nil || !strings.Contains(err.Error(), "10 apps") {
		t.Fatalf("inbound supersedence did not consume the graph limit: %v", err)
	}
}
