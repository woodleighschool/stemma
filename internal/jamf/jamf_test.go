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

// fixtureMarker is the marker of the fixture identity. Packages already in Jamf
// carry this exact line, so it changes only with a deliberate format change.
const fixtureMarker = "[stemma:v1 id=9e24b0af0c9659ee7c0bad65847e693691a7e9f66341fa1fe0a0d7d89b91767e]"

func TestPublicationCreatesMarkedPackageAndConvergesWithoutState(t *testing.T) {
	server, request := newFixture(t)
	request.Metadata = raw(map[string]any{"packageName": "Managed package", "info": "Initial info", "rebootRequired": true})
	request.Method = "plan"
	plan, err := Handle(t.Context(), request)
	if err != nil || len(plan.Changes) != 4 {
		t.Fatalf("first plan: %+v: %v", plan, err)
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("plan wrote to Jamf: %v", writes)
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
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
	current := server.record("1")
	if stringField(current, "fileName") != "vendor.pkg" || stringField(current, "notes") != fixtureMarker || stringField(current, "packageName") != "Managed package" {
		t.Fatalf("package is not natively named and marked: %s", raw(current))
	}
	writes := len(server.writes())
	for _, method := range []string{"plan", "apply"} {
		request.Method = method
		response, err := Handle(t.Context(), request)
		if err != nil || len(response.Changes) != 0 {
			t.Fatalf("unchanged %s: %+v: %v", method, response, err)
		}
	}
	if len(server.writes()) != writes {
		t.Fatalf("unchanged run wrote to Jamf: %v", server.writes()[writes:])
	}
	request.Metadata = raw(map[string]any{"packageName": "Renamed", "info": nil, "rebootRequired": false})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath+"/1/upload") != 1 {
		t.Fatal("metadata-only change uploaded content")
	}
	current = server.record("1")
	if stringField(current, "packageName") != "Renamed" || string(current["info"]) != "null" || string(current["rebootRequired"]) != "false" {
		t.Fatalf("presence ownership failed: %s", raw(current))
	}
	server.set("info", raw("now owned remotely"))
	request.Metadata = raw(map[string]any{"packageName": "Renamed"})
	response, err := Handle(t.Context(), request)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("omitted fields should be left unchanged: %+v: %v", response, err)
	}
	if stringField(server.record("1"), "info") != "now owned remotely" {
		t.Fatal("omission restored an old managed value")
	}
}

func TestPackageNameDefaultsToArtifactFileName(t *testing.T) {
	server, request := newFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if got := stringField(server.record("1"), "packageName"); got != "vendor.pkg" {
		t.Fatalf("default package name: %q", got)
	}
}

func TestNotesKeepMarkerBelowExplicitOrRemoteText(t *testing.T) {
	server, request := newFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	server.set("notes", raw("Operator note\n"+fixtureMarker))
	response, err := Handle(t.Context(), request)
	if err != nil || len(response.Changes) != 0 {
		t.Fatalf("omitted notes were rewritten: %+v: %v", response, err)
	}
	for _, step := range []struct {
		name   string
		remote string
		notes  any
		want   string
	}{
		{name: "marker moved above operator text", remote: fixtureMarker + "\nOperator note", want: "Operator note\n" + fixtureMarker},
		{name: "declared text replaces remote text", notes: "Managed note", want: "Managed note\n" + fixtureMarker},
		{name: "declared empty text", notes: "", want: fixtureMarker},
		{name: "operator text under omitted notes", remote: "Restored by an operator\n" + fixtureMarker, want: "Restored by an operator\n" + fixtureMarker},
		{name: "null clears the text", notes: nil, want: fixtureMarker},
	} {
		t.Run(step.name, func(t *testing.T) {
			if step.remote != "" {
				server.set("notes", raw(step.remote))
				request.Metadata = raw(map[string]any{})
			} else {
				request.Metadata = raw(map[string]any{"notes": step.notes})
			}
			if _, err := Handle(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if got := stringField(server.record("1"), "notes"); got != step.want {
				t.Fatalf("notes = %q, want %q", got, step.want)
			}
			if response, err := Handle(t.Context(), request); err != nil || len(response.Changes) != 0 {
				t.Fatalf("notes did not settle: %+v: %v", response, err)
			}
		})
	}
	if server.count("POST "+packagePath) != 1 {
		t.Fatal("a notes edit lost the package's identity")
	}
}

func TestChangedBytesUnderSameFileNameUploadIntoSameRecord(t *testing.T) {
	server, request := newFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.Artifact = fixtureArtifact(t, "rebuilt package bytes")
	request.Method = "plan"
	writes := len(server.writes())
	plan, err := Handle(t.Context(), request)
	if err != nil || len(plan.Changes) != 1 || plan.Changes[0].Kind != "content" || plan.Changes[0].Action != "upload" {
		t.Fatalf("drift plan: %+v: %v", plan, err)
	}
	if len(server.writes()) != writes {
		t.Fatal("plan wrote to Jamf")
	}
	request.Method = "apply"
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 1 || server.count("POST "+packagePath+"/1/upload") != 2 {
		t.Fatalf("drift was not uploaded into the same record: %v", server.writes())
	}
	if stringField(server.record("1"), "sha256") != request.Artifact.SHA256 {
		t.Fatal("record does not hold the rebuilt bytes")
	}
}

func TestOmittedNotesPreserveEditsDuringUpload(t *testing.T) {
	server, request := newFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	server.set("notes", raw("Previous note\n"+fixtureMarker))
	notes := "Edited during upload\n" + fixtureMarker
	server.mu.Lock()
	server.notesAfterUpload = notes
	server.mu.Unlock()
	request.Artifact = fixtureArtifact(t, "updated package bytes")
	request.Metadata = raw(map[string]any{"info": "New package information"})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if got := stringField(server.record("1"), "notes"); got != notes {
		t.Fatalf("omitted notes overwritten: %q", got)
	}
}

func TestNewFileNameCreatesAnotherFamilyRecord(t *testing.T) {
	server, request := newFixture(t)
	first := request.Artifact
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	request.Artifact = fixtureArtifactNamed(t, "vendor-2.0.pkg", "second package bytes")
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 2 || server.count("POST "+packagePath+"/1/upload") != 1 || server.count("POST "+packagePath+"/2/upload") != 1 {
		t.Fatalf("new file name did not publish into its own record: %v", server.writes())
	}
	second := server.record("2")
	if stringField(second, "fileName") != "vendor-2.0.pkg" || stringField(second, "notes") != fixtureMarker {
		t.Fatalf("second record is not natively named and marked: %s", raw(second))
	}
	if stringField(server.record("1"), "sha256") != first.SHA256 {
		t.Fatal("earlier record's bytes changed")
	}
	request.Artifact = first
	writes := len(server.writes())
	if response, err := Handle(t.Context(), request); err != nil || len(response.Changes) != 0 {
		t.Fatalf("returning to the earlier file name: %+v: %v", response, err)
	}
	if len(server.writes()) != writes {
		t.Fatal("returning to the earlier file name replayed a publication")
	}
}

func TestFileNameHeldOutsideFamilyRequiresAdoption(t *testing.T) {
	for name, notes := range map[string]any{
		"no notes":              nil,
		"administrator notes":   "Uploaded by an administrator",
		"marker within a line":  "copied from " + fixtureMarker + " by an operator",
		"other software marker": "[stemma:v1 id=" + strings.Repeat("0", 64) + "]",
	} {
		t.Run(name, func(t *testing.T) {
			server, request := newFixture(t)
			fields := contentFields("administrator bytes")
			fields["fileName"], fields["packageName"], fields["notes"] = "vendor.pkg", "Vendor", notes
			server.seed(fields)
			for _, method := range []string{"plan", "apply"} {
				request.Method = method
				if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "set package_id to 1") {
					t.Fatalf("%s took over a package outside the family: %v", method, err)
				}
			}
			if writes := server.writes(); len(writes) != 0 {
				t.Fatalf("refused publication wrote to Jamf: %v", writes)
			}
		})
	}
}

func TestPackageIDAdoptsMarksAndConverges(t *testing.T) {
	server, request := newFixture(t)
	fields := contentFields("administrator bytes")
	fields["fileName"], fields["packageName"], fields["notes"] = "vendor-original.pkg", "Vendor", "Uploaded by an administrator"
	remote := map[string]any{"label": "must not be erased", "enabled": false, "nested": map[string]any{"notes": nil}, "values": []any{"one", "two"}}
	fields["newServerField"] = remote
	server.seed(fields)
	request.Metadata = raw(map[string]any{"package_id": "1", "packageName": "Adopted", "priority": 0})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 0 || server.count("POST "+packagePath+"/1/upload") != 1 {
		t.Fatalf("adoption did not converge the pinned record: %v", server.writes())
	}
	steps := server.operations()
	if slices.Index(steps, "PUT "+packagePath+"/1") > slices.Index(steps, "POST "+packagePath+"/1/upload") {
		t.Fatalf("adopted package was not marked before its content: %v", steps)
	}
	current := server.record("1")
	if stringField(current, "fileName") != "vendor.pkg" || stringField(current, "notes") != "Uploaded by an administrator\n"+fixtureMarker || stringField(current, "sha256") != request.Artifact.SHA256 {
		t.Fatalf("adopted package did not converge: %s", raw(current))
	}
	if stringField(current, "packageName") != "Adopted" || string(current["priority"]) != "0" || !equalJSON(current["newServerField"], raw(remote)) {
		t.Fatalf("native values or unknown remote fields changed: %s", raw(current))
	}
	request.Metadata = raw(map[string]any{"packageName": "Adopted"})
	writes := len(server.writes())
	if response, err := Handle(t.Context(), request); err != nil || len(response.Changes) != 0 {
		t.Fatalf("adopted package left the family: %+v: %v", response, err)
	}
	if len(server.writes()) != writes {
		t.Fatal("rediscovering the adopted package wrote to Jamf")
	}
}

func TestPackageIDAdoptionWithMatchingBytesDoesNotUpload(t *testing.T) {
	server, request := newFixture(t)
	fields := contentFields("immutable package bytes")
	fields["fileName"], fields["packageName"] = "vendor-original.pkg", "Vendor"
	server.seed(fields)
	request.Metadata = raw(map[string]any{"package_id": "1"})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath+"/1/upload") != 0 {
		t.Fatal("adoption replaced matching content")
	}
	if current := server.record("1"); stringField(current, "fileName") != "vendor.pkg" || stringField(current, "notes") != fixtureMarker {
		t.Fatalf("adopted package was not renamed and marked: %s", raw(current))
	}
}

func TestPackageIDRefusals(t *testing.T) {
	for name, test := range map[string]struct {
		seed []map[string]any
		want string
	}{
		"missing package":      {want: "does not exist"},
		"other software":       {seed: []map[string]any{{"fileName": "vendor.pkg", "notes": "[stemma:v1 id=" + strings.Repeat("0", 64) + "]"}}, want: "another Stemma identity"},
		"file name held":       {seed: []map[string]any{{"fileName": "vendor-original.pkg"}, {"fileName": "vendor.pkg"}}, want: "set package_id to 2"},
		"file name held by us": {seed: []map[string]any{{"fileName": "vendor-original.pkg"}, {"fileName": "vendor.pkg", "notes": fixtureMarker}}, want: "set package_id to 2"},
	} {
		t.Run(name, func(t *testing.T) {
			server, request := newFixture(t)
			for _, fields := range test.seed {
				server.seed(fields)
			}
			request.Metadata = raw(map[string]any{"package_id": "1"})
			if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("adoption error = %v, want %q", err, test.want)
			}
			if writes := server.writes(); len(writes) != 0 {
				t.Fatalf("refused adoption wrote to Jamf: %v", writes)
			}
		})
	}
}

func TestDuplicateFamilyRecordsAreAmbiguous(t *testing.T) {
	server, request := newFixture(t)
	for range 2 {
		server.seed(map[string]any{"fileName": "vendor.pkg", "notes": fixtureMarker})
	}
	if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "multiple Jamf packages match") {
		t.Fatalf("duplicate records were not reported: %v", err)
	}
	if writes := server.writes(); len(writes) != 0 {
		t.Fatalf("ambiguous publication wrote to Jamf: %v", writes)
	}
	request.Metadata = raw(map[string]any{"package_id": "2"})
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatalf("package_id did not select a duplicate: %v", err)
	}
	if server.count("POST "+packagePath) != 0 || server.count("POST "+packagePath+"/2/upload") != 1 {
		t.Fatalf("selected duplicate was not converged: %v", server.writes())
	}
}

func TestLostResponsesConvergeFromJamfAlone(t *testing.T) {
	for phase, interrupted := range map[string]bool{"create": false, "upload": true, "metadata": false} {
		t.Run(phase, func(t *testing.T) {
			server, request := newFixture(t)
			server.failAfter = phase
			request.Metadata = raw(map[string]any{"packageName": "Managed"})
			if _, err := Handle(t.Context(), request); (err != nil) != interrupted {
				t.Fatalf("committed %s: %v", phase, err)
			}
			server.failAfter = ""
			if _, err := Handle(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if server.count("POST "+packagePath) != 1 || server.count("POST "+packagePath+"/1/upload") != 1 || server.count("PUT "+packagePath+"/1") != 1 {
				t.Fatalf("committed %s was replayed: %v", phase, server.writes())
			}
			if current := server.record("1"); stringField(current, "packageName") != "Managed" || stringField(current, "sha256") != request.Artifact.SHA256 {
				t.Fatalf("publication did not converge: %s", raw(current))
			}
		})
	}
}

func TestUnfinishedContentIsUploadedAgain(t *testing.T) {
	for _, status := range []string{"AWAITING_UPLOAD", "PENDING", "FAILED"} {
		t.Run(status, func(t *testing.T) {
			server, request := newFixture(t)
			server.seed(map[string]any{"fileName": "vendor.pkg", "packageName": "vendor.pkg", "notes": fixtureMarker, "cloudTransferStatus": status})
			request.Method = "plan"
			plan, err := Handle(t.Context(), request)
			if err != nil || len(plan.Changes) != 1 || plan.Changes[0].Action != "upload" {
				t.Fatalf("unfinished content plan: %+v: %v", plan, err)
			}
			request.Method = "apply"
			if _, err := Handle(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if server.count("POST "+packagePath) != 0 || server.count("POST "+packagePath+"/1/upload") != 1 {
				t.Fatalf("interrupted record was not finished in place: %v", server.writes())
			}
		})
	}
}

func TestFailedUploadLeavesRecordForNextRun(t *testing.T) {
	server, request := newFixture(t)
	server.failUpload = true
	_, err := Handle(t.Context(), request)
	if err == nil {
		t.Fatal("failed upload was published")
	}
	if strings.Contains(err.Error(), "test-client-secret") {
		t.Fatal("credential leaked in HTTP error")
	}
	if server.count("DELETE "+packagePath+"/1") != 0 || stringField(server.record("1"), "notes") != fixtureMarker {
		t.Fatal("failed upload did not leave a marked record")
	}
	server.failUpload = false
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("POST "+packagePath) != 1 || server.count("POST "+packagePath+"/1/upload") != 2 {
		t.Fatalf("retry did not upload into the same record: %v", server.writes())
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
	control, request := newFixture(t)
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	server, request := newFixture(t)
	server.failReads = 1
	if _, err := Handle(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if server.count("GET "+packagePath) != control.count("GET "+packagePath)+1 || server.count("POST "+packagePath) != 1 {
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
	request.Config = raw(configuration{URL: server.URL, ClientID: "test-client-id", ClientSecret: "test-client-secret"})
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
	request.Config = raw(configuration{URL: server.URL, ClientID: "test-client-id", ClientSecret: "test-client-secret"})
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
	for _, metadata := range []string{`{"priority":null}`, `{"packageName":null}`, `{"policies":[]}`, `{"fileName":"overridden.pkg"}`, `{"sha256":"untrusted"}`, `{"priority":"10"}`, `{"notes":1}`, `{"package_id":"../1"}`, `{"notes":"a","notes":"b"}`, `{"notes":"[stemma:v1 id=forged]"}`} {
		request.Metadata = json.RawMessage(metadata)
		if _, err := Handle(t.Context(), request); err == nil {
			t.Fatalf("invalid metadata accepted: %s", metadata)
		}
	}
	request.Metadata = raw(map[string]any{})
	request.Config = raw(map[string]any{"url": "https://example.com", "client_id": "ID", "client_secret": "SECRET", "package_id": "1"})
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("accepted software adoption in shared destination config")
	}
}

func TestArtifactDescriptorMismatchBlocksWrites(t *testing.T) {
	server, request := newFixture(t)
	request.Artifact.SHA256 = strings.Repeat("0", 64)
	if _, err := Handle(t.Context(), request); err == nil {
		t.Fatal("accepted bytes with wrong immutable descriptor")
	}
	if len(server.operations()) != 0 {
		t.Fatalf("contacted Jamf before validating the artifact: %v", server.operations())
	}
}

func TestApplicationDiskImageIsNotAPackage(t *testing.T) {
	server, request := newFixture(t)
	request.Artifact.Filename, request.Artifact.Format = "vendor-1.0.dmg", "dmg"
	for _, method := range []string{"validate", "apply"} {
		request.Method = method
		if _, err := Handle(t.Context(), request); err == nil || !strings.Contains(err.Error(), "PKG") {
			t.Fatalf("%s accepted a disk image: %v", method, err)
		}
	}
	if len(server.operations()) != 0 {
		t.Fatalf("disk image reached Jamf: %v", server.operations())
	}
}

func TestPublicationHonorsCancellation(t *testing.T) {
	_, request := newFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Handle(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled publication: %v", err)
	}
}

type fakeServer struct {
	t                *testing.T
	mu               sync.Mutex
	packages         map[string]map[string]json.RawMessage
	nextPackage      int
	native           *nativeServer
	requests         []string
	version          int
	failAfter        string
	failUpload       bool
	tokenLifetime    int
	tokens           int
	failReads        int
	notesAfterUpload string
}

func newFixture(t *testing.T) (*fakeServer, plugin.ReconcileRequest) {
	t.Helper()
	fake := &fakeServer{t: t, packages: make(map[string]map[string]json.RawMessage), version: 1, tokenLifetime: 1800}
	server := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(server.Close)
	request := plugin.ReconcileRequest{Method: "apply", Identity: plugin.Identity{Project: "school", Resource: plugin.ResourceReference{Kind: "MacSoftware", Name: "vendor"}, Destination: "jamf"}, Config: raw(configuration{URL: server.URL, ClientID: "test-client-id", ClientSecret: "test-client-secret"}), Metadata: raw(map[string]any{}), Artifact: fixtureArtifact(t, "immutable package bytes")}
	return fake, request
}

func fixtureArtifact(t *testing.T, content string) plugin.Artifact {
	t.Helper()
	return fixtureArtifactNamed(t, "vendor.pkg", content)
}

// fixtureArtifactNamed materializes content away from its artifact filename, as
// a leased workspace may.
func fixtureArtifactNamed(t *testing.T, filename, content string) plugin.Artifact {
	t.Helper()
	file := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(content))
	return plugin.Artifact{Path: file, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(content)), Filename: filename}
}

// contentFields are the fields Jamf reports once it has processed content.
func contentFields(content string) map[string]any {
	digest, digest3 := sha256.Sum256([]byte(content)), sha3.Sum512([]byte(content))
	return map[string]any{"sha256": hex.EncodeToString(digest[:]), "sha3512": hex.EncodeToString(digest3[:]), "hashType": "SHA3_512", "hashValue": hex.EncodeToString(digest3[:]), "cloudTransferStatus": "READY"}
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
	if s.native != nil && s.native.handle(s, w, r) {
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
			results := []map[string]json.RawMessage{}
			for id := 1; id <= s.nextPackage; id++ {
				if pkg := s.packages[strconv.Itoa(id)]; pkg != nil && s.matches(pkg, r.URL.Query().Get("filter")) {
					results = append(results, pkg)
				}
			}
			writeJSON(s.t, w, map[string]any{"totalCount": len(results), "results": results})
			return
		case http.MethodPost:
			fields := s.readObject(r)
			if !strings.HasPrefix(stringField(fields, "notes"), "[stemma:v1 id=") {
				s.t.Error("created a package the next run could not find")
			}
			s.nextPackage++
			id := strconv.Itoa(s.nextPackage)
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
		if s.failUpload {
			pkg["cloudTransferStatus"] = raw("FAILED")
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(s.t, w, map[string]any{"error": "test-client-secret"})
			return
		}
		for key, value := range contentFields(string(data)) {
			pkg[key] = raw(value)
		}
		if s.notesAfterUpload != "" {
			pkg["notes"] = raw(s.notesAfterUpload)
		}
		if s.failAfter == "upload" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(s.t, w, map[string]any{"id": id})
		return
	}
	switch r.Method {
	case http.MethodDelete:
		delete(s.packages, id)
		w.WriteHeader(http.StatusNoContent)
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

// matches applies the two RSQL forms the provider sends. Like Jamf, the notes
// form finds its text anywhere, which is looser than a marker line.
func (s *fakeServer) matches(pkg map[string]json.RawMessage, filter string) bool {
	field, value, _ := strings.Cut(filter, "==")
	value = strings.TrimSuffix(strings.TrimPrefix(value, `"`), `"`)
	switch {
	case field == "fileName":
		return stringField(pkg, "fileName") == value
	case field == "notes" && len(value) > 2 && strings.HasPrefix(value, "*") && strings.HasSuffix(value, "*"):
		return strings.Contains(stringField(pkg, "notes"), strings.Trim(value, "*"))
	}
	s.t.Errorf("unexpected package filter %q", filter)
	return false
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

// seed stores a package that exists before the run, READY unless fields say otherwise.
func (s *fakeServer) seed(fields map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextPackage++
	id := strconv.Itoa(s.nextPackage)
	pkg := map[string]json.RawMessage{"id": raw(id), "cloudTransferStatus": raw("READY")}
	for key, value := range fields {
		pkg[key] = raw(value)
	}
	s.packages[id] = pkg
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

// writes lists the requests that could change Jamf.
func (s *fakeServer) writes() []string {
	return slices.DeleteFunc(s.operations(), func(operation string) bool {
		return strings.HasPrefix(operation, "GET ") || operation == "POST /api/v1/oauth/token"
	})
}
func (s *fakeServer) exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.packages[id] != nil
}
func (s *fakeServer) set(key string, value json.RawMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.packages["1"][key] = value
}
func (s *fakeServer) record(id string) map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, err := decodeObject(raw(s.packages[id]))
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
