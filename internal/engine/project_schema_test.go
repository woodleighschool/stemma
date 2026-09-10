package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/plugin"
)

func TestProjectSchemaOnlyDescribesOperations(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "stemma.yaml")
	if err := os.WriteFile(filename, []byte(`apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: fixture
spec:
  imports:
    - not-created-yet.yaml
  destinations:
    deployment:
      operation: intune
      config:
        tenant_id: '${STEMMA_MISSING_SCHEMA_TENANT}'
`), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := ProjectSchema(t.Context(), Options{ConfigPath: filename, Lock: lockfile.Options{Offline: true}, Handlers: map[string]reconcileHandler{"intune": func(context.Context, plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
		t.Fatal("schema generation invoked a destination")
		return plugin.ReconcileResponse{}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("missing project schema")
	}
}
