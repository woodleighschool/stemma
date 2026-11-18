package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/engine"
)

func TestMain(m *testing.M) {
	// Commands that don't specify a cache must never maintain the developer's cache.
	dir, err := os.MkdirTemp("", "stemma-cli-tests-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("STEMMA_CACHE_DIR", dir); err != nil {
		panic(err)
	}
	if err := os.Setenv("STEMMA_CACHE_MAX_SIZE", "32GiB"); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func cacheCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var out, logs bytes.Buffer
	cmd, finish := command(&out, &logs, nil)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	err = finish(err)
	return out.String(), logs.String(), err
}

func TestCachePolicyAndPruneReports(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref, err := store.Import(t.Context(), strings.NewReader("synthetic installer"), "")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"--cache-dir", store.Dir, "--cache-max-size", "1B", "cache"}
	stdout, stderr, err := cacheCLI(t, append(args, "prune")...)
	if err != nil || stderr != "" || !strings.Contains(stdout, "reclaimed 0 B") || !strings.Contains(stdout, "protected") || !store.Has(ref) {
		t.Fatalf("recent prune: %s / %s / %v", stdout, stderr, err)
	}
	stdout, _, err = cacheCLI(t, append(args, "prune", "--all", "--dry-run", "--json")...)
	var result cas.Collection
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Reclaimed != ref.Size || !store.Has(ref) {
		t.Fatalf("dry run: %s / %v", stdout, err)
	}
	stdout, _, err = cacheCLI(t, append(args, "prune", "--all")...)
	if err != nil || !strings.Contains(stdout, "reclaimed 19 B") || store.Has(ref) {
		t.Fatalf("all: %s / %v", stdout, err)
	}
	t.Setenv("STEMMA_CACHE_MAX_SIZE", "7GiB")
	stdout, _, err = cacheCLI(t, "cache", "info", "--json")
	if err != nil || !strings.Contains(stdout, `"max_size_bytes": 7516192768`) {
		t.Fatalf("environment: %s / %v", stdout, err)
	}
	stdout, _, err = cacheCLI(t, "--cache-max-size", "0", "cache", "info")
	if err != nil || !strings.Contains(stdout, "Budget: disabled") {
		t.Fatalf("override: %s / %v", stdout, err)
	}
	for _, invalid := range []string{"bad", "-1", "9223372036854775808", "18446744073709551616GiB"} {
		if _, _, err := cacheCLI(t, "--cache-max-size", invalid, "cache", "info"); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestCommandMaintenanceRunsOnFailureAndSkipsOfflineEviction(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(map[bool]string{false: "online", true: "offline"}[offline], func(t *testing.T) {
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ref, err := store.Import(t.Context(), strings.NewReader("old synthetic installer"), "")
			if err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-2 * cas.Grace)
			if err := os.Chtimes(filepath.Join(store.Dir, "uses", "objects-"+ref.SHA256), old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store.Dir, "work", "abandoned"), []byte("temporary"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--root", t.TempDir(), "--cache-dir", store.Dir, "--cache-max-size", "1B", "prepare"}
			if offline {
				args = append(args, "--offline")
			}
			out, logs, err := cacheCLI(t, args...)
			if err == nil || out != "" || strings.Contains(logs, "Cache") {
				t.Fatalf("failed command: %s / %s / %v", out, logs, err)
			}
			if store.Has(ref) != offline {
				t.Fatalf("offline=%v cache presence=%v", offline, store.Has(ref))
			}
			usage, err := store.Info(context.Background())
			if err != nil || usage.Work != 0 {
				t.Fatalf("temporary work: %+v / %v", usage, err)
			}
		})
	}
}

func TestJSONIncludesCleanupWarnings(t *testing.T) {
	var out, logs bytes.Buffer
	display := newCommandOutput(&out, &logs)
	c := &cli{out: &out, display: display, cacheDir: t.TempDir()}
	root := &cobra.Command{Use: "stemma", PersistentPreRunE: func(cmd *cobra.Command, _ []string) error { return display.start(cmd) }}
	cmd := &cobra.Command{Use: "update", RunE: func(_ *cobra.Command, _ []string) error {
		// Break an empty cache directory after startup maintenance. The command
		// succeeds; only the cleanup inventory fails.
		objects := filepath.Join(c.cacheDir, "objects")
		if err := os.Remove(objects); err != nil {
			return err
		}
		if err := os.WriteFile(objects, []byte("not a directory"), 0o600); err != nil {
			return err
		}
		c.result = func() error {
			return display.report(&out, "update", engine.Report{Command: "update", Resources: []engine.ResourceReport{}})
		}
		return nil
	}}
	jsonFlag(cmd)
	root.AddCommand(cmd)
	c.maintainCommands(root)
	root.SetArgs([]string{"update", "--json"})
	err := root.ExecuteContext(t.Context())
	err = display.finish(err)
	var report engine.Report
	if err != nil || json.Unmarshal(out.Bytes(), &report) != nil || len(report.Warnings) != 1 || !strings.Contains(report.Warnings[0], "Cache maintenance incomplete") || logs.Len() != 0 {
		t.Fatalf("cleanup result=%s stderr=%s error=%v", out.String(), logs.String(), err)
	}
}
