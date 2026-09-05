package intune

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/plugin"
)

func TestUploadThenMetadataAndAssignmentOwnership(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	var b binding
	if err := json.Unmarshal(response.Binding, &b); err != nil {
		t.Fatal(err)
	}
	if b.AppID != "app-1" || b.ContentVersion != "1" || b.PayloadSHA256 == "" || b.EnvelopeSHA256 == "" || b.Pending != nil {
		t.Fatalf("incomplete binding: %+v", b)
	}
	fake.mu.Lock()
	if fake.creates != 1 || fake.versions != 1 || fake.blobLists != 1 || fake.commits != 1 || fake.assigns != 1 {
		t.Fatal("upload did not complete all required stages")
	}
	if fake.app["setupFilePath"] != "setup.cmd" || fake.app["fileName"] != "setup.intunewin" || fake.app["committedContentVersion"] != "1" {
		t.Fatalf("native content metadata: %+v", fake.app)
	}
	fake.app["owner"] = "Remote owner"
	fake.app["isFeatured"] = true
	fake.app["installExperience"].(object)["deviceRestartBehavior"] = "suppress"
	fake.mu.Unlock()
	req.Binding = response.Binding
	req.Metadata = raw(object{"@odata.type": win32Type, "displayName": "Renamed", "isFeatured": false, "installExperience": object{"runAsAccount": "user"}})
	desired, err = validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	if fake.versions != 1 || fake.blobLists != 1 || fake.assigns != 1 {
		t.Fatal("metadata-only change uploaded or reassigned")
	}
	if fake.app["owner"] != "Remote owner" || fake.app["isFeatured"] != false || fake.app["installExperience"].(object)["deviceRestartBehavior"] != "suppress" {
		t.Fatalf("lost omitted fields or false: %+v", fake.app)
	}
	fake.mu.Unlock()
	req.Binding = response.Binding
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("unchanged reconciliation: %+v, %v", response.Changes, err)
	}
	req.Metadata = raw(object{"@odata.type": win32Type, "allowedArchitectures": nil, "assignments": []any{}})
	desired, err = validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.app["allowedArchitectures"] != nil || len(fake.assignments) != 0 || fake.assigns != 2 {
		t.Fatal("explicit null or assignment clear was lost")
	}
}

func TestInterruptedCommitResumesWithoutReupload(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.failCommit = true
	req := fixtureRequest(t)
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err == nil {
		t.Fatal("expected interrupted commit")
	}
	var b binding
	if err := json.Unmarshal(response.Binding, &b); err != nil {
		t.Fatal(err)
	}
	if b.AppID == "" || b.Pending == nil || b.Pending.Stage != "committing" || b.Pending.EncryptionInfo.Mac == "" {
		t.Fatalf("lost resumable progress: %+v", b)
	}
	if strings.Contains(string(response.Binding), "sig=") {
		t.Fatal("persisted expiring SAS URL")
	}
	req.Binding = response.Binding
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	// Unmarshal into a fresh value because absent fields intentionally clear state.
	b = binding{}
	if err := json.Unmarshal(response.Binding, &b); err != nil {
		t.Fatal(err)
	}
	if b.Pending != nil || b.ContentVersion != "1" {
		t.Fatalf("unfinished resumed binding: %+v", b)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 1 || fake.versions != 1 || fake.blobLists != 1 || fake.commits != 1 {
		t.Fatal("resuming a completed commit repeated creation or upload")
	}
}

func TestRecoverBindingByIdentityMarker(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, configuration{}, desired); err != nil {
		t.Fatal(err)
	}
	req.Method = "plan"
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("lost-state recovery: %+v, %v", response.Changes, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 1 || fake.versions != 1 {
		t.Fatal("recovery duplicated app/content")
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
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil || len(response.Changes) == 0 {
		t.Fatalf("plan: %+v, %v", response, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 0 || fake.versions != 0 || fake.blobLists != 0 {
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

func TestUncertainCreationIsNotRepeated(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	hash := sha256.Sum256(raw(req.Identity))
	req.Binding = raw(binding{Identity: hex.EncodeToString(hash[:]), UncertainCreate: true})
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, configuration{}, desired); err == nil {
		t.Fatal("retried uncertain creation")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.creates != 0 {
		t.Fatal("issued duplicate app creation")
	}
}

func fixtureRequest(t *testing.T) plugin.Request {
	t.Helper()
	path := filepath.Join(t.TempDir(), "setup.cmd")
	data := []byte("@echo off\r\necho fixture\r\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return plugin.Request{Protocol: plugin.ProtocolVersion, Method: "apply", Identity: plugin.Identity{Project: "example", Recipe: "test", Destination: "intune"}, Artifact: plugin.Artifact{Path: path, Filename: "setup.cmd", SHA256: hex.EncodeToString(digest[:]), Size: int64(len(data))}, Metadata: raw(object{
		"@odata.type": win32Type, "displayName": "Fixture", "description": "Test app", "publisher": "Fixture Publisher",
		"installCommandLine": "setup.cmd", "uninstallCommandLine": "setup.cmd /remove", "minimumSupportedWindowsRelease": "Windows11_23H2", "allowedArchitectures": "x64",
		"installExperience": object{"runAsAccount": "system"},
		"rules":             []any{object{"@odata.type": "#microsoft.graph.win32LobAppProductCodeRule", "ruleType": "detection", "productCode": "{AC01F3D3-C5D5-40DB-9E8C-ED53982E17ED}", "productVersionOperator": "notConfigured"}},
		"assignments":       []any{object{"intent": "required", "target": object{"@odata.type": "#microsoft.graph.groupAssignmentTarget", "groupId": "group-1"}}},
	})}
}

type graphFixture struct {
	mu                                             sync.Mutex
	url                                            string
	app                                            object
	assignments                                    []any
	file                                           object
	blocks                                         map[string][]byte
	uploaded                                       []byte
	creates, versions, blobLists, commits, assigns int
	failCommit                                     bool
	plaintext                                      []byte
	expectedAPI                                    string
	contentTypes                                   []string
	paths                                          []string
}

func newGraphFixture(t *testing.T) (*graphFixture, *client) {
	t.Helper()
	fake := &graphFixture{blocks: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)
	fake.url = server.URL
	return fake, &client{base: server.URL + "/v1.0", token: "test-token", http: server.Client(), pollInterval: time.Millisecond}
}

func (f *graphFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	var body object
	if r.Method == http.MethodPost || r.Method == http.MethodPatch {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid JSON", 400)
			return
		}
	}
	if r.URL.Path == "/blob" {
		if r.Header.Get("Authorization") != "" {
			http.Error(w, "leaked Graph token", 400)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", 400)
			return
		}
		if r.URL.Query().Get("comp") == "block" {
			f.blocks[r.URL.Query().Get("blockid")] = data
		} else {
			var list struct {
				Latest []string `xml:"Latest"`
			}
			if err := xml.Unmarshal(data, &list); err != nil {
				http.Error(w, "bad block list", 400)
				return
			}
			f.uploaded = nil
			for _, id := range list.Latest {
				f.uploaded = append(f.uploaded, f.blocks[id]...)
			}
			f.blobLists++
		}
		w.WriteHeader(201)
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
	if !strings.HasPrefix(r.URL.Path, "/"+api+"/") {
		http.Error(w, "wrong API version", http.StatusBadRequest)
		return
	}
	f.paths = append(f.paths, r.URL.Path)
	path := strings.TrimPrefix(r.URL.Path, "/"+api)
	switch {
	case path == appsPath && r.Method == http.MethodGet:
		apps := []any{}
		if f.app != nil {
			apps = append(apps, f.app)
		}
		write(object{"value": apps})
	case path == appsPath && r.Method == http.MethodPost:
		f.creates++
		f.app = body
		f.app["id"] = "app-1"
		f.app["publishingState"] = "notPublished"
		write(f.app)
	case path == appsPath+"/app-1" && r.Method == http.MethodGet:
		write(f.app)
	case path == appsPath+"/app-1" && r.Method == http.MethodPatch:
		maps.Copy(f.app, body)
		if body["committedContentVersion"] != nil {
			f.app["publishingState"] = "published"
		}
		w.WriteHeader(204)
	case strings.HasSuffix(path, "/contentVersions") && r.Method == http.MethodPost:
		if f.app == nil || !strings.Contains(path, "/"+strings.TrimPrefix(text(f.app["@odata.type"]), "#")+"/contentVersions") {
			http.Error(w, "wrong subtype content path", http.StatusBadRequest)
			return
		}
		f.contentTypes = append(f.contentTypes, text(f.app["@odata.type"]))
		f.versions++
		write(object{"id": fmt.Sprint(f.versions)})
	case strings.HasSuffix(path, "/files") && r.Method == http.MethodPost:
		f.file = body
		f.file["id"] = "file-1"
		f.file["uploadState"] = "azureStorageUriRequestSuccess"
		f.file["azureStorageUri"] = f.url + "/blob?sig=temporary"
		write(f.file)
	case strings.HasSuffix(path, "/files/file-1") && r.Method == http.MethodGet:
		write(f.file)
	case strings.HasSuffix(path, "/commit"):
		info, ok := body["fileEncryptionInfo"].(object)
		if !ok || len(f.uploaded) < 48 {
			http.Error(w, "bad commit body", 400)
			return
		}
		macKey, err := base64.StdEncoding.DecodeString(text(info["macKey"]))
		if err != nil {
			http.Error(w, "bad key", 400)
			return
		}
		mac := hmac.New(sha256.New, macKey)
		mac.Write(f.uploaded[32:])
		recorded, err := base64.StdEncoding.DecodeString(text(info["mac"]))
		if err != nil || !hmac.Equal(mac.Sum(nil), recorded) || !hmac.Equal(f.uploaded[:32], recorded) {
			http.Error(w, "commit metadata does not authenticate uploaded bytes", 400)
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
		if !bytes.Equal(actualDigest[:], digest) || f.file["size"] != float64(len(plaintext)) || f.file["sizeEncrypted"] != float64(len(f.uploaded)) || info["profileIdentifier"] != "ProfileVersion1" || info["fileDigestAlgorithm"] != "SHA256" {
			http.Error(w, "bad plaintext digest or sizes", http.StatusBadRequest)
			return
		}
		f.plaintext = plaintext
		f.commits++
		f.file["isCommitted"] = true
		f.file["uploadState"] = "commitFileSuccess"
		if f.failCommit {
			f.failCommit = false
			http.Error(w, "ambiguous commit", 500)
			return
		}
		w.WriteHeader(204)
	case strings.HasSuffix(path, "/assignments"):
		write(object{"value": f.assignments})
	case strings.HasSuffix(path, "/assign"):
		f.assigns++
		f.assignments, _ = body["mobileAppAssignments"].([]any)
		w.WriteHeader(204)
	default:
		http.Error(w, "unexpected route", 404)
	}
}

func TestMacRawContentAndMetadata(t *testing.T) {
	for _, appType := range []string{dmgType, pkgType} {
		t.Run(appType, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			extension := ".dmg"
			if appType == pkgType {
				fake.expectedAPI = "beta"
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
			response, err := c.handle(t.Context(), req, configuration{}, desired)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fake.plaintext, source) {
				t.Fatal("macOS upload changed vendor bytes or wrapped them in a ZIP")
			}
			if fake.app["fileName"] != "vendor"+extension || fake.app["setupFilePath"] != nil || fake.app["installCommandLine"] != nil || fake.versions != 1 || fake.commits != 1 {
				t.Fatalf("incorrect native macOS contract: %+v", fake.app)
			}
			var b binding
			if err := json.Unmarshal(response.Binding, &b); err != nil {
				t.Fatal(err)
			}
			if b.PayloadSHA256 != req.Artifact.SHA256 || b.EnvelopeSHA256 == req.Artifact.SHA256 {
				t.Fatal("confused source identity and randomized encrypted transport")
			}
			fake.app["owner"] = "Remote owner"
			fake.app["ignoreVersionDetection"] = true
			req.Binding = response.Binding
			req.Metadata = raw(object{"@odata.type": appType, "primaryBundleVersion": "1.1", "ignoreVersionDetection": false, "minimumSupportedOperatingSystem": object{"v13_0": true}})
			desired, err = validateMetadata(req.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.handle(t.Context(), req, configuration{}, desired)
			if err != nil {
				t.Fatal(err)
			}
			operatingSystem := fake.app["minimumSupportedOperatingSystem"].(object)
			if operatingSystem["v12_0"] != false || operatingSystem["v13_0"] != true {
				t.Fatal("minimum OS retained two selections")
			}
			if fake.app["owner"] != "Remote owner" || fake.app["ignoreVersionDetection"] != false || fake.versions != 1 || fake.blobLists != 1 {
				t.Fatal("metadata update lost omission/false or uploaded unchanged bytes")
			}
			req.Binding = nil
			req.Method = "plan"
			response, err = c.handle(t.Context(), req, configuration{}, desired)
			if err != nil || len(response.Changes) != 0 {
				t.Fatalf("cold binding recovery: %v, %v", response.Changes, err)
			}
		})
	}
}

func TestMacValidationAndAdoption(t *testing.T) {
	for _, metadata := range []object{
		{"@odata.type": dmgType, "installCommandLine": "unsupported"},
		{"@odata.type": win32Type, "primaryBundleId": "org.example.app"},
		{"@odata.type": dmgType, "minimumSupportedOperatingSystem": object{"v12_0": true, "v13_0": true}},
		{"@odata.type": dmgType, "minimumSupportedOperatingSystem": object{"v14_0": true}},
		{"@odata.type": pkgType, "includedApps": []any{}},
		{"@odata.type": pkgType, "preInstallScript": object{"scriptContent": "unsupported"}},
	} {
		if _, err := validateMetadata(raw(metadata)); err == nil {
			t.Fatalf("accepted unsupported native metadata: %s", raw(metadata))
		}
	}
	fake, _ := newGraphFixture(t)
	fake.app = object{"@odata.type": dmgType, "id": "app-1", "notes": "", "committedContentVersion": "1", "publishingState": "published"}
	req := fixtureRequest(t)
	req.Method = "plan"
	req.Artifact.Filename = "existing.dmg"
	req.Config = raw(object{"graph_url": fake.url + "/v1.0", "token_env": "STEMMA_TEST_INTUNE_TOKEN"})
	req.Metadata = raw(object{"@odata.type": dmgType, "app_id": "app-1", "displayName": "Adopted"})
	t.Setenv("STEMMA_TEST_INTUNE_TOKEN", "test-token")
	if _, err := Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if len(fake.paths) != 1 || fake.paths[0] != "/v1.0"+appsPath+"/app-1" {
		t.Fatalf("adoption did not read explicit per-recipe ID: %v", fake.paths)
	}
	req.Method = "validate"
	if _, err := Handle(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	req.Config = raw(object{"token_env": "unused", "app_id": "app-1"})
	if _, err := Handle(t.Context(), req); err == nil {
		t.Fatal("accepted recipe adoption ID in shared connection")
	}
}

func TestMacInterruptedCommitRetainsActualTransport(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.expectedAPI = "beta"
	fake.failCommit = true
	req := fixtureRequest(t)
	req.Artifact.Filename = "vendor.pkg"
	req.Metadata = raw(object{"@odata.type": pkgType, "displayName": "Vendor", "description": "Raw installer", "publisher": "Vendor", "primaryBundleId": "org.example.app", "primaryBundleVersion": "1.0", "includedApps": []any{object{"bundleId": "org.example.app", "bundleVersion": "1.0"}}, "minimumSupportedOperatingSystem": object{"v26_0": true}})
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err == nil {
		t.Fatal("expected lost commit response")
	}
	var partial binding
	if err := json.Unmarshal(response.Binding, &partial); err != nil {
		t.Fatal(err)
	}
	if partial.Pending == nil || partial.Pending.Stage != "committing" || partial.Pending.EnvelopeSHA256 == "" {
		t.Fatal("lost actual ciphertext identity")
	}
	uploaded := append([]byte(nil), fake.uploaded...)
	req.Binding = response.Binding
	if _, err := c.handle(t.Context(), req, configuration{}, desired); err != nil {
		t.Fatal(err)
	}
	if fake.versions != 1 || fake.blobLists != 1 || fake.commits != 1 || !bytes.Equal(uploaded, fake.uploaded) {
		t.Fatal("retry regenerated randomized transport after commit")
	}
}

func TestBoundSubtypeCannotBeChanged(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	desired, err := validateMetadata(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.handle(t.Context(), req, configuration{}, desired); err != nil {
		t.Fatal(err)
	}
	req.Artifact.Filename = "vendor.dmg"
	desired = object{"@odata.type": dmgType}
	if _, err := c.handle(t.Context(), req, configuration{}, desired); err == nil {
		t.Fatal("created another app for the same identity under a new subtype")
	}
	if fake.creates != 1 {
		t.Fatal("duplicated cross-subtype identity")
	}
}
