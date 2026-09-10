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

func readBinding(t *testing.T, data json.RawMessage) binding {
	t.Helper()
	var b binding
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRetentionUsesPublishedPayloadOrderAndProtectsUnknownContent(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	desired["retention"] = object{"keep": float64(2)}
	delete(desired, "assignments")
	for _, payload := range []string{"a", "b", "c"} {
		changePayload(t, &req, payload)
		response, err := c.handle(t.Context(), req, configuration{}, desired)
		if err != nil {
			t.Fatal(err)
		}
		req.Binding = response.Binding
	}
	b := readBinding(t, req.Binding)
	if b.Publications.Sequence != 3 || b.ContentVersion != "3" || len(b.Versions) != 2 {
		t.Fatalf("unexpected publication history: %+v", b)
	}
	fake.mu.Lock()
	if !slices.Equal(fake.deletedVersions, []string{"1"}) || fake.creates != 1 || fake.assigns != 0 {
		t.Fatal("retention must delete only inactive content inside the same unassigned app")
	}
	fake.contentVersions["unknown-history"] = true
	fake.mu.Unlock()
	desired["displayName"] = "New display label"
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	b = readBinding(t, response.Binding)
	if b.Publications.Sequence != 3 {
		t.Fatal("metadata-only change advanced publication order")
	}
	req.Binding = response.Binding
	changePayload(t, &req, "d")
	req.Method = "plan"
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(response.Changes, func(change plugin.Change) bool { return change.Kind == "retention" && string(change.Before) == `"2"` }) {
		t.Fatalf("plan omitted post-publication cleanup: %+v", response.Changes)
	}
	if readBinding(t, response.Binding).Publications.Sequence != 3 {
		t.Fatal("planning changed durable publication history")
	}
	fake.mu.Lock()
	if fake.versions != 3 || !fake.contentVersions["2"] || !fake.contentVersions["unknown-history"] {
		t.Fatal("planning mutated content")
	}
	fake.mu.Unlock()
	// Marker recovery can identify active content, but grants no cleanup ownership.
	req.Method, req.Binding = "apply", nil
	changePayload(t, &req, "c")
	desired["retention"] = object{"keep": float64(1)}
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	b = readBinding(t, response.Binding)
	if b.AppID != "app-1" || b.Publications.Sequence != 0 || len(b.Versions) != 0 {
		t.Fatalf("marker recovery inferred ownership or publication order: %+v", b)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deletedVersions) != 1 || fake.versions != 3 {
		t.Fatal("binding loss pruned unknown history or repeated upload")
	}
}

func TestRetentionWaitsForReferencesAndRetryDoesNotUploadAgain(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	desired["retention"] = object{"keep": float64(1)}
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	req.Binding = response.Binding
	changePayload(t, &req, "second")
	desired["dependencies"] = []any{object{"software": "runtime", "auto_install": true}}
	req.Bindings = map[string]json.RawMessage{"runtime": raw(binding{AppID: "runtime-app"})}
	fake.mu.Lock()
	fake.relatedApps["runtime-app"] = object{"id": "runtime-app", "@odata.type": win32Type, "publishingState": "published"}
	fake.failRelationships = true
	fake.mu.Unlock()
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err == nil {
		t.Fatal("expected relationship failure")
	}
	b := readBinding(t, response.Binding)
	if b.Publications.Sequence != 1 || b.Pending == nil || b.Pending.Stage != "published" {
		t.Fatalf("failed references lost resumable upload or advanced history: %+v", b)
	}
	fake.mu.Lock()
	if len(fake.deletedVersions) != 0 || fake.versions != 2 {
		t.Fatal("failed reference update pruned content")
	}
	fake.failRelationships = false
	fake.mu.Unlock()
	req.Binding = response.Binding
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	b = readBinding(t, response.Binding)
	if b.Publications.Sequence != 2 || b.Pending != nil {
		t.Fatalf("reference retry did not finish publication: %+v", b)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.versions != 2 || !slices.Equal(fake.deletedVersions, []string{"1"}) || fake.relationshipWrites != 2 {
		t.Fatal("retry repeated content upload or failed to finish retention")
	}
}

func TestRepublishingPayloadKeepsItsLatestNativeContent(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	desired["retention"] = object{"keep": float64(2)}
	for _, payload := range []string{"a", "b", "a"} {
		changePayload(t, &req, payload)
		response, err := c.handle(t.Context(), req, configuration{}, desired)
		if err != nil {
			t.Fatal(err)
		}
		req.Binding = response.Binding
	}
	b := readBinding(t, req.Binding)
	if b.Publications.Sequence != 3 || len(b.Publications.Order) != 2 {
		t.Fatalf("payload revisit did not preserve distinct publication history: %+v", b.Publications)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !slices.Equal(fake.deletedVersions, []string{"1"}) || !fake.contentVersions["2"] || !fake.contentVersions["3"] {
		t.Fatal("retention preserved an older duplicate or removed a retained payload")
	}
}

func TestRelationshipsPreserveOmittedCategoriesAndInboundReferences(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	req.Binding = response.Binding
	req.Bindings = map[string]json.RawMessage{"runtime": raw(binding{AppID: "runtime-app"})}
	desired["dependencies"] = []any{object{"software": "runtime", "auto_install": false}}
	fake.mu.Lock()
	fake.relatedApps["runtime-app"] = object{"id": "runtime-app", "@odata.type": win32Type, "publishingState": "published"}
	fake.relations["app-1"] = []object{
		{"@odata.type": supersedenceType, "targetType": "child", "targetId": "older", "supersedenceType": "replace"},
		{"@odata.type": dependencyType, "targetType": "parent", "targetId": "consumer", "dependencyType": "autoInstall"},
	}
	fake.mu.Unlock()
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	req.Binding = response.Binding
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("unchanged references did not settle: %+v, %v", response.Changes, err)
	}
	desired["dependencies"] = []any{}
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.relations["app-1"]) != 2 || fake.relationshipWrites != 2 || fake.assigns != 0 || fake.versions != 1 {
		t.Fatal("category clear changed inbound references, supersedence, assignments or content")
	}
	if readBinding(t, response.Binding).Publications.Sequence != 1 {
		t.Fatal("relationship-only edits advanced payload history")
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
