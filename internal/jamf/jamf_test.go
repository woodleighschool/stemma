package jamf

import (
	"context"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

	"github.com/woodleighschool/stemma/plugin"
)

func TestPackageFirstPublicationUnchangedAndMetadataOnly(t *testing.T) {
	server, request := newFixture(t)
	request.Metadata = raw(map[string]any{"packageName": "Managed package", "info": "Initial info", "rebootRequired": true})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil || len(plan.Changes) != 4 {
		t.Fatalf("first plan: %+v: %v", plan, err)
	}
	if got := server.count("POST " + packagePath); got != 0 {
		t.Fatal("plan created a package")
	}
	request.Method = "apply"
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 1 || server.count("POST "+packagePath+"/1/upload") != 1 {
		t.Fatal("first publication did not create and upload exactly once")
	}
	steps := server.operations()
	upload := slices.Index(steps, "POST "+packagePath+"/1/upload")
	metadata := slices.Index(steps, "PUT "+packagePath+"/1")
	if upload < 0 || metadata < upload {
		t.Fatalf("managed metadata activated before content: %v", steps)
	}
	request.Binding = response.Binding
	response, err = Handle(t.Context(), request)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("unchanged apply: %+v: %v", response, err)
	}
	if server.count("POST "+packagePath+"/1/upload") != 1 || server.count("PUT "+packagePath+"/1") != 1 {
		t.Fatal("unchanged apply wrote content or metadata")
	}
	server.set("notes", raw("operator-owned note"))
	request.Metadata = raw(map[string]any{"packageName": "Renamed", "info": nil, "rebootRequired": false})
	_, err = Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath+"/1/upload") != 1 {
		t.Fatal("metadata-only change uploaded content")
	}
	current := server.record()
	if stringField(current, "packageName") != "Renamed" || stringField(current, "notes") != "operator-owned note" || string(current["info"]) != "null" || string(current["rebootRequired"]) != "false" {
		t.Fatalf("presence ownership failed: %s", raw(current))
	}
	server.set("info", raw("now owned remotely"))
	request.Metadata = raw(map[string]any{"packageName": "Renamed"})
	response, err = Handle(t.Context(), request)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("omitted fields should remain unmanaged: %+v: %v", response, err)
	}
	if stringField(server.record(), "info") != "now owned remotely" {
		t.Fatal("omission restored an old managed value")
	}
}

func TestRecoverMissingBindingAndStablePackageContentUpdate(t *testing.T) {
	server, request := newFixture(t)
	_, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Binding = nil
	request.Artifact = fixtureArtifact(t, "second immutable package bytes")
	request.Metadata = raw(map[string]any{"packageName": "New version"})
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 1 || server.count("POST "+packagePath+"/1/upload") != 2 {
		t.Fatal("binding loss created a duplicate or failed content replacement")
	}
	var state binding
	if err := json.Unmarshal(response.Binding, &state); err != nil {
		t.Fatal(err)
	}
	if state.PackageID != "1" || state.PayloadSHA256 != request.Artifact.SHA256 {
		t.Fatalf("wrong replacement binding: %+v", state)
	}
	current := server.record()
	if stringField(current, "sha256") != request.Artifact.SHA256 || stringField(current, "packageName") != "New version" {
		t.Fatalf("replacement not read back: %s", raw(current))
	}
}

func TestAmbiguousCreateAndUploadAreReobserved(t *testing.T) {
	for _, phase := range []string{"create", "upload", "metadata"} {
		t.Run(phase, func(t *testing.T) {
			server, request := newFixture(t)
			server.failAfter = phase
			request.Metadata = raw(map[string]any{"packageName": "Managed"})
			response, err := Handle(t.Context(), request)
			if err != nil {
				t.Fatalf("committed %s was not recovered: %v", phase, err)
			}
			if len(response.Binding) == 0 || server.count("POST "+packagePath) != 1 || server.count("POST "+packagePath+"/1/upload") != 1 || server.count("PUT "+packagePath+"/1") != 1 {
				t.Fatalf("ambiguous %s was replayed", phase)
			}
		})
	}
}

func TestFailedUploadRetainsBindingForRetry(t *testing.T) {
	server, request := newFixture(t)
	server.failBeforeUpload = true
	response, err := Handle(t.Context(), request)
	if err == nil || len(response.Binding) == 0 {
		t.Fatalf("failed upload lost binding: %+v: %v", response, err)
	}
	if strings.Contains(err.Error(), "test-client-secret") {
		t.Fatal("credential leaked in HTTP error")
	}
	var state binding
	if err := json.Unmarshal(response.Binding, &state); err != nil || state.PackageID != "1" || state.PendingCreate {
		t.Fatalf("failed upload did not retain the created package ID: %s: %v", response.Binding, err)
	}
	request.Binding = response.Binding
	server.failBeforeUpload = false
	_, err = Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 1 || server.count("POST "+packagePath+"/1/upload") != 2 {
		t.Fatal("failed upload recovery duplicated metadata creation")
	}
}

func TestUnresolvedCreationCannotBeBlindlyRepeated(t *testing.T) {
	server, request := newFixture(t)
	server.failAfter = "create"
	server.hideDiscovery = true
	response, err := Handle(t.Context(), request)
	if err == nil {
		t.Fatal("invisible ambiguous creation unexpectedly succeeded")
	}
	request.Binding = response.Binding
	if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "still unresolved") {
		t.Fatalf("unresolved create not retained: %v", err)
	}
	if server.count("POST "+packagePath) != 1 {
		t.Fatal("unresolved create was blindly replayed")
	}
	server.hideDiscovery = false
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatalf("visible committed record was not recovered: %v", err)
	}
	if server.count("POST "+packagePath) != 1 {
		t.Fatal("recovery duplicated a committed package")
	}
}

func TestReadbackRequiresReadyAndStrongContentDigest(t *testing.T) {
	content := payload{filename: "vendor.pkg", sha256: strings.Repeat("a", 64), sha3512: strings.Repeat("b", 128)}
	for _, fields := range []map[string]json.RawMessage{
		{"fileName": raw(content.filename), "cloudTransferStatus": raw("PENDING"), "sha256": raw(content.sha256)},
		{"fileName": raw(content.filename), "cloudTransferStatus": raw("READY"), "hashType": raw("MD5"), "hashValue": raw("whatever")},
		{"fileName": raw(content.filename), "cloudTransferStatus": raw("READY"), "sha256": raw(strings.Repeat("c", 64))},
		{"fileName": raw("other.pkg"), "cloudTransferStatus": raw("READY"), "sha256": raw(content.sha256)},
	} {
		if contentMatches(&observed{Fields: fields}, content) {
			t.Fatalf("unverified readback matched: %s", raw(fields))
		}
	}
	if !contentMatches(&observed{Fields: map[string]json.RawMessage{"fileName": raw(content.filename), "cloudTransferStatus": raw("READY"), "hashType": raw("SHA3_512"), "hashValue": raw(content.sha3512)}}, content) {
		t.Fatal("valid native SHA3-512 readback rejected")
	}
}

func TestPackageAdoptionAndUnknownRemoteFields(t *testing.T) {
	server, request := newFixture(t)
	_, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	server.set("fileName", raw("vendor-original.pkg"))
	request.Binding = nil
	request.Metadata = raw(map[string]any{"package_id": "1", "packageName": "Adopted"})
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 1 {
		t.Fatal("explicit adoption created a duplicate")
	}
	remote := raw(map[string]any{"label": "must not be erased", "enabled": false, "nested": map[string]any{"notes": nil}, "values": []any{"one", "two"}})
	server.set("newServerField", remote)
	request.Binding = response.Binding
	request.Metadata = raw(map[string]any{"packageName": "Further change", "notes": "", "priority": 0})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	current := server.record()
	if !equalJSON(current["newServerField"], remote) || string(current["notes"]) != `""` || string(current["priority"]) != "0" {
		t.Fatalf("native values or omitted remote fields changed: %s", raw(current))
	}
}

func TestAuthenticationRefreshesBeforeExpiry(t *testing.T) {
	server, request := newFixture(t)
	server.tokenLifetime = 1
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST /api/v1/oauth/token") < 2 {
		t.Fatal("expiring token was not refreshed during publication")
	}
}

func TestReadRetriesWithoutReplayingCreation(t *testing.T) {
	server, request := newFixture(t)
	server.failReads = 1
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("GET "+packagePath) != 2 || server.count("POST "+packagePath) != 1 {
		t.Fatal("transient read failed to retry safely")
	}
}

func TestAuthenticationHonorsCancellation(t *testing.T) {
	_, request := newFixture(t)
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	request.Config = raw(configuration{URL: server.URL, ClientIDEnv: "STEMMA_TEST_JAMF_CLIENT_ID", ClientSecretEnv: "STEMMA_TEST_JAMF_CLIENT_SECRET"})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Handle(ctx, request)
		done <- err
	}()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("authentication failed before request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("authentication request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("authentication cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("authentication outlived cancelled publication")
	}
}

func TestAuthenticationRejectsCrossOriginRedirect(t *testing.T) {
	_, request := newFixture(t)
	received := make(chan struct{}, 1)
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		writeJSON(t, w, map[string]any{"access_token": "unexpected-token", "expires_in": 1800})
	}))
	t.Cleanup(target.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	request.Config = raw(configuration{URL: server.URL, ClientIDEnv: "STEMMA_TEST_JAMF_CLIENT_ID", ClientSecretEnv: "STEMMA_TEST_JAMF_CLIENT_SECRET"})
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("authentication followed a cross-origin redirect")
	}
	select {
	case <-received:
		t.Fatal("OAuth client credentials reached another origin")
	default:
	}
}

func TestStrictValidationPreservesNullFalseAndZero(t *testing.T) {
	_, request := newFixture(t)
	request.Method = "validate"
	for _, metadata := range []string{`{"priority":0,"rebootRequired":false,"notes":null}`, `{}`} {
		request.Metadata = json.RawMessage(metadata)
		if _, err := Handle(t.Context(), request); err != nil {
			t.Fatalf("valid ownership rejected: %s: %v", metadata, err)
		}
	}
	for _, metadata := range []string{`{"priority":null}`, `{"packageName":null}`, `{"policies":[]}`, `{"fileName":"overridden.pkg"}`, `{"sha256":"untrusted"}`, `{"priority":"10"}`, `{"notes":1}`, `{"package_id":"../1"}`, `{"notes":"a","notes":"b"}`} {
		request.Metadata = json.RawMessage(metadata)
		if _, err := Handle(t.Context(), request); err == nil {
			t.Fatalf("invalid metadata accepted: %s", metadata)
		}
	}
	request.Metadata = raw(map[string]any{})
	request.Config = raw(map[string]any{"url": "https://example.com", "client_id_env": "ID", "client_secret_env": "SECRET", "package_id": "1"})
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("accepted recipe adoption in shared destination config")
	}
}

func TestDigestAndIdentityMismatchBlockWrites(t *testing.T) {
	server, request := newFixture(t)
	request.Artifact.SHA256 = strings.Repeat("0", 64)
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("accepted bytes with wrong immutable descriptor")
	}
	if server.count("POST "+packagePath) != 0 {
		t.Fatal("created metadata before validating artifact identity")
	}
	request.Artifact = fixtureArtifact(t, "valid bytes")
	request.Binding = raw(binding{Server: "https://other.example", IdentitySHA256: identityDigest(request.Identity), PackageID: "1"})
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("binding from another server was reused")
	}
}

func TestPublicationHonorsCancellation(t *testing.T) {
	server, request := newFixture(t)
	response, err := Handle(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.Binding = response.Binding
	server.set("cloudTransferStatus", raw("IN_PROGRESS"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Handle(ctx, request); err == nil {
		t.Fatal("cancelled publication succeeded")
	}
}

type fakeServer struct {
	t                *testing.T
	mu               sync.Mutex
	packages         map[string]map[string]json.RawMessage
	requests         []string
	version          int
	failAfter        string
	failBeforeUpload bool
	hideDiscovery    bool
	tokenLifetime    int
	tokens           int
	failReads        int
}

func newFixture(t *testing.T) (*fakeServer, plugin.Request) {
	t.Helper()
	t.Setenv("STEMMA_TEST_JAMF_CLIENT_ID", "test-client-id")
	t.Setenv("STEMMA_TEST_JAMF_CLIENT_SECRET", "test-client-secret")
	fake := &fakeServer{t: t, packages: make(map[string]map[string]json.RawMessage), version: 1, tokenLifetime: 1800}
	server := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(server.Close)
	request := plugin.Request{Protocol: plugin.ProtocolVersion, Method: "apply", Identity: plugin.Identity{Project: "school", Recipe: "vendor", Destination: "jamf"}, Config: raw(configuration{URL: server.URL, ClientIDEnv: "STEMMA_TEST_JAMF_CLIENT_ID", ClientSecretEnv: "STEMMA_TEST_JAMF_CLIENT_SECRET"}), Metadata: raw(map[string]any{}), Artifact: fixtureArtifact(t, "immutable package bytes")}
	return fake, request
}

func fixtureArtifact(t *testing.T, content string) plugin.Artifact {
	t.Helper()
	file := filepath.Join(t.TempDir(), "vendor.pkg")
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	return plugin.Artifact{Path: file, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), Filename: "vendor.pkg"}
}

func (s *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/api/v1/oauth/token" && r.Method == http.MethodPost {
		if err := r.ParseForm(); err != nil {
			s.t.Error(err)
		}
		if r.Form.Get("client_id") != "test-client-id" || r.Form.Get("client_secret") != "test-client-secret" || r.Form.Get("grant_type") != "client_credentials" {
			s.t.Error("incorrect OAuth2 client credential request")
		}
		s.tokens++
		writeJSON(s.t, w, map[string]any{"access_token": "test-token-" + strconv.Itoa(s.tokens), "expires_in": s.tokenLifetime})
		return
	}
	if r.Header.Get("Authorization") != "Bearer test-token-"+strconv.Itoa(s.tokens) {
		s.t.Error("missing bearer token")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.URL.Path == packagePath {
		switch r.Method {
		case http.MethodGet:
			if s.failReads > 0 {
				s.failReads--
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if !strings.HasPrefix(r.URL.Query().Get("filter"), `fileName=="stemma-`) {
				s.t.Error("discovery did not use deterministic filename identity")
			}
			var results []map[string]json.RawMessage
			for _, pkg := range s.packages {
				if !s.hideDiscovery {
					results = append(results, pkg)
				}
			}
			writeJSON(s.t, w, map[string]any{"totalCount": len(results), "results": results})
			return
		case http.MethodPost:
			fields := s.readObject(r)
			id := strconv.Itoa(len(s.packages) + 1)
			fields["id"] = raw(id)
			fields["cloudTransferStatus"] = raw("AWAITING_UPLOAD")
			s.packages[id] = fields
			if s.failAfter == "create" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusCreated)
			writeJSON(s.t, w, map[string]any{"id": id, "href": "ignored-not-followed"})
			return
		}
	}
	if !strings.HasPrefix(r.URL.Path, packagePath+"/") {
		s.t.Errorf("unexpected endpoint (package-only boundary violated): %s", r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	path := strings.Split(strings.TrimPrefix(r.URL.Path, packagePath+"/"), "/")
	id := path[0]
	pkg := s.packages[id]
	if pkg == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if len(path) == 2 && path[1] == "upload" && r.Method == http.MethodPost {
		reader, err := r.MultipartReader()
		if err != nil {
			s.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		part, err := reader.NextPart()
		if err != nil {
			s.t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if part.FormName() != "file" || part.FileName() != stringField(pkg, "fileName") {
			s.t.Error("incorrect upload form field or filename")
		}
		data, err := io.ReadAll(part)
		if err != nil {
			s.t.Error(err)
		}
		if s.failBeforeUpload {
			pkg["cloudTransferStatus"] = raw("FAILED")
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(s.t, w, map[string]any{"error": "test-client-secret"})
			return
		}
		digest, digest3 := sha256.Sum256(data), sha3.Sum512(data)
		pkg["sha256"], pkg["sha3512"] = raw(hex.EncodeToString(digest[:])), raw(hex.EncodeToString(digest3[:]))
		pkg["hashType"], pkg["hashValue"] = raw("SHA3_512"), pkg["sha3512"]
		pkg["cloudTransferStatus"] = raw("READY")
		if s.failAfter == "upload" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(s.t, w, map[string]any{"id": id})
		return
	}
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("ETag", strconv.Quote(strconv.Itoa(s.version)))
		writeJSON(s.t, w, pkg)
	case http.MethodPut:
		if r.Header.Get("If-Match") != strconv.Quote(strconv.Itoa(s.version)) {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		fields := s.readObject(r)
		for _, key := range readOnlyFields {
			if _, exists := fields[key]; exists {
				s.t.Errorf("PUT included read-only field %s", key)
			}
			if value, exists := pkg[key]; exists {
				fields[key] = value
			}
		}
		s.packages[id] = fields
		s.version++
		if s.failAfter == "metadata" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		writeJSON(s.t, w, fields)
	default:
		s.t.Errorf("unexpected package method %s", r.Method)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *fakeServer) readObject(r *http.Request) map[string]json.RawMessage {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		s.t.Error(err)
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		s.t.Error("metadata request must be JSON")
	}
	fields, err := decodeObject(data)
	if err != nil {
		s.t.Error(err)
	}
	return fields
}

func (s *fakeServer) count(operation string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, request := range s.requests {
		if request == operation {
			count++
		}
	}
	return count
}
func (s *fakeServer) operations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}
func (s *fakeServer) set(key string, value json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.packages["1"][key] = value
}
func (s *fakeServer) record() map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := decodeObject(raw(s.packages["1"]))
	if err != nil {
		s.t.Fatal(err)
	}
	return record
}
func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}
