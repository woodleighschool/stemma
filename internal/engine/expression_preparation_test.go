package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
)

func TestBuildExpressionsTrackPreparationInputs(t *testing.T) {
	root := t.TempDir()
	if err := os.CopyFS(filepath.Join(root, "Vendor.app"), os.DirFS("../apple/testdata/Fixture.app")); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "stemma.yaml")
	manifest := `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: expressions}
spec:
  imports: ['*.software.yaml']
  destinations:
    repo: {operation: munki, config: {path: repo}}
---
apiVersion: stemma/v1alpha1
kind: BuildMacPkg
metadata: {name: vendor}
spec:
  inputs:
    vendor: {path: Vendor.app}
  inspect:
    app: {$input: vendor}
  package:
    identifier: org.example.vendor
    version: "{{ facts.app.app.version }}-1"
  payload:
    /Applications/Vendor.app: {$input: vendor}
    /Library/Example/message:
      content: "{{ env.STEMMA_TEST_CONTENT }}"
      uid: "{{ int(env.STEMMA_TEST_UID) }}"
---
apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: vendor}
spec:
  source:
    resource: {kind: BuildMacPkg, name: vendor}
  destinations:
    repo:
      pkginfo:
        description: "{{ env.STEMMA_TEST_DESCRIPTION }}"
`
	testproject.Write(t, filename, manifest)
	t.Setenv("STEMMA_TEST_CONTENT", "one")
	t.Setenv("STEMMA_TEST_UID", "501")
	t.Setenv("STEMMA_TEST_DESCRIPTION", "first")
	opts := Options{ConfigPath: filename, CacheDir: t.TempDir(), Method: "prepare"}
	run := func() ResourceReport {
		t.Helper()
		report, err := Run(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Resources) != 2 {
			t.Fatalf("resources=%d", len(report.Resources))
		}
		return report.Resources[0]
	}
	first := run()
	if first.Cached || first.Artifacts["installer"].Version == "" {
		t.Fatalf("first build=%+v", first)
	}
	unchanged := run()
	if !unchanged.Cached {
		t.Fatal("unchanged build missed cache")
	}
	t.Setenv("STEMMA_TEST_DESCRIPTION", "changed publication")
	t.Setenv("STEMMA_TEST_UNUSED", "unrelated")
	if result := run(); !result.Cached {
		t.Fatal("publication or unrelated environment rebuilt package")
	}
	t.Setenv("STEMMA_TEST_CONTENT", "two")
	envChanged := run()
	if envChanged.Cached || envChanged.Artifacts["installer"].Payload == first.Artifacts["installer"].Payload || envChanged.Artifacts["installer"].Version != first.Artifacts["installer"].Version {
		t.Fatal("used environment did not rebuild with unchanged receipt version")
	}
	payload := filepath.Join(root, "Vendor.app/Contents/Resources/message.txt")
	if err := os.WriteFile(payload, []byte("changed source bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	opts.Method = "update"
	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	opts.Method = "prepare"
	bytesChanged := run()
	if bytesChanged.Cached || bytesChanged.Artifacts["installer"].Payload == envChanged.Artifacts["installer"].Payload || bytesChanged.Artifacts["installer"].Version != envChanged.Artifacts["installer"].Version {
		t.Fatal("changed source bytes did not rebuild with unchanged receipt version")
	}
	plist := filepath.Join(root, "Vendor.app/Contents/Info.plist")
	data, err := os.ReadFile(plist)
	if err != nil {
		t.Fatal(err)
	}
	oldVersion := strings.TrimSuffix(first.Artifacts["installer"].Version, "-1")
	data = []byte(strings.Replace(string(data), "<string>"+oldVersion+"</string>", "<string>9.8.7</string>", 1))
	if err := os.WriteFile(plist, data, 0o644); err != nil {
		t.Fatal(err)
	}
	opts.Method = "update"
	if _, err := Run(t.Context(), opts); err != nil {
		t.Fatal(err)
	}
	opts.Method = "prepare"
	if result := run(); result.Cached || result.Artifacts["installer"].Version != "9.8.7-1" {
		t.Fatalf("inspection did not refresh version: %+v", result.Artifacts["installer"])
	}
}

func TestResourceExpressionsKeepSchedulingReferencesLiteral(t *testing.T) {
	for _, source := range []any{
		map[string]any{"resolver": "{{ 'http' }}", "url": "https://example.invalid/app.pkg"},
		"{{ {'resource': {'kind': 'BuildMacPkg', 'name': 'other'}} }}",
		"{{ {'resolver': 'http', 'url': 'https://example.invalid/app.pkg'} }}",
	} {
		resource := config.Resource{APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Spec: map[string]any{"source": source}}
		if _, _, err := resourceDeclaration(resource); err == nil {
			t.Fatal("accepted expression-generated scheduling reference")
		}
	}
}

func TestResourceExpressionsTreatFileNamesAndProviderSettingsAsData(t *testing.T) {
	t.Setenv("STEMMA_TEST_RESOURCE_CONTENT", "fixture content")
	for name, resource := range map[string]config.Resource{
		"build file entries": {APIVersion: "stemma/v1alpha1", Kind: "BuildMacPkg", Spec: map[string]any{
			"scripts": map[string]any{
				"resource":  map[string]any{"content": "{{ env.STEMMA_TEST_RESOURCE_CONTENT }}"},
				"resolver":  map[string]any{"content": "{{ env.STEMMA_TEST_RESOURCE_CONTENT }}"},
				"operation": map[string]any{"content": "{{ env.STEMMA_TEST_RESOURCE_CONTENT }}"},
			},
			"payload": map[string]any{"operation": map[string]any{"content": "{{ env.STEMMA_TEST_RESOURCE_CONTENT }}"}},
		}},
		"HTTP header": {APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Spec: map[string]any{
			"source": map[string]any{"resolver": "http", "url": "https://example.invalid/app.pkg", "headers": map[string]any{"resource": "{{ env.STEMMA_TEST_RESOURCE_CONTENT }}"}},
		}},
		"Windows file name": {APIVersion: "stemma/v1alpha1", Kind: "WindowsSoftware", Spec: map[string]any{
			"content": map[string]any{"files": map[string]any{"resolver": map[string]any{"path": "{{ env.STEMMA_TEST_RESOURCE_CONTENT }}"}}},
		}},
		"external plugin data": {APIVersion: "example.test/v1", Kind: "Custom", Spec: map[string]any{
			"resource": map[string]any{"operation": "{{ env.STEMMA_TEST_RESOURCE_CONTENT }}"},
			"source":   map[string]any{"resolver": "{{ env.STEMMA_TEST_RESOURCE_CONTENT }}"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resourceDeclaration(resource); err != nil {
				t.Fatalf("ordinary data was treated as a scheduling reference: %v", err)
			}
		})
	}
}

func TestResourceExpressionsKeepScopedInputAndOutputReferencesLiteral(t *testing.T) {
	for name, resource := range map[string]config.Resource{
		"source reference": {APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Spec: map[string]any{
			"source": map[string]any{"resource": map[string]any{"kind": "BuildMacPkg", "name": "{{ 'other' }}"}},
		}},
		"named input resolver": {APIVersion: "stemma/v1alpha1", Kind: "BuildMacPkg", Spec: map[string]any{
			"inputs": map[string]any{"vendor": map[string]any{"resolver": "{{ 'http' }}", "url": "https://example.invalid/app.pkg"}},
		}},
		"generated inputs": {APIVersion: "stemma/v1alpha1", Kind: "BuildMacPkg", Spec: map[string]any{
			"inputs": "{{ {'vendor': {'resource': {'kind': 'MacSoftware', 'name': 'other'}}} }}",
		}},
		"generated inspection": {APIVersion: "stemma/v1alpha1", Kind: "BuildMacPkg", Spec: map[string]any{
			"inspect": "{{ {'app': {'$input': 'vendor'}} }}",
		}},
		"script input": {APIVersion: "stemma/v1alpha1", Kind: "BuildMacPkg", Spec: map[string]any{
			"scripts": map[string]any{"resource": map[string]any{"$input": "{{ 'vendor' }}"}},
		}},
		"generated Windows files": {APIVersion: "stemma/v1alpha1", Kind: "WindowsSoftware", Spec: map[string]any{
			"content": "{{ {'files': {'config.xml': {'resource': {'kind': 'Custom', 'name': 'other'}}}} }}",
		}},
		"destination installer": {APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Spec: map[string]any{
			"destinations": map[string]any{"repo": map[string]any{"installer": "{{ 'other' }}"}},
		}},
		"destination input": {APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Spec: map[string]any{
			"destinations": map[string]any{"repo": map[string]any{"inputs": map[string]any{"helper": "{{ 'other' }}"}}},
		}},
		"destination input map": {APIVersion: "stemma/v1alpha1", Kind: "MacSoftware", Spec: map[string]any{
			"destinations": map[string]any{"repo": map[string]any{"inputs": "{{ {'helper': 'other'} }}"}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := resourceDeclaration(resource); err == nil {
				t.Fatal("accepted computed scheduling reference")
			}
		})
	}
}
