package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestMetadataResolutionAndLockedAcquisition(t *testing.T) {
	payload := "reviewed installer"
	sum := sha256.Sum256([]byte(payload))
	digest := hex.EncodeToString(sum[:])
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = w.Write([]byte(payload))
	}))
	defer server.Close()
	m := manager(t)
	evidence := json.RawMessage(`{"silent":"/S"}`)
	filename := "first.exe"
	version, root := "1", "release/1"
	acquire := func(context.Context, plugin.Input, json.RawMessage) (Acquisition, error) {
		return Acquisition{Download: &Download{URL: server.URL + "/installer", Filename: filename, SHA256: digest}}, nil
	}
	m.resolvers["registry"] = Resolver{Fingerprint: func(input plugin.Input) (string, error) { return fingerprint(input.Config) }, Version: "1", Acquire: acquire, Discover: func(context.Context, plugin.Input) (Discovery, error) {
		return Discovery{Version: version, ContentRoot: root, Observation: json.RawMessage(`{"version":"1"}`), Content: &Content{SHA256: digest, Filename: filename, Mode: 0o644}, Evidence: map[string]json.RawMessage{"registry.installer": evidence}}, nil
	}}
	input := plugin.Input{Resolver: "registry"}
	first, err := m.Resolve(t.Context(), input)
	if err != nil || requests != 0 {
		t.Fatalf("metadata-only resolve: %v, requests %d", err, requests)
	}
	evidence = json.RawMessage(`{"silent":"/quiet"}`)
	filename = "second.exe"
	version, root = "2", "release/2"
	second, _, err := m.Refresh(t.Context(), input, first)
	if first.InputVersion != "1" || first.ContentRoot != "release/1" || second.InputVersion != "2" || second.ContentRoot != "release/2" {
		t.Fatal("resolved input version or root did not follow the selected observation")
	}
	if err != nil || requests != 0 || first.Equal(second) || string(second.Evidence["registry.installer"]) != string(evidence) || second.Content.Filename != filename {
		t.Fatalf("metadata refresh: %+v %v, requests %d", second, err, requests)
	}
	encoded, _ := json.Marshal(second)
	if strings.Contains(string(encoded), `"size"`) {
		t.Fatalf("lock contains measured size: %s", encoded)
	}
	m.resolvers["registry"] = Resolver{Fingerprint: func(input plugin.Input) (string, error) { return fingerprint(input.Config) }, Version: "1", Acquire: acquire, Discover: func(context.Context, plugin.Input) (Discovery, error) {
		t.Fatal("locked acquisition rediscovered")
		return Discovery{}, nil
	}}
	if _, err := m.FetchLocked(t.Context(), input, second); err != nil || requests != 1 {
		t.Fatalf("cold consumption: %v requests %d", err, requests)
	}
	ref, err := m.Store.Lookup(digest)
	if err != nil || ref.Size != int64(len(payload)) {
		t.Fatalf("measured cache object: %+v %v", ref, err)
	}
	third, err := m.Resolve(t.Context(), plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/other.exe", "filename": "other.exe", "sha256": digest}})
	if err != nil || third.Content.Filename != "other.exe" || requests != 1 {
		t.Fatalf("same bytes borrowed representation: %+v %v", third, err)
	}
}

func TestResolvedLockDoesNotCertifyAvailabilityOrContent(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		body   string
	}{
		{"different bytes", http.StatusOK, "replacement"},
		{"unavailable", http.StatusNotFound, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			m := manager(t)
			sum := sha256.Sum256([]byte("expected"))
			input := plugin.Input{Resolver: "http", Config: map[string]any{"url": server.URL + "/app.pkg", "filename": "app.pkg", "sha256": hex.EncodeToString(sum[:])}}
			entry, err := m.Resolve(t.Context(), input)
			if err != nil || requests != 0 {
				t.Fatalf("resolution: %v requests=%d", err, requests)
			}
			m.Offline = true
			if _, err = m.FetchLocked(t.Context(), input, entry); err == nil || requests != 0 {
				t.Fatalf("offline missing content: %v", err)
			}
			m.Offline = false
			if _, err = m.FetchLocked(t.Context(), input, entry); err == nil || requests != 1 {
				t.Fatalf("locked consumption accepted missing/different bytes: %v", err)
			}
		})
	}
}
