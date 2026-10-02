package source

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestAcquisitionIsTransientAndRecreatedForLockedFetch(t *testing.T) {
	body := "installer"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
	secret := "first-secret"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("signature") != secret || r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("runtime credentials not supplied")
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	m := manager(t)
	resolver := Resolver{
		Version: "1", Fingerprint: func(plugin.Input) (string, error) { return fingerprint("stable-declaration") },
		Discover: func(context.Context, plugin.Input) (Discovery, error) {
			return Discovery{Observation: json.RawMessage(`{"release":"1"}`), Content: &Content{SHA256: digest, Filename: "app.pkg", Mode: 0o644}}, nil
		},
		Acquire: func(context.Context, plugin.Input, json.RawMessage) (Acquisition, error) {
			return Acquisition{Download: &Download{URL: server.URL + "/app?signature=" + secret, Headers: map[string]string{"Authorization": "Bearer " + secret}, Filename: "app.pkg", SHA256: digest}}, nil
		},
	}
	if err := m.Register("vendor", resolver); err != nil {
		t.Fatal(err)
	}
	input := plugin.Input{Resolver: "vendor"}
	entry, err := m.Resolve(t.Context(), input)
	if err != nil || requests != 0 {
		t.Fatalf("discovery: %v requests=%d", err, requests)
	}
	encoded, _ := json.Marshal(entry)
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "download") {
		t.Fatalf("transient acquisition persisted: %s", encoded)
	}
	secret = "rotated-secret"
	resolver.Discover = func(context.Context, plugin.Input) (Discovery, error) {
		t.Fatal("locked fetch rediscovered")
		return Discovery{}, nil
	}
	cold := New(m.Store, m.Root, false)
	if err := cold.Register("vendor", resolver); err != nil {
		t.Fatal(err)
	}
	if _, err := cold.FetchLocked(t.Context(), input, entry); err != nil || requests != 1 {
		t.Fatalf("cold replay: %v requests=%d", err, requests)
	}
}

func TestAcquisitionRejectsAmbiguousResults(t *testing.T) {
	for _, request := range []Acquisition{{}, {Download: &Download{URL: "https://example.test/app.pkg"}, Artifact: &plugin.Artifact{Path: "unexpected"}}} {
		m := manager(t)
		resolver := Resolver{Acquire: func(context.Context, plugin.Input, json.RawMessage) (Acquisition, error) { return request, nil }}
		if _, _, err := m.acquire(t.Context(), resolver, plugin.Input{}, Entry{}, nil); err == nil {
			t.Fatal("accepted ambiguous acquisition")
		}
	}
}

func TestDownloadRejectsHTTPSDowngrade(t *testing.T) {
	m := manager(t)
	requests := 0
	m.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://example.test/app.pkg"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	_, _, err := m.download(t.Context(), Download{URL: "https://example.test/app.pkg"}, nil)
	if err == nil || !strings.Contains(err.Error(), "downgrade") || requests != 1 {
		t.Fatalf("downgrade accepted: %v requests=%d", err, requests)
	}
}

func TestHomebrewReplayRejectsDifferentBottle(t *testing.T) {
	m := manager(t)
	digest := strings.Repeat("a", 64)
	observed := homebrewObservation{URL: "https://ghcr.io/v2/homebrew/core/other/blobs/sha256:" + digest, Filename: "app.tar.gz", SHA256: digest, Bottle: true}
	data, _ := json.Marshal(observed)
	if _, err := m.acquireHomebrew(t.Context(), homebrewConfig{Formula: "app"}, data); err == nil {
		t.Fatal("accepted another formula's bottle")
	}
}
