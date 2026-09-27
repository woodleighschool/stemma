package source

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

func TestEntryEvidenceRoundTripAndEquality(t *testing.T) {
	entry := Entry{
		Version: 1, Resolver: "vendor.release", ResolverVersion: "1", Declaration: strings.Repeat("a", 64),
		Observation: json.RawMessage(`{}`),
		Content:     Content{Artifact: cas.Ref{SHA256: strings.Repeat("b", 64), Size: 1}, Filename: "input.pkg", Mode: 0o644},
		Evidence:    map[string]json.RawMessage{"vendor.release": json.RawMessage(`{ "version": "1.2", "id": 9007199254740993, "enabled": false }`)},
	}
	data, err := yaml.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Entry
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	if got := string(decoded.Evidence["vendor.release"]); got != `{"enabled":false,"id":9007199254740993,"version":"1.2"}` || !decoded.Equal(entry) {
		t.Fatalf("evidence changed across YAML: %s", got)
	}
	decoded.Evidence["vendor.release"] = json.RawMessage(`{"enabled":false,"id":9007199254740992,"version":"1.2"}`)
	if decoded.Equal(entry) {
		t.Fatal("evidence changes did not change lock equality")
	}
	decoded.Evidence["vendor.release"] = json.RawMessage(`{`)
	if err := decoded.Validate(); err == nil || decoded.Equal(entry) {
		t.Fatal("invalid evidence was accepted in a lock entry")
	}
	entry.Evidence = nil
	decoded.Evidence = map[string]json.RawMessage{}
	if !decoded.Equal(entry) {
		t.Fatal("empty and omitted evidence differ")
	}
	if err := yaml.Unmarshal([]byte("observation: {}\nevidence: {}\n"), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Evidence != nil {
		t.Fatal("empty lock evidence was not normalized")
	}
}

func TestResolveRejectsInvalidEvidence(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(store, t.TempDir(), false)
	filename := filepath.Join(t.TempDir(), "input.pkg")
	if err := os.WriteFile(filename, []byte("installer"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, value string }{{"empty", ""}, {"malformed", "{"}, {"trailing", "{} {}"}} {
		t.Run(test.name, func(t *testing.T) {
			evidence := map[string]json.RawMessage{"vendor.release": json.RawMessage(test.value)}
			m.Resolvers["vendor.release"] = Resolver{
				Version: "1",
				Discover: func(context.Context, plugin.Input) (Discovery, error) {
					return Discovery{Observation: json.RawMessage(`{}`)}, nil
				},
				Fetch: func(context.Context, plugin.Input, json.RawMessage) (plugin.Artifact, error) {
					return plugin.Artifact{Path: filename, Filename: "input.pkg", Evidence: evidence}, nil
				},
			}
			if _, err := m.Resolve(t.Context(), plugin.Input{Resolver: "vendor.release"}); err == nil || !strings.Contains(err.Error(), `resolver evidence "vendor.release"`) {
				t.Fatalf("invalid resolver evidence accepted: %v", err)
			}
		})
	}
}
