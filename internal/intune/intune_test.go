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
	desired, err := validateMetadata(req.Metadata)
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
	req.Metadata = raw(object{"@odata.type": win32Type, "displayName": "Renamed", "isFeatured": false, "installExperience": object{"runAsAccount": "user"}})
	desired, err = validateMetadata(req.Metadata)
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
	req.Metadata = raw(object{"@odata.type": win32Type, "allowedArchitectures": nil, "assignments": []any{}})
	desired, err = validateMetadata(req.Metadata)
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

func TestInterruptedFirstPublicationIsRepeatedInTheSameApp(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.failCommit = true
	req := fixtureRequest(t)
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err == nil {
		t.Fatal("expected interrupted commit")
	}
	if published := publishedMarker(t, fake); published.payload != "" || published.content != "" {
		t.Fatalf("marker records content that was never activated: %+v", published)
	}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	if published := publishedMarker(t, fake); published.content != "2" {
		t.Fatalf("retry did not publish a fresh content version: %+v", published)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 1 || fake.versions != 2 || fake.blobLists != 2 || fake.commits != 2 || fake.app["committedContentVersion"] != "2" {
		t.Fatal("retry created another app or resumed the interrupted upload")
	}
}

func TestUnchangedApplyWaitsForPendingPublication(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := validateMetadata(req.Metadata)
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
			desired, err := validateMetadata(req.Metadata)
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
			desired, err = validateMetadata(req.Metadata)
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
	desired, err := validateMetadata(req.Metadata)
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
		`{"@odata.type":"#microsoft.graph.macOSLobApp"}`,
		`{"@odata.type":"#microsoft.graph.win32LobApp","id":"read-only"}`,
		`{"@odata.type":"#microsoft.graph.win32LobApp","isFeatured":null}`,
		`{"@odata.type":"#microsoft.graph.win32LobApp","installExperience":{"runAsAccount":null}}`,
		`{"@odata.type":"#microsoft.graph.win32LobApp","assignments":null}`,
	} {
		if _, err := validateMetadata([]byte(data)); err == nil {
			t.Fatalf("accepted unsupported metadata %s", data)
		}
	}
}

func TestPayloadChangeActivatesFreshVersionWithItsMarker(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := validateMetadata(req.Metadata)
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
	// A marker written apart from activation could describe content that is not active.
	activated := false
	for _, patch := range fake.patches {
		_, activates := patch["committedContentVersion"]
		_, marks := patch["notes"]
		if activates != marks {
			t.Fatalf("activation and marker were written separately: %+v", fake.patches)
		}
		activated = activated || activates
	}
	if !activated {
		t.Fatalf("no patch activated the new content: %+v", fake.patches)
	}
}

func TestMarkerDriftRepublishesContent(t *testing.T) {
	for _, drift := range []string{"edited", "removed", "other version activated"} {
		t.Run(drift, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			req := fixtureRequest(t)
			desired, err := validateMetadata(req.Metadata)
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
			if restored := publishedMarker(t, fake); restored.content != "2" || restored.payload != published.payload {
				t.Fatalf("marker was not rewritten for fresh content: %+v", restored)
			}
			fake.mu.Lock()
			writes := fake.writes
			if fake.creates != 1 || fake.versions != 2 || !strings.HasPrefix(text(fake.app["notes"]), "Edited by an administrator\n") {
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
	desired, err := validateMetadata(req.Metadata)
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

func fixtureRequest(t *testing.T) plugin.ReconcileRequest {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup.cmd")
	data := []byte("@echo off\r\necho fixture\r\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return plugin.ReconcileRequest{Method: "apply", Identity: plugin.Identity{Project: "example", Software: "test", Destination: "intune"}, Artifact: plugin.Artifact{Path: path, Filename: "setup.cmd", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}, Metadata: raw(object{
		"@odata.type": win32Type, "displayName": "Fixture", "description": "Test app", "publisher": "Fixture Publisher",
		"installCommandLine": "setup.cmd", "uninstallCommandLine": "setup.cmd /remove", "minimumSupportedWindowsRelease": "Windows11_23H2", "allowedArchitectures": "x64",
		"installExperience": object{"runAsAccount": "system"},
		"rules":             []any{object{"@odata.type": "#microsoft.graph.win32LobAppProductCodeRule", "ruleType": "detection", "productCode": "{AC01F3D3-C5D5-40DB-9E8C-ED53982E17ED}", "productVersionOperator": "notConfigured"}},
		"assignments":       []any{object{"intent": "required", "target": object{"@odata.type": "#microsoft.graph.groupAssignmentTarget", "groupId": "group-1"}}},
	})}
}

type graphFixture struct {
	mu          sync.Mutex
	url         string
	app         object
	assignments []any
	files       map[string]object // content version ID to its file, nil until an upload creates one
	versionBase int               // numbers new content versions after history a test seeds
	blocks      map[string][]byte
	uploaded    []byte
	plaintext   []byte

	creates, versions, blobLists, commits, assigns, appLists, writes int
	failBlob, failCommit, failRelationships                          bool

	expectedAPI        string
	pendingAppReads    int
	contentTypes       []string
	paths              []string
	patches            []object
	deletedVersions    []string
	relations          map[string][]object
	relatedApps        map[string]object
	relationshipWrites int
}

func newGraphFixture(t *testing.T) (*graphFixture, *client) {
	t.Helper()
	fake := &graphFixture{blocks: map[string][]byte{}, files: map[string]object{}, relations: map[string][]object{}, relatedApps: map[string]object{}}
	server := httptest.NewTLSServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	fake.url = server.URL
	auth, err := authentication.NewApiKeyAuthenticationProvider("Bearer test-token", "Authorization", authentication.HEADER_KEYLOCATION)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newSDKClient(server.URL+"/v1.0", auth, server.Client().Transport)
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
			f.uploaded = data
			f.blobLists++
		default:
			var list struct {
				Latest []string `xml:"Latest"`
			}
			if err := xml.Unmarshal(data, &list); err != nil {
				http.Error(w, "bad block list", http.StatusBadRequest)
				return
			}
			f.uploaded = nil
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
	api := f.expectedAPI
	if api == "" {
		api = "v1.0"
	}
	if strings.Contains(r.URL.Path, "/relationships") || strings.HasSuffix(r.URL.Path, "/updateRelationships") {
		api = "beta"
	}
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
		maps.Copy(f.app, body)
		if body["committedContentVersion"] != nil {
			f.app["publishingState"] = "published"
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/contentVersions") && r.Method == http.MethodPost:
		if f.app == nil || !strings.Contains(path, "/"+strings.TrimPrefix(text(f.app["@odata.type"]), "#microsoft.")+"/contentVersions") {
			http.Error(w, "wrong subtype content path", http.StatusBadRequest)
			return
		}
		f.contentTypes = append(f.contentTypes, text(f.app["@odata.type"]))
		f.versions++
		id := strconv.Itoa(f.versionBase + f.versions)
		f.files[id] = nil
		write(object{"id": id})
	case strings.HasSuffix(path, "/contentVersions") && r.Method == http.MethodGet:
		items := []object{}
		for id := range f.files {
			items = append(items, object{"id": id})
		}
		write(object{"value": items})
	case strings.Contains(path, "/contentVersions/") && r.Method == http.MethodDelete:
		if version == f.app["committedContentVersion"] {
			http.Error(w, "cannot delete active content", http.StatusBadRequest)
			return
		}
		delete(f.files, version)
		f.deletedVersions = append(f.deletedVersions, version)
		w.WriteHeader(http.StatusNoContent)
	case strings.HasSuffix(path, "/files") && r.Method == http.MethodPost:
		body["id"] = "file-1"
		body["uploadState"] = "azureStorageUriRequestSuccess"
		body["azureStorageUri"] = f.url + "/blob?sig=temporary"
		f.files[version] = body
		write(body)
	case strings.HasSuffix(path, "/files") && r.Method == http.MethodGet:
		items := []object{}
		if file := f.files[version]; file != nil {
			items = append(items, file)
		}
		write(object{"value": items})
	case strings.HasSuffix(path, "/files/file-1") && r.Method == http.MethodGet:
		write(f.files[version])
	case strings.HasSuffix(path, "/commit"):
		file := f.files[version]
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
	for _, appType := range []string{dmgType, pkgType} {
		t.Run(appType, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			fake.expectedAPI = "beta"
			extension := ".dmg"
			if appType == pkgType {
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
			req.Metadata = raw(object{"@odata.type": appType, "displayName": "Vendor", "description": "Raw installer", "publisher": "Vendor", "primaryBundleId": "org.example.app", "primaryBundleVersion": "1.0", "includedApps": []any{object{"bundleId": "org.example.app", "bundleVersion": "1.0"}}, "minimumSupportedOperatingSystem": object{"v12_0": true}, "ignoreVersionDetection": false})
			desired, err := validateMetadata(req.Metadata)
			if err != nil {
				t.Fatal(err)
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
			req.Metadata = raw(object{"@odata.type": appType, "primaryBundleVersion": "1.1", "ignoreVersionDetection": false, "minimumSupportedOperatingSystem": object{"v14_0": true}})
			desired, err = validateMetadata(req.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.handle(t.Context(), req, desired)
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
	for _, metadata := range []object{
		{"@odata.type": dmgType, "installCommandLine": "unsupported"},
		{"@odata.type": win32Type, "primaryBundleId": "org.example.app"},
		{"@odata.type": dmgType, "minimumSupportedOperatingSystem": object{"v12_0": true, "v13_0": true}},
		{"@odata.type": dmgType, "minimumSupportedOperatingSystem": object{"v99_0": true}},
		{"@odata.type": pkgType, "includedApps": []any{}},
		{"@odata.type": pkgType, "preInstallScript": object{"scriptContent": "unsupported"}},
	} {
		if _, err := validateMetadata(raw(metadata)); err == nil {
			t.Fatalf("accepted unsupported native metadata: %s", raw(metadata))
		}
	}
	fake, c := newGraphFixture(t)
	fake.expectedAPI = "beta"
	fake.app = object{"@odata.type": dmgType, "id": "app-1", "notes": "", "committedContentVersion": "1", "publishingState": "published"}
	req := fixtureRequest(t)
	req.Method = "plan"
	req.Artifact.Filename = "existing.dmg"
	req.Config = raw(object{"graph_url": fake.url + "/v1.0", "token": "test-token"})
	req.Metadata = raw(object{"@odata.type": dmgType, "app_id": "app-1", "displayName": "Adopted"})
	desired, err := validateMetadata(req.Metadata)
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
	if _, err := Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	req.Config = raw(object{"token": "unused", "app_id": "app-1"})
	if _, err := Handle(t.Context(), req); err == nil {
		t.Fatal("accepted software adoption ID in shared connection")
	}
}

func TestAppSubtypeCannotBeChanged(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, desired); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.expectedAPI = "beta"
	fake.mu.Unlock()
	req.Artifact.Filename = "vendor.dmg"
	desired = object{"@odata.type": dmgType}
	if _, err := c.handle(t.Context(), req, desired); err == nil || !strings.Contains(err.Error(), "different native subtype") {
		t.Fatalf("identity moved to another subtype: %v", err)
	}
	if fake.creates != 1 {
		t.Fatal("duplicated cross-subtype identity")
	}
}
