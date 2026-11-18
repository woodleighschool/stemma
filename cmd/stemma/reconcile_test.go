package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lock"
	"github.com/woodleighschool/stemma/internal/reconcile"
)

func TestReconcileRequiresSourceControl(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stemma.yaml"), []byte("apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: catalog\nspec:\n  imports:\n    - '*.yaml'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cmd, finish := command(&out, &errOut, nil)
	cmd.SetArgs([]string{"reconcile", "--root", root})
	err := cmd.ExecuteContext(t.Context())
	err = finish(err)
	if err == nil || !strings.Contains(err.Error(), "reconcile.source_control") {
		t.Fatalf("project without source control accepted: %v", err)
	}
}

func TestReconcileStreamsOutcomesAndCountsProposals(t *testing.T) {
	report := reconcile.Report{Branch: "main", Head: "0123456789abcdef", Apply: &reconcile.Apply{Commit: "0123456789abcdef", Summary: "2 resources, 3 destination changes", Elapsed: 65 * time.Second}, Update: &reconcile.Update{Proposals: []reconcile.Proposal{
		{Name: "MacSoftware/chrome", Branch: "stemma/MacSoftware/chrome", Action: "created", PullRequest: "https://github.example/pull/1", Summary: "MacSoftware/chrome 129: 3 planned changes across 1 destination"},
		{Name: "MacSoftware/broken", Action: "failed", Error: "download returned HTTP 404"},
		{Name: "MacSoftware/firefox", Action: "unchanged", PullRequest: "https://github.example/pull/2"},
	}}}
	lookup := []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "chrome", Key: "stemma/v1alpha1/MacSoftware/chrome", Inputs: []lock.InputChange{{Resource: "stemma/v1alpha1/MacSoftware/chrome", Input: "source", Kind: lock.Updated}}},
		{Kind: "MacSoftware", Name: "firefox", Key: "stemma/v1alpha1/MacSoftware/firefox"},
	}
	for _, interactive := range []bool{false, true} {
		var out bytes.Buffer
		o := newCommandOutput(&out, &bytes.Buffer{})
		o.interactive, o.reconciling = interactive, true
		if err := o.applyDone(report); err != nil {
			t.Fatal(err)
		}
		for _, resource := range lookup {
			if err := o.resourceDone("update", resource); err != nil {
				t.Fatal(err)
			}
		}
		// A proposal's checks are summarised by its outcome line alone.
		if err := o.resourceDone("prepare", engine.ResourceReport{Kind: "MacSoftware", Name: "chrome"}); err != nil {
			t.Fatal(err)
		}
		for _, proposal := range report.Update.Proposals {
			if err := o.proposalDone(proposal); err != nil {
				t.Fatal(err)
			}
		}
		if err := o.reconciled(&out, report); err != nil {
			t.Fatal(err)
		}
		want := "✓ main@0123456789ab: applied in 1m5s\n  2 resources, 3 destination changes\n" +
			"✓ MacSoftware/chrome: created\n  pull request: https://github.example/pull/1\n  MacSoftware/chrome 129: 3 planned changes across 1 destination\n" +
			"✗ MacSoftware/broken: failed\n  download returned HTTP 404\n" +
			"✓ MacSoftware/firefox: unchanged\n  pull request: https://github.example/pull/2\n" +
			"\n3 proposals: 1 created, 1 failed, 1 unchanged.\n"
		if out.String() != want {
			t.Fatalf("interactive=%v:\n%s\nwant:\n%s", interactive, out.String(), want)
		}
	}
}

func TestReconcileShowsEachFailureOnce(t *testing.T) {
	const head = "0123456789abcdef"
	const unloadable = "main@0123456789ab: lock: version 2 is not supported; run stemma update"
	for name, test := range map[string]struct {
		report         reconcile.Report
		err            error
		stdout, stderr string
	}{
		// Neither phase ran: the error is the whole report.
		"project does not load": {
			report: reconcile.Report{Branch: "main", Head: head, Error: unloadable},
			err:    errors.New(unloadable),
			stderr: "✗ Error:\n  " + unloadable + "\n",
		},
		"phases stopped": {
			report: reconcile.Report{Branch: "main", Head: head,
				Apply:  &reconcile.Apply{Commit: head, Error: "plugin downloads: image is not cached", Report: &engine.Report{Error: "plugin downloads: image is not cached"}},
				Update: &reconcile.Update{Error: "lock contains stale plugins; run stemma plugins update"},
			},
			err: reconcile.ErrFailed,
			stdout: "✗ main@0123456789ab: failed\n  plugin downloads: image is not cached\n" +
				"\n✗ Error:\n  lock contains stale plugins; run stemma plugins update\n",
		},
		// Resources and proposals show their own failures as they finish.
		"resources and proposals failed": {
			report: reconcile.Report{Branch: "main", Head: head,
				Apply:  &reconcile.Apply{Commit: head, Summary: "1 resource failed: chrome", Report: &engine.Report{Error: "MacSoftware/chrome: upload failed"}},
				Update: &reconcile.Update{Proposals: []reconcile.Proposal{{Name: "MacSoftware/firefox", Action: "failed", Error: "push rejected"}}},
			},
			err: reconcile.ErrFailed,
			stdout: "✗ main@0123456789ab: failed\n  1 resource failed: chrome\n" +
				"✗ MacSoftware/firefox: failed\n  push rejected\n" +
				"\n1 proposal: 1 failed.\n",
		},
		"lookup interrupted": {
			report: reconcile.Report{Branch: "main", Head: head,
				Apply:  &reconcile.Apply{Commit: head, Summary: "2 resources, 3 destination changes", Report: &engine.Report{}},
				Update: &reconcile.Update{},
			},
			err: errInterrupted,
			stdout: "✓ main@0123456789ab: applied\n  2 resources, 3 destination changes\n" +
				"\n0 proposals.\n",
			stderr: "– Interrupted.\n",
		},
	} {
		t.Run(name, func(t *testing.T) {
			var out, logs bytes.Buffer
			o := newCommandOutput(&out, &logs)
			o.reconciling = true
			if test.report.Apply != nil {
				if err := o.applyDone(test.report); err != nil {
					t.Fatal(err)
				}
			}
			if test.report.Update != nil {
				for _, proposal := range test.report.Update.Proposals {
					if err := o.proposalDone(proposal); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := o.reconciled(&out, test.report); err != nil {
				t.Fatal(err)
			}
			_ = o.finish(test.err)
			if out.String() != test.stdout || logs.String() != test.stderr {
				t.Fatalf("stdout:\n%s\nwant:\n%s\nstderr:\n%s\nwant:\n%s", out.String(), test.stdout, logs.String(), test.stderr)
			}
		})
	}
}

func TestFailedProposalRetainsItsPullRequest(t *testing.T) {
	row := proposalRow(reconcile.Proposal{Error: "planning failed", PullRequest: "https://example.test/pulls/1"})
	if row.Summary.String() != "failed" || len(row.Details) != 2 || row.Details[0].String() != "planning failed" || row.Details[1].String() != "pull request: https://example.test/pulls/1" {
		t.Fatalf("failed proposal lost its result: %+v", row)
	}
}
