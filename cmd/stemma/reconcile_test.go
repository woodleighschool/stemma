package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/reconcile"
)

func TestReconcileRequiresSourceControl(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stemma.yaml"), []byte("apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: catalog\nspec:\n  imports:\n    - '*.yaml'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cmd, finish := command(&out, &errOut)
	cmd.SetArgs([]string{"reconcile", "--root", root, "--no-progress"})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
	if err == nil || !strings.Contains(err.Error(), "reconcile.source_control") {
		t.Fatalf("project without source control accepted: %v", err)
	}
}

func TestPrintReconcile(t *testing.T) {
	report := reconcile.Report{Branch: "main", Head: "0123456789abcdef", Apply: &reconcile.Apply{Commit: "0123456789abcdef", Summary: "2 resources, 3 destination changes"}, Updates: []reconcile.Update{
		{Resource: "stemma/v1alpha1/MacSoftware/chrome", Branch: "stemma/MacSoftware/chrome", Action: "created", PullRequest: "https://github.example/pull/1", Summary: "MacSoftware/chrome 129: 3 planned changes across 1 destination"},
		{Resource: "stemma/v1alpha1/MacSoftware/broken", Action: "failed", Error: "download returned HTTP 404"},
	}}
	var out bytes.Buffer
	if err := printReconcile(&out, report); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Reviewed: main@0123456789ab applied (2 resources, 3 destination changes)", "MacSoftware/chrome: created https://github.example/pull/1 MacSoftware/chrome 129", "MacSoftware/broken: failed download returned HTTP 404"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("report missing %q:\n%s", want, out.String())
		}
	}
}
