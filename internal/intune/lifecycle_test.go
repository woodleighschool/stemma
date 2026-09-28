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

func changePayload(t *testing.T, req *plugin.ReconcileRequest[Config], content string) {
	t.Helper()
	data := []byte("@echo off\r\necho " + content + "\r\n")
	if err := os.WriteFile(req.Artifact.Path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	req.Artifact.SHA256 = hex.EncodeToString(digest[:])
	req.Artifact.Size = int64(len(data))
}

func committedFile() object { return object{"id": "file-1", "isCommitted": true} }

func TestInterruptedUpdateIsRepublishedInAFreshVersion(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := compile(req)
	delete(desired, "assignments")
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
	if fake.app["committedContentVersion"] != "1" || fake.files["2"]["isCommitted"] == true {
		t.Fatalf("interrupted upload changed active content: %+v", fake.app)
	}
	fake.mu.Unlock()
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	if published := publishedMarker(t, fake); published.content != "3" || published.payload == first.payload {
		t.Fatalf("retry did not publish fresh content: %+v", published)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.versions != 3 || fake.creates != 1 {
		t.Fatalf("retry did not publish in the same app: %d versions, %d apps", fake.versions, fake.creates)
	}
}

func TestReferenceRetryDoesNotUploadAgain(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := compile(req)
	delete(desired, "assignments")
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	changePayload(t, &req, "second")
	desired["dependencies"] = []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}, "auto_install": true}}
	req.Peers = map[string]json.RawMessage{"stemma/v1alpha1/WindowsSoftware/runtime": raw(object{"type": "win32", "app_id": "runtime-app"})}
	fake.mu.Lock()
	fake.relatedApps["runtime-app"] = object{"id": "runtime-app", "@odata.type": win32Type, "publishingState": "published"}
	fake.failRelationships = true
	fake.mu.Unlock()
	if _, err := c.handle(t.Context(), req, desired); err == nil {
		t.Fatal("expected relationship failure")
	}
	fake.mu.Lock()
	if fake.versions != 2 {
		t.Fatal("failed reference update did not publish content first")
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
	if fake.versions != 2 || fake.relationshipWrites != 2 {
		t.Fatal("retry repeated content upload or skipped the references")
	}
}

func TestRelationshipsPreserveOmittedCategoriesAndInboundReferences(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := compile(req)
	delete(desired, "assignments")
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	desired["dependencies"] = []any{object{"resource": object{"kind": "WindowsSoftware", "name": "runtime"}, "auto_install": false}}
	req.Peers = map[string]json.RawMessage{"stemma/v1alpha1/WindowsSoftware/runtime": raw(object{"type": "win32"})}
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
func peerNotes(req plugin.ReconcileRequest[Config], software string) string {
	return withMarker("", publication{identity: markerIdentity(plugin.Identity{Project: req.Identity.Project, Resource: plugin.ResourceReference{Kind: "WindowsSoftware", Name: software}, Destination: req.Identity.Destination})})
}

func TestRelationshipPeersResolveByExplicitAppOrMarker(t *testing.T) {
	published := func(id, notes string) object {
		return object{"id": id, "@odata.type": win32Type, "publishingState": "published", "notes": notes}
	}
	for _, test := range []struct {
		name, peer, external, target, failure string
		declared, duplicate                   bool
	}{
		{name: "declared app_id", peer: `{"app_id":"pinned"}`, declared: true, target: "pinned"},
		{name: "canonical marker", peer: `{"type":"win32"}`, declared: true, target: "marked"},
		{name: "unmanaged app", external: "pinned", target: "pinned"},
		{name: "missing resource metadata", failure: "does not publish to this destination"},
		{name: "unpublished peer", peer: `{}`, declared: true, failure: "not published to this destination yet"},
		{name: "ambiguous marker", peer: `{}`, declared: true, duplicate: true, failure: "multiple Intune apps"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			c.appType = win32Type
			req := fixtureRequest(t)
			ref := relationshipReference{Resource: &plugin.ResourceReference{Kind: "WindowsSoftware", Name: "runtime"}, Install: true}
			if test.declared {
				req.Peers = map[string]json.RawMessage{ref.Resource.Key(): json.RawMessage(test.peer)}
			}
			if test.external != "" {
				ref.Resource = nil
				ref.AppID = test.external
			}
			fake.relatedApps["pinned"] = published("pinned", "")
			if test.name != "unpublished peer" {
				// Publication uses an explicit API version; the reference defaults it.
				identity := req.Identity
				identity.Resource = plugin.ResourceReference{APIVersion: "stemma/v1alpha1", Kind: "WindowsSoftware", Name: "runtime"}
				fake.relatedApps["marked"] = published("marked", withMarker("", publication{identity: markerIdentity(identity)}))
			}
			// Same name, other kind is a different publication.
			identity := req.Identity
			identity.Resource = plugin.ResourceReference{Kind: "MacSoftware", Name: "runtime"}
			fake.relatedApps["other-kind"] = published("other-kind", withMarker("", publication{identity: markerIdentity(identity)}))
			if test.duplicate {
				fake.relatedApps["copy"] = published("copy", peerNotes(req, "runtime"))
			}
			for _, category := range []string{"dependencies", "supersedes"} {
				lifecycle := lifecycle{}
				if category == "dependencies" {
					lifecycle.Dependencies = []relationshipReference{ref}
				} else {
					lifecycle.Supersedes = []relationshipReference{ref}
				}
				relationships, err := c.desiredRelationships(t.Context(), req, &tenantApps{client: c}, lifecycle, "")
				if test.failure != "" {
					if err == nil || !strings.Contains(err.Error(), test.failure) {
						t.Fatalf("%s: %v", category, err)
					}
					continue
				}
				if err != nil || len(relationships) != 1 || relationships[0]["targetId"] != test.target {
					t.Fatalf("%s: %+v, %v", category, relationships, err)
				}
				if category == "dependencies" && relationships[0]["dependencyType"] != "autoInstall" || category == "supersedes" && relationships[0]["supersedenceType"] != "replace" {
					t.Fatalf("policy lost: %+v", relationships)
				}
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
