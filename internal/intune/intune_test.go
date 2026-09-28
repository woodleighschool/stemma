package intune

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/microsoft/kiota-abstractions-go/authentication"
	"github.com/woodleighschool/stemma/plugin"
)

func TestUploadThenMetadataAndAssignmentOwnership(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	published := publishedMarker(t, fake)
	if published.identity != markerIdentity(req.Identity) || published.content != "1" || published.payload == "" {
		t.Fatalf("incomplete marker: %+v", published)
	}
	fake.mu.Lock()
	if fake.creates != 1 || fake.versions != 1 || fake.blobLists != 1 || fake.commits != 1 || fake.assigns != 1 {
		t.Fatal("upload did not complete all required stages")
	}
	if fake.app["setupFilePath"] != "setup.cmd" || fake.app["fileName"] != "test-"+req.Artifact.SHA256[:12]+".intunewin" || fake.app["committedContentVersion"] != "1" {
		t.Fatalf("native content metadata: %+v", fake.app)
	}
	fake.app["owner"] = "Remote owner"
	fake.app["isFeatured"] = true
	fake.app["installExperience"].(object)["deviceRestartBehavior"] = "suppress"
	fake.mu.Unlock()
	req.Metadata = raw(object{"display_name": "Renamed", "featured": false, "install_experience": object{"run_as": "user"}})
	desired, err = compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	if fake.versions != 1 || fake.blobLists != 1 || fake.assigns != 1 {
		t.Fatal("metadata-only change uploaded or reassigned")
	}
	if fake.app["owner"] != "Remote owner" || fake.app["isFeatured"] != false || fake.app["installExperience"].(object)["deviceRestartBehavior"] != "suppress" {
		t.Fatalf("lost omitted fields or false: %+v", fake.app)
	}
	writes := fake.writes
	fake.mu.Unlock()
	response, err := c.handle(t.Context(), req, desired)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("unchanged reconciliation: %+v, %v", response.Changes, err)
	}
	fake.mu.Lock()
	if fake.writes != writes {
		t.Fatal("unchanged reconciliation wrote to the tenant")
	}
	fake.mu.Unlock()
	req.Metadata = raw(object{"architectures": nil, "assignments": []any{}})
	desired, err = compile(req)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.handle(t.Context(), req, desired)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.app["allowedArchitectures"] != nil || len(fake.assignments) != 0 || fake.assigns != 2 {
		t.Fatal("explicit null or assignment clear was lost")
	}
}

func TestAssignmentFilterAndNotificationsBelongToTheAssignment(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	declare := func(assignment object) object {
		t.Helper()
		req.Metadata = raw(object{
			"display_name": "Fixture", "description": "Test app", "publisher": "Fixture Publisher",
			"install_command": "setup.cmd", "uninstall_command": "setup.cmd /remove", "minimum_windows_release": "Windows11_23H2", "architectures": []any{"x64"},
			"install_experience": object{"run_as": "system"},
			"detection":          []any{object{"type": "msi", "product_code": "{AC01F3D3-C5D5-40DB-9E8C-ED53982E17ED}"}},
			"assignments":        []any{assignment},
		})
		desired, err := compile(req)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.handle(t.Context(), req, desired); err != nil {
			t.Fatal(err)
		}
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.assignments[0].(object)
	}
	assigned := declare(object{"intent": "available", "all_devices": true, "filter": object{"id": "filter-1", "mode": "include"}, "notifications": "hide_all"})
	target, settings := assigned["target"].(object), assigned["settings"].(object)
	if target["deviceAndAppManagementAssignmentFilterId"] != "filter-1" || target["deviceAndAppManagementAssignmentFilterType"] != "include" || settings["notifications"] != "hideAll" {
		t.Fatalf("assignment: %+v", assigned)
	}
	fake.mu.Lock()
	settings["deliveryOptimizationPriority"] = "foreground"
	assigns := fake.assigns
	fake.mu.Unlock()
	kept := declare(object{"intent": "available", "all_devices": true})
	fake.mu.Lock()
	reassigned := fake.assigns != assigns
	fake.mu.Unlock()
	if reassigned || kept["target"].(object)["deviceAndAppManagementAssignmentFilterId"] != "filter-1" || kept["settings"].(object)["notifications"] != "hideAll" {
		t.Fatalf("omitted filter and notifications were not kept: %+v", kept)
	}
	cleared := declare(object{"intent": "available", "all_devices": true, "filter": nil})
	target, settings = cleared["target"].(object), cleared["settings"].(object)
	if target["deviceAndAppManagementAssignmentFilterId"] != nil || target["deviceAndAppManagementAssignmentFilterType"] != "none" || settings["deliveryOptimizationPriority"] != "foreground" {
		t.Fatalf("filter removal: %+v", cleared)
	}
}

func TestAssignmentSettingsNeedAnIncludedWin32Target(t *testing.T) {
	for name, metadata := range map[string]object{
		"excluded filter":       {"assignments": []any{object{"intent": "required", "exclude_group": "group-1", "filter": object{"id": "filter-1", "mode": "include"}}}},
		"excluded notification": {"assignments": []any{object{"intent": "required", "exclude_group": "group-1", "notifications": "hide_all"}}},
		"filter mode":           {"assignments": []any{object{"intent": "required", "all_devices": true, "filter": object{"id": "filter-1", "mode": "only"}}}},
		"Mac notification":      {"type": "pkg", "assignments": []any{object{"intent": "required", "all_devices": true, "notifications": "hide_all"}}},
	} {
		t.Run(name, func(t *testing.T) {
			kind := "WindowsSoftware"
			if metadata["type"] == "pkg" {
				kind = "MacSoftware"
			}
			req := plugin.ReconcileRequest[Config]{Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: kind, Name: "example"}}, Metadata: raw(metadata)}
			if _, err := compile(req); err == nil {
				t.Fatal("accepted assignment settings")
			}
		})
	}
}

func TestPublicationSurvivesLostReply(t *testing.T) {
	for _, test := range []struct {
		name             string
		update, activate bool
	}{
		{"first commit", false, false},
		{"updated commit", true, false},
		{"first activation", false, true},
		{"updated activation", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			req := fixtureRequest(t)
			desired, err := compile(req)
			if err != nil {
				t.Fatal(err)
			}
			want := 1
			if test.update {
				if _, err := c.handle(t.Context(), req, desired); err != nil {
					t.Fatal(err)
				}
				changePayload(t, &req, "second release")
				want++
			}
			fake.failCommit, fake.failActivation = !test.activate, test.activate
			if _, err := c.handle(t.Context(), req, desired); err == nil {
				t.Fatal("expected interrupted commit")
			}
			if _, err := c.handle(t.Context(), req, desired); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.creates != 1 || fake.versions != want || fake.fileCreates != want || fake.blobLists != want || fake.commits != want || fake.app["committedContentVersion"] != strconv.Itoa(want) {
				t.Fatalf("retry duplicated committed content: apps=%d versions=%d files=%d uploads=%d commits=%d active=%v", fake.creates, fake.versions, fake.fileCreates, fake.blobLists, fake.commits, fake.app["committedContentVersion"])
			}
		})
	}
}

func TestFirstUploadRejectsDifferentPayloadOfSameSize(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	req.Artifact.Filename = "test.pkg"
	desired := object{"@odata.type": pkgType, "displayName": "Test", "description": "Test package", "publisher": "Test", "primaryBundleId": "org.example.test", "primaryBundleVersion": "1.0", "includedApps": []any{object{"bundleId": "org.example.test", "bundleVersion": "1.0"}}, "minimumSupportedOperatingSystem": object{"v12_0": true}}
	fake.failCommit = true
	if _, err := c.handle(t.Context(), req, desired); err == nil {
		t.Fatal("expected interrupted commit")
	}
	if fake.commits != 1 {
		t.Fatal("fixture did not commit its first payload")
	}
	size := req.Artifact.Size
	changePayload(t, &req, "changed")
	if req.Artifact.Size != size {
		t.Fatal("fixture must keep the same size")
	}
	if _, err := c.handle(t.Context(), req, desired); err == nil || !strings.Contains(err.Error(), "recorded payload") {
		t.Fatalf("different first payload: %v", err)
	}
	if fake.commits != 1 || text(fake.app["committedContentVersion"]) != "" {
		t.Fatal("activated content from the previous request")
	}
}

func TestInterruptedFirstUploadIsRetriedIntoItsFile(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.failBlob = true
	req := fixtureRequest(t)
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err == nil {
		t.Fatal("expected interrupted upload")
	}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	if published := publishedMarker(t, fake); published.content != "1" {
		t.Fatalf("retry did not publish the first content version: %+v", published)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	// Graph can neither delete the file an interrupted upload leaves nor
	// activate its version while that file is uncommitted.
	if fake.creates != 1 || fake.versions != 1 || fake.fileCreates != 1 || fake.app["committedContentVersion"] != "1" {
		t.Fatalf("retry did not reuse the unfinished upload: %d versions, %d files", fake.versions, fake.fileCreates)
	}
}

func TestExpiredFirstUploadNamesTheAppToDelete(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.failBlob = true
	req := fixtureRequest(t)
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err == nil {
		t.Fatal("expected interrupted upload")
	}
	fake.mu.Lock()
	fake.files["1"]["azureStorageUriExpirationDateTime"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	fake.mu.Unlock()
	_, err = c.handle(t.Context(), req, desired)
	if err == nil || !strings.Contains(err.Error(), "intune app app-1 cannot finish its first upload; delete the app") {
		t.Fatalf("stuck first upload: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.versions != 1 || fake.fileCreates != 1 {
		t.Fatalf("stuck first upload started another upload: %d versions, %d files", fake.versions, fake.fileCreates)
	}
}

func TestFirstUploadReadFailureDoesNotRequireDeletion(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.failBlob = true
	req := fixtureRequest(t)
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err == nil {
		t.Fatal("expected interrupted upload")
	}
	fake.failFileRead = true
	if _, err := c.handle(t.Context(), req, desired); err == nil || strings.Contains(err.Error(), "delete the app") {
		t.Fatalf("read failure: %v", err)
	}
	fake.failFileRead = false
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	if fake.versions != 1 || fake.fileCreates != 1 {
		t.Fatal("retry replaced the upload after a read failure")
	}
}

func TestUnchangedApplyWaitsForPendingPublication(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.app["publishingState"] = "processing"
	fake.pendingAppReads = 5
	writes := fake.writes
	fake.mu.Unlock()
	req.Method = "plan"
	if response, err := c.handle(t.Context(), req, desired); err != nil || len(response.Changes) != 0 {
		t.Fatalf("pending plan: %+v, %v", response, err)
	}
	fake.mu.Lock()
	if fake.writes != writes || fake.app["publishingState"] != "processing" {
		t.Fatal("plan changed or waited for publication")
	}
	fake.mu.Unlock()
	req.Method = "apply"
	if response, err := c.handle(t.Context(), req, desired); err != nil || len(response.Changes) != 0 {
		t.Fatalf("pending apply: %+v, %v", response, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.app["publishingState"] != "published" {
		t.Fatal("apply succeeded while publication was pending")
	}
	if fake.writes != writes {
		t.Fatal("waiting replayed content or metadata writes")
	}
}

func TestFreshRequestRediscoversPublishedApp(t *testing.T) {
	for _, discovery := range []string{"marker", "app_id"} {
		t.Run(discovery, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			req := fixtureRequest(t)
			desired, err := compile(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.handle(t.Context(), req, desired); err != nil {
				t.Fatal(err)
			}
			fake.mu.Lock()
			lists, writes := fake.appLists, fake.writes
			fake.mu.Unlock()
			// A later invocation starts from the catalog and the tenant alone.
			req = fixtureRequest(t)
			desired, err = compile(req)
			if err != nil {
				t.Fatal(err)
			}
			if discovery == "app_id" {
				desired["app_id"] = "app-1"
			}
			for _, method := range []string{"plan", "apply"} {
				req.Method = method
				response, err := c.handle(t.Context(), req, desired)
				if err != nil || len(response.Changes) != 0 {
					t.Fatalf("%s after rediscovery: %+v, %v", method, response.Changes, err)
				}
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.creates != 1 || fake.versions != 1 || fake.writes != writes {
				t.Fatal("rediscovery duplicated the app or its content")
			}
			// The marker is found in one listing per invocation; app_id needs none.
			if discovery == "marker" {
				lists += 2
			}
			if fake.appLists != lists {
				t.Fatalf("tenant listings = %d, want %d", fake.appLists, lists)
			}
		})
	}
}

func TestPlanAndValidationDoNotWrite(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	req.Method = "plan"
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.handle(t.Context(), req, desired)
	if err != nil || len(response.Changes) == 0 {
		t.Fatalf("plan: %+v, %v", response, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.writes != 0 {
		t.Fatal("plan wrote remote state")
	}
	for _, data := range []string{
		`{"type":"pkg"}`,
		`{"@odata.type":"#microsoft.graph.win32LobApp"}`,
		`{"id":"read-only"}`,
		`{"featured":null}`,
		`{"install_experience":{"run_as":null}}`,
		`{"assignments":null}`,
		`{"assignments":[{"intent":"required","target":{"@odata.type":"#microsoft.graph.groupAssignmentTarget","groupId":"group-1"}}]}`,
	} {
		req.Metadata = json.RawMessage(data)
		if _, err := compile(req); err == nil {
			t.Fatalf("accepted unsupported metadata %s", data)
		}
	}
}

func TestPayloadChangeActivatesFreshVersionWithItsMarker(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	first := publishedMarker(t, fake)
	fake.mu.Lock()
	fake.patches = nil
	fake.mu.Unlock()
	changePayload(t, &req, "second release")
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	second := publishedMarker(t, fake)
	if second.content != "2" || second.payload == first.payload || second.identity != first.identity {
		t.Fatalf("marker does not describe the new content: %+v after %+v", second, first)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 1 || fake.versions != 2 || fake.app["committedContentVersion"] != "2" {
		t.Fatal("payload change did not publish a fresh version in the same app")
	}
	for _, patch := range fake.patches {
		if patch["committedContentVersion"] != nil && patch["notes"] != nil {
			t.Fatal("activation and metadata share a PATCH")
		}
	}
}

func TestMarkerDriftRestoresContent(t *testing.T) {
	for _, drift := range []string{"edited", "removed", "other version activated"} {
		t.Run(drift, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			req := fixtureRequest(t)
			desired, err := compile(req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.handle(t.Context(), req, desired); err != nil {
				t.Fatal(err)
			}
			published := publishedMarker(t, fake)
			fake.mu.Lock()
			fake.app["notes"] = withMarker("Edited by an administrator", published)
			switch drift {
			case "edited":
				edited := published
				edited.payload = strings.Repeat("0", 64)
				fake.app["notes"] = withMarker("Edited by an administrator", edited)
			case "removed":
				// Without its marker only a declared app_id still identifies the app.
				fake.app["notes"] = "Edited by an administrator"
				desired["app_id"] = "app-1"
			default:
				// The marker still describes version 1, which is no longer the active one.
				fake.files["7"] = committedFile()
				fake.app["committedContentVersion"] = "7"
			}
			fake.mu.Unlock()
			if _, err := c.handle(t.Context(), req, desired); err != nil {
				t.Fatal(err)
			}
			want := "2"
			if drift == "other version activated" {
				want = "1"
			}
			if restored := publishedMarker(t, fake); restored.content != want || restored.payload != published.payload {
				t.Fatalf("marker was not rewritten for fresh content: %+v", restored)
			}
			fake.mu.Lock()
			writes := fake.writes
			if fake.creates != 1 || fake.app["committedContentVersion"] != want || !strings.HasPrefix(text(fake.app["notes"]), "Edited by an administrator\n") {
				t.Fatalf("drift was not repaired in place: %+v", fake.app["notes"])
			}
			fake.mu.Unlock()
			response, err := c.handle(t.Context(), req, desired)
			if err != nil || len(response.Changes) != 0 {
				t.Fatalf("repaired drift did not settle: %+v, %v", response.Changes, err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.writes != writes {
				t.Fatal("settled reconciliation wrote to the tenant")
			}
		})
	}
}

func TestDuplicateMarkerAppsAreAmbiguous(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	notes := withMarker("", publication{identity: markerIdentity(req.Identity)})
	for _, id := range []string{"first-copy", "second-copy"} {
		fake.relatedApps[id] = object{"id": id, "@odata.type": win32Type, "notes": notes}
	}
	for _, method := range []string{"plan", "apply"} {
		req.Method = method
		_, err := c.handle(t.Context(), req, desired)
		if err == nil || !strings.Contains(err.Error(), "multiple Intune apps carry this Stemma identity") || !strings.Contains(err.Error(), "first-copy, second-copy") {
			t.Fatalf("%s: %v", method, err)
		}
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.writes != 0 {
		t.Fatal("ambiguous identity wrote to the tenant")
	}
}

// publishedMarker reads the publication recorded in the fixture app's notes.
func publishedMarker(t *testing.T, fake *graphFixture) publication {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	match := markerPattern.FindStringSubmatch(text(fake.app["notes"]))
	if match == nil {
		t.Fatalf("app notes carry no marker: %q", fake.app["notes"])
	}
	return publication{identity: match[1], payload: match[2], content: match[3]}
}

func fixtureRequest(t *testing.T) plugin.ReconcileRequest[Config] {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup.cmd")
	data := []byte("@echo off\r\necho fixture\r\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return plugin.ReconcileRequest[Config]{Method: "apply", Identity: plugin.Identity{Project: "example", Resource: plugin.ResourceReference{Kind: "WindowsSoftware", Name: "test"}, Destination: "intune"}, Artifact: plugin.Artifact{Path: path, Filename: "setup.cmd", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}, Metadata: raw(object{
		"display_name": "Fixture", "description": "Test app", "publisher": "Fixture Publisher",
		"install_command": "setup.cmd", "uninstall_command": "setup.cmd /remove", "minimum_windows_release": "Windows11_23H2", "architectures": []any{"x64"},
		"install_experience": object{"run_as": "system"},
		"detection":          []any{object{"type": "msi", "product_code": "{AC01F3D3-C5D5-40DB-9E8C-ED53982E17ED}"}},
		"assignments":        []any{object{"intent": "required", "group": "group-1"}},
	})}
}

type graphFixture struct {
	mu          sync.Mutex
	url         string
	app         object
	assignments []any
	files       map[string]object // content version ID to its file, nil until an upload creates one
	blocks      map[string][]byte
	uploaded    []byte
	plaintext   []byte

	creates, versions, blobLists, commits, assigns, appLists, writes      int
	failBlob, failCommit, failActivation, failFileRead, failRelationships bool
	blockList                                                             bool // the upload was committed as a block list

	pendingAppReads    int
	contentTypes       []string
	paths              []string
	patches            []object
	relations          map[string][]object
	relatedApps        map[string]object
	relationshipWrites int
	fileCreates        int
	abandonedFiles     map[string]int // uncommitted files a newer file left behind, by content version
	categories         []object       // the tenant's app categories
	appCategories      []string       // category IDs the app references
}

func newGraphFixture(t *testing.T) (*graphFixture, *client) {
	t.Helper()
	fake := &graphFixture{blocks: map[string][]byte{}, files: map[string]object{}, relations: map[string][]object{}, relatedApps: map[string]object{}, abandonedFiles: map[string]int{}}
	server := httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	fake.url = server.URL
	auth, err := authentication.NewApiKeyAuthenticationProvider("Bearer test-token", "Authorization", authentication.HEADER_KEYLOCATION)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newSDKClient(server.URL, auth, server.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	c.pollInterval = time.Millisecond
	return fake, c
}

func (f *graphFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, "invalid gzip", http.StatusBadRequest)
			return
		}
		defer func() { _ = reader.Close() }()
		r.Body = reader
	}
	var body object
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
	}
	if r.URL.Path == "/blob" {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "leaked Graph token", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}
		f.writes++
		if f.failBlob {
			f.failBlob = false
			http.Error(w, "interrupted upload", http.StatusForbidden)
			return
		}
		switch r.URL.Query().Get("comp") {
		case "block":
			f.blocks[r.URL.Query().Get("blockid")] = data
		case "":
			f.uploaded, f.blockList = data, false
			f.blobLists++
		default:
			var list struct {
				Latest []string `xml:"Latest"`
			}
			if err := xml.Unmarshal(data, &list); err != nil {
				http.Error(w, "bad block list", http.StatusBadRequest)
				return
			}
			f.uploaded, f.blockList = nil, true
			for _, id := range list.Latest {
				f.uploaded = append(f.uploaded, f.blocks[id]...)
			}
			f.blobLists++
		}
		w.WriteHeader(http.StatusCreated)
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token" {
		http.Error(w, "missing auth", http.StatusUnauthorized)
		return
	}
	const api = "beta"
	if !strings.HasPrefix(r.URL.Path, "/"+api+"/") {
		http.Error(w, "wrong API version", http.StatusBadRequest)
		return
	}
	f.paths = append(f.paths, r.URL.Path)
	if r.Method != http.MethodGet {
		f.writes++
	}
	path := strings.TrimPrefix(r.URL.Path, "/"+api)
	_, version, _ := strings.Cut(path, "/contentVersions/")
	version, _, _ = strings.Cut(version, "/")
	switch {
	case path == "/deviceAppManagement/mobileAppCategories" && r.Method == http.MethodGet:
		write(object{"value": f.categories})
	case path == "/deviceAppManagement/mobileAppCategories" && r.Method == http.MethodPost:
		category := object{"id": "cat-" + strconv.Itoa(len(f.categories)+1), "displayName": body["displayName"]}
		f.categories = append(f.categories, category)
		write(category)
	case path == appsPath+"/app-1/categories" && r.Method == http.MethodGet:
		items := []object{}
		for _, category := range f.categories {
			if slices.Contains(f.appCategories, text(category["id"])) {
				items = append(items, category)
			}
		}
		write(object{"value": items})
	case path == appsPath+"/app-1/categories/$ref" && r.Method == http.MethodPost:
		id, found := strings.CutPrefix(text(body["@odata.id"]), f.url+"/beta/deviceAppManagement/mobileAppCategories/")
		if !found || slices.Contains(f.appCategories, id) {
			http.Error(w, "bad category reference", http.StatusBadRequest)
			return
		}
		f.appCategories = append(f.appCategories, id)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, appsPath+"/app-1/categories/") && strings.HasSuffix(path, "/$ref") && r.Method == http.MethodDelete:
		id := strings.TrimSuffix(strings.TrimPrefix(path, appsPath+"/app-1/categories/"), "/$ref")
		index := slices.Index(f.appCategories, id)
		if index < 0 {
			http.Error(w, "no such category reference", http.StatusNotFound)
			return
		}
		f.appCategories = slices.Delete(f.appCategories, index, index+1)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/relationships") && r.Method == http.MethodGet:
		id := strings.TrimSuffix(strings.TrimPrefix(path, appsPath+"/"), "/relationships")
		items := f.relations[id]
		if items == nil {
			items = []object{}
		}
		write(object{"value": items})
	case strings.HasSuffix(path, "/updateRelationships") && r.Method == http.MethodPost:
		f.relationshipWrites++
		if f.failRelationships {
			http.Error(w, "relationship failure", http.StatusBadRequest)
			return
		}
		if f.app["publishingState"] != "published" {
			http.Error(w, "references precede publication", http.StatusBadRequest)
			return
		}
		id := strings.TrimSuffix(strings.TrimPrefix(path, appsPath+"/"), "/updateRelationships")
		var relationships []object
		for _, item := range f.relations[id] {
			if item["targetType"] == "parent" {
				relationships = append(relationships, item)
			}
		}
		for _, item := range body["relationships"].([]any) {
			relationships = append(relationships, item.(object))
		}
		f.relations[id] = relationships
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(path, appsPath+"/") && f.relatedApps[strings.TrimPrefix(path, appsPath+"/")] != nil && r.Method == http.MethodGet:
		write(f.relatedApps[strings.TrimPrefix(path, appsPath+"/")])
	case path == appsPath && r.Method == http.MethodGet:
		f.appLists++
		apps := []any{}
		listed := func(app object) {
			// Collection responses may omit heavyweight properties such as largeIcon.
			app = maps.Clone(app)
			delete(app, "largeIcon")
			apps = append(apps, app)
		}
		if f.app != nil {
			listed(f.app)
		}
		for _, id := range slices.Sorted(maps.Keys(f.relatedApps)) {
			listed(f.relatedApps[id])
		}
		write(object{"value": apps})
	case path == appsPath && r.Method == http.MethodPost:
		f.creates++
		f.app = body
		f.app["id"] = "app-1"
		f.app["publishingState"] = "notPublished"
		write(f.app)
	case path == appsPath+"/app-1" && r.Method == http.MethodGet:
		if f.pendingAppReads > 0 {
			f.pendingAppReads--
			if f.pendingAppReads == 0 {
				f.app["publishingState"] = "published"
			}
		}
		write(f.app)
	case path == appsPath+"/app-1" && r.Method == http.MethodPatch:
		f.patches = append(f.patches, body)
		if body["committedContentVersion"] == nil && f.app["publishingState"] != "published" {
			http.Error(w, "Invalid operation: app's PublishingState is not 'Published'.", http.StatusBadRequest)
			return
		}
		if version := text(body["committedContentVersion"]); version != "" && f.abandonedFiles[version] > 0 {
			http.Error(w, "All AppFiles must be committed before committing an application.", http.StatusBadRequest)
			return
		}
		if version := body["committedContentVersion"]; version != nil && f.app["publishingState"] != "published" {
			// Graph drops every other property of the PATCH that first publishes an app.
			f.app["committedContentVersion"] = version
		} else {
			maps.Copy(f.app, body)
		}
		if body["committedContentVersion"] != nil {
			f.app["publishingState"] = "published"
			if f.failActivation {
				f.failActivation = false
				http.Error(w, "activation reply failed", http.StatusBadRequest)
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/contentVersions") && r.Method == http.MethodPost:
		if f.app == nil || !strings.Contains(path, "/"+strings.TrimPrefix(text(f.app["@odata.type"]), "#microsoft.")+"/contentVersions") {
			http.Error(w, "wrong subtype content path", http.StatusBadRequest)
			return
		}
		if f.app["committedContentVersion"] == nil && len(f.files) > 0 {
			http.Error(w, "The mobile app content cannot be updated before the first content version is committed.", http.StatusBadRequest)
			return
		}
		f.contentTypes = append(f.contentTypes, text(f.app["@odata.type"]))
		f.versions++
		id := strconv.Itoa(f.versions)
		f.files[id] = nil
		write(object{"id": id})
	case strings.HasSuffix(path, "/contentVersions") && r.Method == http.MethodGet:
		items := []object{}
		for id := range f.files {
			items = append(items, object{"id": id})
		}
		write(object{"value": items})
	case strings.HasSuffix(path, "/files") && r.Method == http.MethodPost:
		body["id"] = "file-1"
		body["uploadState"] = "azureStorageUriRequestSuccess"
		body["azureStorageUri"] = f.url + "/blob?sig=temporary"
		if previous := f.files[version]; previous != nil && previous["isCommitted"] != true {
			f.abandonedFiles[version]++
		}
		f.fileCreates++
		f.files[version] = body
		write(body)
	case strings.HasSuffix(path, "/files") && r.Method == http.MethodGet:
		items := []object{}
		if file := f.files[version]; file != nil {
			items = append(items, file)
		}
		write(object{"value": items})
	case strings.HasSuffix(path, "/renewUpload"):
		// Graph renews no upload URL for a file an interrupted upload left behind.
		f.files[version]["uploadState"] = "azureStorageUriRenewalFailed"
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/files/file-1") && r.Method == http.MethodGet:
		if f.failFileRead {
			http.Error(w, "read denied", http.StatusForbidden)
			return
		}
		write(f.files[version])
	case strings.HasSuffix(path, "/commit"):
		file := f.files[version]
		if !f.blockList {
			// Intune accepts the request but fails to commit content stored by a
			// single Put Blob.
			file["uploadState"] = "commitFileFailed"
			w.WriteHeader(http.StatusNoContent)
			return
		}
		info, ok := body["fileEncryptionInfo"].(object)
		if !ok || len(f.uploaded) < 48 {
			http.Error(w, "bad commit body", http.StatusBadRequest)
			return
		}
		macKey, err := base64.StdEncoding.DecodeString(text(info["macKey"]))
		if err != nil {
			http.Error(w, "bad key", http.StatusBadRequest)
			return
		}
		mac := hmac.New(sha256.New, macKey)
		mac.Write(f.uploaded[32:])
		recorded, err := base64.StdEncoding.DecodeString(text(info["mac"]))
		if err != nil || !hmac.Equal(mac.Sum(nil), recorded) || !hmac.Equal(f.uploaded[:32], recorded) {
			http.Error(w, "commit metadata does not authenticate uploaded bytes", http.StatusBadRequest)
			return
		}
		key, keyErr := base64.StdEncoding.DecodeString(text(info["encryptionKey"]))
		iv, ivErr := base64.StdEncoding.DecodeString(text(info["initializationVector"]))
		digest, digestErr := base64.StdEncoding.DecodeString(text(info["fileDigest"]))
		block, blockErr := aes.NewCipher(key)
		if keyErr != nil || ivErr != nil || digestErr != nil || blockErr != nil || len(iv) != 16 || !bytes.Equal(iv, f.uploaded[32:48]) || (len(f.uploaded)-48)%16 != 0 {
			http.Error(w, "bad encryption contract", http.StatusBadRequest)
			return
		}
		plaintext := append([]byte(nil), f.uploaded[48:]...)
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, plaintext)
		padding := int(plaintext[len(plaintext)-1])
		if padding < 1 || padding > 16 || !bytes.Equal(plaintext[len(plaintext)-padding:], bytes.Repeat([]byte{byte(padding)}, padding)) {
			http.Error(w, "bad padding", http.StatusBadRequest)
			return
		}
		plaintext = plaintext[:len(plaintext)-padding]
		actualDigest := sha256.Sum256(plaintext)
		if !bytes.Equal(actualDigest[:], digest) || file["size"] != float64(len(plaintext)) || file["sizeEncrypted"] != float64(len(f.uploaded)) || info["profileIdentifier"] != "ProfileVersion1" || info["fileDigestAlgorithm"] != "SHA256" {
			http.Error(w, "bad plaintext digest or sizes", http.StatusBadRequest)
			return
		}
		f.plaintext = plaintext
		f.commits++
		file["isCommitted"] = true
		file["uploadState"] = "commitFileSuccess"
		if f.failCommit {
			f.failCommit = false
			http.Error(w, "ambiguous commit", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/assignments"):
		write(object{"value": f.assignments})
	case strings.HasSuffix(path, "/assign"):
		f.assigns++
		f.assignments, _ = body["mobileAppAssignments"].([]any)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unexpected route", http.StatusNotFound)
	}
}

func TestMacRawContentAndMetadata(t *testing.T) {
	for _, appType := range []string{dmgType, pkgType, lobType} {
		t.Run(appType, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			extension := ".dmg"
			if appType != dmgType {
				extension = ".pkg"
			}
			// Transport fixtures deliberately do not claim installer-format validity.
			// Installer inspection/signature policy belongs to the artifact pipeline.
			source := append([]byte("vendor artifact bytes preserved exactly\x00"), bytes.Repeat([]byte{0x91, 0x3, 0x72}, 2000)...)
			req := fixtureRequest(t)
			req.Artifact.Path = filepath.Join(t.TempDir(), "vendor"+extension)
			req.Artifact.Filename = "vendor" + extension
			req.Artifact.Size = int64(len(source))
			digest := sha256.Sum256(source)
			req.Artifact.SHA256 = hex.EncodeToString(digest[:])
			if err := os.WriteFile(req.Artifact.Path, source, 0o600); err != nil {
				t.Fatal(err)
			}
			desired := object{"@odata.type": appType, "displayName": "Vendor", "description": "Raw installer", "publisher": "Vendor", "primaryBundleId": "org.example.app", "primaryBundleVersion": "1.0", "includedApps": []any{object{"bundleId": "org.example.app", "bundleVersion": "1.0"}}, "minimumSupportedOperatingSystem": object{"v12_0": true}, "ignoreVersionDetection": false}
			update := object{"@odata.type": appType, "primaryBundleVersion": "1.1", "ignoreVersionDetection": false, "minimumSupportedOperatingSystem": object{"v14_0": true}}
			if appType == lobType {
				for _, key := range []string{"primaryBundleId", "primaryBundleVersion", "includedApps"} {
					delete(desired, key)
				}
				desired["bundleId"], desired["buildNumber"], desired["versionNumber"] = "org.example.app", "1.0", "100"
				desired["childApps"] = []any{object{"bundleId": "org.example.app", "buildNumber": "1.0", "versionNumber": "100"}}
				delete(update, "primaryBundleVersion")
				update["buildNumber"] = "1.1"
			}
			if _, err := c.handle(t.Context(), req, desired); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fake.plaintext, source) {
				t.Fatal("macOS upload changed vendor bytes or wrapped them in a ZIP")
			}
			if fake.app["fileName"] != "vendor"+extension || fake.app["setupFilePath"] != nil || fake.app["installCommandLine"] != nil || fake.versions != 1 || fake.commits != 1 {
				t.Fatalf("incorrect native macOS contract: %+v", fake.app)
			}
			if published := publishedMarker(t, fake); published.payload != req.Artifact.SHA256 {
				t.Fatalf("marker does not identify the source bytes: %+v", published)
			}
			fake.app["owner"] = "Remote owner"
			fake.app["ignoreVersionDetection"] = true
			desired = update
			_, err := c.handle(t.Context(), req, desired)
			if err != nil {
				t.Fatal(err)
			}
			operatingSystem := fake.app["minimumSupportedOperatingSystem"].(object)
			if operatingSystem["v12_0"] != false || operatingSystem["v14_0"] != true {
				t.Fatal("minimum OS retained two selections")
			}
			if fake.app["owner"] != "Remote owner" || fake.app["ignoreVersionDetection"] != false || fake.versions != 1 || fake.blobLists != 1 {
				t.Fatal("metadata update lost omission/false or uploaded unchanged bytes")
			}
			req.Method = "plan"
			response, err := c.handle(t.Context(), req, desired)
			if err != nil || len(response.Changes) != 0 {
				t.Fatalf("settled plan: %v, %v", response.Changes, err)
			}
		})
	}
}

func TestMacValidationAndAdoption(t *testing.T) {
	mac := plugin.ReconcileRequest[Config]{Identity: plugin.Identity{Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "example"}}}
	for _, metadata := range []object{
		{"type": "dmg", "install_command": "unsupported"},
		{"type": "win32"},
		{"minimum_os": "12.0"},
		{"minimumSupportedOperatingSystem": object{"v12_0": true}},
		{"primary_bundle_id": "org.example.app"},
		{"included_apps": []any{}},
		{"included_apps": []any{object{"bundleId": "org.example.app", "bundleVersion": "1.0"}}},
		{"pre_install_script": object{"script": "unsupported"}},
	} {
		mac.Metadata = raw(metadata)
		if _, err := compile(mac); err == nil {
			t.Fatalf("accepted unsupported native metadata: %s", raw(metadata))
		}
	}
	fake, c := newGraphFixture(t)
	fake.app = object{"@odata.type": dmgType, "id": "app-1", "notes": "", "committedContentVersion": "1", "publishingState": "published"}
	req := fixtureRequest(t)
	req.Method = "plan"
	req.Artifact.Filename = "existing.dmg"
	req.Config = Config{GraphURL: fake.url, Token: "test-token"}
	req.Identity.Resource.Kind = "MacSoftware"
	req.Metadata = raw(object{"app_id": "app-1", "display_name": "Adopted", "included_apps": []any{object{"id": "org.example.app", "version": "1.0"}}})
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.handle(t.Context(), req, desired)
	if err != nil {
		t.Fatal(err)
	}
	// Graph exposes no digest of adopted content, so adoption publishes it again.
	if !slices.ContainsFunc(response.Changes, func(change plugin.Change) bool { return change.Kind == "content" }) {
		t.Fatalf("adoption did not plan publication: %+v", response.Changes)
	}
	if len(fake.paths) != 1 || fake.paths[0] != "/beta"+appsPath+"/app-1" {
		t.Fatalf("adoption did not read explicit per-software ID: %v", fake.paths)
	}
	fake.app["notes"] = withMarker("", publication{identity: strings.Repeat("a", 64)})
	if _, err := c.handle(t.Context(), req, desired); err == nil || !strings.Contains(err.Error(), "another Stemma identity") {
		t.Fatalf("adopted an app that belongs to other software: %v", err)
	}
	desired["app_id"] = "absent"
	if _, err := c.handle(t.Context(), req, desired); err == nil || !strings.Contains(err.Error(), `app_id "absent" does not exist`) {
		t.Fatalf("missing adopted app: %v", err)
	}
	if fake.writes != 0 || fake.appLists != 0 {
		t.Fatal("adoption planning wrote to or listed the tenant")
	}
	req.Method = "validate"
	req.MinimumOS = &plugin.MinimumOS{Version: "12.0", Origin: "software.minimum_os"}
	if _, err := Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if err := plugin.ValidateSchema(raw(plugin.SchemaFor[Config]()), raw(object{"token": "unused", "app_id": "app-1"})); err == nil {
		t.Fatal("accepted software adoption ID in shared connection")
	}
}

func TestMacLOBPreservesManagedInstallRequirements(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.app = object{"@odata.type": lobType, "id": "app-1", "installAsManaged": true}
	receipt := plugin.Subject{ID: "PackageInfo", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.package", Version: "2.0", HasPayload: true}}
	app := plugin.Subject{ID: "Payload/Example.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "2.0"}}
	other := plugin.Subject{ID: "Payload/Other.app", Parent: "PackageInfo", Kind: "app", InstalledPath: "/Applications/Other.app", App: &plugin.AppFacts{BundleID: "org.example.other", Version: "2.0"}}
	req := lobRequest(object{"type": "lob", "app_id": "app-1", "included_apps": []any{object{"id": "org.example.app", "version": "2.0"}}}, nil, receipt, app, other)
	req.Method = "plan"
	req.Artifact.SHA256 = strings.Repeat("a", 64)
	derived, _, err := Derive(req)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := decodeObject(derived.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err == nil || !strings.Contains(err.Error(), "install_as_managed") {
		t.Fatalf("omitting install_as_managed bypassed the existing app's requirements: %v", err)
	}
	desired["installAsManaged"] = false
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatalf("explicit unmanaged install kept managed restrictions: %v", err)
	}
}

func TestAppSubtypeCannotBeChanged(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := compile(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	req.Artifact.Filename = "vendor.dmg"
	desired = object{"@odata.type": dmgType}
	if _, err := c.handle(t.Context(), req, desired); err == nil || !strings.Contains(err.Error(), "different native subtype") {
		t.Fatalf("identity moved to another subtype: %v", err)
	}
	if fake.creates != 1 {
		t.Fatal("duplicated cross-subtype identity")
	}
}
