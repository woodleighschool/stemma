package engine

import (
	"context"
	"encoding/json"
	"github.com/woodleighschool/stemma/internal/testproject"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestSecretRotationPreservesDestinationBinding(t *testing.T) {
	for _, kind := range []string{"jamf", "intune"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			endpointField, secretField := "url", "client_secret"
			credentials, metadata := "        client_id: test-client\n", "{}"
			if kind == "intune" {
				endpointField, secretField = "graph_url", "token"
				credentials, metadata = "", "{'@odata.type': '#microsoft.graph.win32LobApp'}"
			}
			manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: rotation}
spec:
  imports: ['*.software.yaml']
  destinations:
    remote:
      operation: ` + kind + `
      config:
        ` + endpointField + `: ${STEMMA_TEST_ENDPOINT}
        ` + secretField + `: ${STEMMA_TEST_SECRET}
` + credentials + `---
apiVersion: stemma/v1alpha1
kind: Software
metadata: {name: app}
spec:
  source: {type: file, path: installer.bin}
  destinations: {remote: ` + metadata + `}
`
			configPath := filepath.Join(root, "stemma.yaml")
			if err := testproject.Write(configPath, []byte(manifest)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "installer.bin"), []byte("installer"), 0o600); err != nil {
				t.Fatal(err)
			}
			options := Options{ConfigPath: configPath, CacheDir: t.TempDir(), StateDir: t.TempDir(), Method: "apply"}
			for i, step := range []struct{ endpoint, secret, binding string }{
				{"https://example.test", "first-secret", ""},
				{"https://example.test", "rotated-secret", `{"id":"existing"}`},
				{"https://other.test", "rotated-secret", ""},
			} {
				if kind == "intune" {
					step.endpoint += "/v1.0"
				}
				t.Setenv("STEMMA_TEST_ENDPOINT", step.endpoint)
				t.Setenv("STEMMA_TEST_SECRET", step.secret)
				called := false
				options.Handlers = map[string]reconcileHandler{kind: func(_ context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
					if request.Method == "validate" || request.Method == "plan" {
						return plugin.ReconcileResponse{}, nil
					}
					called = true
					if compactJSON(t, request.Binding) != step.binding {
						t.Fatalf("run %d received binding %s, want %s", i, request.Binding, step.binding)
					}
					var settings map[string]string
					if err := json.Unmarshal(request.Config, &settings); err != nil || settings[secretField] != step.secret {
						t.Fatal("destination did not receive resolved secrets")
					}
					return plugin.ReconcileResponse{Binding: json.RawMessage(`{"id":"existing"}`)}, nil
				}}
				if _, err := Run(t.Context(), options); err != nil || !called {
					t.Fatalf("run %d: %v, called=%v", i, err, called)
				}
				data, err := os.ReadFile(filepath.Join(options.StateDir, "rotation.json"))
				if err != nil || strings.Contains(string(data), step.secret) {
					t.Fatalf("state exposed a credential: %v", err)
				}
			}
		})
	}
}
