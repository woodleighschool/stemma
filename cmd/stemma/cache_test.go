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

	"github.com/woodleighschool/stemma/internal/cas"
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
	cmd, finish := command(&out, &logs)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(t.Context())
	finish(err)
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
	if err != nil || stderr != "" || !strings.Contains(stdout, "Reclaimed 0 B") || !strings.Contains(stdout, "protected") || !store.Has(ref) {
		t.Fatalf("recent prune: %s / %s / %v", stdout, stderr, err)
	}
	stdout, _, err = cacheCLI(t, append(args, "prune", "--all", "--dry-run", "--json")...)
	var result cas.Collection
	if err != nil || json.Unmarshal([]byte(stdout), &result) != nil || result.Reclaimed != ref.Size || !store.Has(ref) {
		t.Fatalf("dry run: %s / %v", stdout, err)
	}
	stdout, _, err = cacheCLI(t, append(args, "prune", "--all")...)
	if err != nil || !strings.Contains(stdout, "Reclaimed 19 B") || store.Has(ref) {
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
			if err == nil || out != "" || !strings.Contains(logs, "Cache: reclaimed") {
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
