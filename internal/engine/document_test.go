package engine

import (
	"encoding/json"
	"github.com/woodleighschool/stemma/internal/testproject"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"howett.net/plist"
)

func TestSharedPkginfoDocumentPreservesPatchValues(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Payload"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Payload", "fixture.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "stemma.yaml")
	manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata:
  name: document
spec:
  destinations:
    local: {operation: munki, path: repo}
  imports: ['*.software.yaml']
---
apiVersion: stemma/v1alpha1
kind: Software
metadata:
  name: fixture
spec:
  source: {type: local, include: [Payload/**]}
  steps:
    - name: build
      operation: pkg
      inputs: {input: prepared}
      config: {identifier: org.example.fixture, version: "1", payload: Payload}
    - name: metadata
      operation: munki.pkginfo
      inputs: {input: build/artifact}
      config:
        name: Fixture
        description: original
        blocking_applications: []
        unattended_install: false
  destinations:
    local:
      artifact: metadata/artifact
      inputs: {installer: build/artifact}
      catalogs: [testing]
`
	write := func(data string) {
		t.Helper()
		if err := testproject.Write(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	write(manifest)
	opts := Options{ConfigPath: path, CacheDir: t.TempDir(), Method: "apply"}
	first, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	write(strings.Replace(manifest, "description: original", "description: null", 1))
	second, err := Run(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Software[0].Steps[0].Cached || second.Software[0].Steps[1].Cached || first.Software[0].Steps[0].Artifacts["artifact"].Payload != second.Software[0].Steps[0].Artifacts["artifact"].Payload {
		t.Fatal("metadata document edit rebuilt installer")
	}
	for _, change := range second.Software[0].Destinations[0].Changes {
		if change.Kind == "content" {
			t.Fatal("metadata document edit republished installer")
		}
	}
	documentRef := second.Software[0].Steps[1].Artifacts["artifact"].Payload
	// Open via CAS rather than depending on the cache's on-disk layout.
	store, err := cas.Open(opts.CacheDir)
	if err != nil {
		t.Fatal(err)
	}
	documentPath, err := store.Path(documentRef)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(documentPath)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if value, exists := document["description"]; !exists || value != nil {
		t.Fatal("native document lost explicit PATCH null")
	}
	if document["unattended_install"] != false || len(document["blocking_applications"].([]any)) != 0 {
		t.Fatal("native document lost false or empty list")
	}
	files, err := filepath.Glob(filepath.Join(root, "repo", "pkgsinfo", "stemma", "*.plist"))
	if err != nil || len(files) != 1 {
		t.Fatal("missing local pkginfo")
	}
	data, err = os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]any
	if _, err := plist.Unmarshal(data, &actual); err != nil {
		t.Fatal(err)
	}
	if _, exists := actual["description"]; exists {
		t.Fatal("local repository did not clear description")
	}
	if actual["installer_item_hash"] != first.Software[0].Steps[0].Artifacts["artifact"].Payload.SHA256 {
		t.Fatal("local repository published pkginfo JSON as installer")
	}
}
