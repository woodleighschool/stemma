package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/reconcile"
)

func TestReconcileRequiresSourceControl(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stemma.yaml"), []byte("apiVersion: stemma/v1alpha1\nkind: Project\nmetadata:\n  name: catalog\nspec:\n  imports:\n    - '*.yaml'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cmd, finish := command(&out, &errOut)
	cmd.SetArgs([]string{"reconcile", "--root", root})
	err := cmd.ExecuteContext(t.Context())
	finish(err)
	if err == nil || !strings.Contains(err.Error(), "reconcile.source_control") {
		t.Fatalf("project without source control accepted: %v", err)
	}
}

func TestReconcileStreamsOutcomesAndCountsProposals(t *testing.T) {
	report := reconcile.Report{Branch: "main", Head: "0123456789abcdef", Apply: &reconcile.Apply{Commit: "0123456789abcdef", Summary: "2 resources, 3 destination changes"}, Update: &reconcile.Update{Proposals: []reconcile.Proposal{
		{Resource: "MacSoftware/chrome", Branch: "stemma/MacSoftware/chrome", Action: "created", PullRequest: "https://github.example/pull/1", Summary: "MacSoftware/chrome 129: 3 planned changes across 1 destination"},
		{Resource: "MacSoftware/broken", Action: "failed", Error: "download returned HTTP 404"},
		{Resource: "MacSoftware/firefox", Action: "unchanged", PullRequest: "https://github.example/pull/2"},
	}}}
	lookup := []engine.ResourceReport{
		{Kind: "MacSoftware", Name: "chrome", Key: "stemma/v1alpha1/MacSoftware/chrome", Inputs: []lockfile.InputChange{{Resource: "stemma/v1alpha1/MacSoftware/chrome", Input: "source", ContentChanged: true}}},
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
		if err := o.reconciled(&out, report, nil); err != nil {
			t.Fatal(err)
		}
		want := "Reviewed: main@0123456789ab applied (2 resources, 3 destination changes)\n"
		if interactive {
			want += "MacSoftware/chrome: 1 input changed\nMacSoftware/firefox: inputs unchanged\n"
		}
		want += "MacSoftware/chrome: created https://github.example/pull/1\n  MacSoftware/chrome 129: 3 planned changes across 1 destination\n" +
			"MacSoftware/broken: failed\n  error: download returned HTTP 404\n"
		if interactive {
			want += "MacSoftware/firefox: unchanged https://github.example/pull/2\n"
		}
		want += "Proposals: 1 created, 1 failed, 1 unchanged.\n"
		if out.String() != want {
			t.Fatalf("interactive=%v:\n%s\nwant:\n%s", interactive, out.String(), want)
		}
	}
}

func TestReconcileShowsEachFailureOnce(t *testing.T) {
	const head = "0123456789abcdef"
	const unloadable = "main@0123456789ab: lockfile: version 2 is not supported; delete it and run stemma update"
	for name, test := range map[string]struct {
		report         reconcile.Report
		err            error
		stdout, stderr string
	}{
		// Neither phase ran: the error is the whole report.
		"project does not load": {
			report: reconcile.Report{Branch: "main", Head: head, Error: unloadable},
			err:    errors.New(unloadable),
			stderr: "Error: " + unloadable + "\n",
		},
		"phases stopped": {
			report: reconcile.Report{Branch: "main", Head: head,
				Apply:  &reconcile.Apply{Commit: head, Error: "plugin downloads: image is not cached", Report: &engine.Report{Error: "plugin downloads: image is not cached"}},
				Update: &reconcile.Update{Error: "lockfile contains stale plugins; run stemma plugins update"},
			},
			err: reconcile.ErrFailed,
			stdout: "Reviewed: main@0123456789ab failed\n  error: plugin downloads: image is not cached\n" +
				"Proposals: failed\n  error: lockfile contains stale plugins; run stemma plugins update\n",
		},
		// Resources and proposals show their own failures as they finish.
		"resources and proposals failed": {
			report: reconcile.Report{Branch: "main", Head: head,
				Apply:  &reconcile.Apply{Commit: head, Summary: "1 resource failed: chrome", Report: &engine.Report{Error: "MacSoftware/chrome: upload failed"}},
				Update: &reconcile.Update{Proposals: []reconcile.Proposal{{Resource: "MacSoftware/firefox", Action: "failed", Error: "push rejected"}}},
			},
			err: reconcile.ErrFailed,
			stdout: "Reviewed: main@0123456789ab failed (1 resource failed: chrome)\n" +
				"MacSoftware/firefox: failed\n  error: push rejected\nProposals: 1 failed.\n",
		},
		"lookup interrupted": {
			report: reconcile.Report{Branch: "main", Head: head,
				Apply:  &reconcile.Apply{Commit: head, Summary: "2 resources, 3 destination changes", Report: &engine.Report{}},
				Update: &reconcile.Update{},
			},
			err:    context.Canceled,
			stdout: "Reviewed: main@0123456789ab applied (2 resources, 3 destination changes)\nProposals interrupted: none.\n",
			stderr: "Interrupted.\n",
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
			if err := o.reconciled(&out, test.report, test.err); err != nil {
				t.Fatal(err)
			}
			o.finish(test.err)
			if out.String() != test.stdout || logs.String() != test.stderr {
				t.Fatalf("stdout:\n%s\nwant:\n%s\nstderr:\n%s\nwant:\n%s", out.String(), test.stdout, logs.String(), test.stderr)
			}
		})
	}
}
