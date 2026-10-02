package cas

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/plugin"
)

func testStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

func importObject(t *testing.T, s *Store, content string) Ref {
	t.Helper()
	ref, err := s.Import(t.Context(), strings.NewReader(content), "")
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func putFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLRUUsesHitsWithoutChangingArtifactTimes(t *testing.T) {
	s, now := testStore(t)
	hot := importObject(t, s, "first installer reused later")
	path, _ := s.Path(hot)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	cold := importObject(t, s, "second installer")
	*now = now.Add(2 * Grace)
	if err := s.Verify(t.Context(), hot); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if before.ModTime() != after.ModTime() {
		t.Fatal("cache hit changed artifact timestamp")
	}
	result, err := s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{})
	if err != nil || s.Has(cold) || !s.Has(hot) || result.After.Retained != hot.Size || result.Reclaimed != cold.Size {
		t.Fatalf("LRU did not preserve actual use: %+v / %v", result, err)
	}
	if result.After.Retained <= 1 {
		t.Fatal("working set should exceed soft target")
	}
}

func TestCollectionAccountsForCopiesAndInvalidatesIndexes(t *testing.T) {
	s, now := testStore(t)
	ref := importObject(t, s, "installer")
	descriptor := importObject(t, s, "output descriptor")
	sourceKey, derivationKey := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := s.RememberSource(t.Context(), sourceKey, "response", ref.SHA256); err != nil {
		t.Fatal(err)
	}
	if err := s.Remember(t.Context(), derivationKey, descriptor, ref); err != nil {
		t.Fatal(err)
	}
	materialized := filepath.Join(s.Dir, "materialized", ref.SHA256, "setup.pkg")
	if err := s.Materialize(t.Context(), ref, materialized); err != nil {
		t.Fatal(err)
	}
	putFile(t, filepath.Join(s.Dir, "work", "abandoned", "scratch"), "scratch")
	putFile(t, filepath.Join(s.Dir, "objects", ".import-interrupted"), "partial")
	*now = now.Add(2 * Grace)
	usage, err := s.Info(t.Context())
	if err != nil || usage.Materialized != ref.Size || usage.Metadata == 0 || usage.Work != 14 {
		t.Fatalf("usage: %+v / %v", usage, err)
	}
	dry, err := s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{DryRun: true})
	if err != nil || !s.Has(ref) {
		t.Fatalf("dry run removed object: %+v / %v", dry, err)
	}
	if _, err := os.Stat(materialized); err != nil {
		t.Fatal(err)
	}
	result, err := s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{})
	if err != nil || !reflect.DeepEqual(result, dry) || result.Reclaimed != usage.Retained+usage.Work || result.After != (Usage{}) {
		t.Fatalf("prune: %+v / dry %+v / %v", result, dry, err)
	}
	var response string
	if s.RecallSource(t.Context(), sourceKey, &response) {
		t.Fatal("source index survived eviction")
	}
	if _, hit := s.Recall(t.Context(), derivationKey); hit {
		t.Fatal("derivation survived eviction")
	}
	for _, path := range []string{materialized, filepath.Join(s.Dir, "sources", sourceKey), filepath.Join(s.Dir, "derivations", derivationKey)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan: %s / %v", path, err)
		}
	}
}

func TestInventoryDoesNotRefreshMetadataRecency(t *testing.T) {
	s, now := testStore(t)
	key := strings.Repeat("c", 64)
	if err := s.RememberSource(t.Context(), key, "metadata"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * Grace)
	if _, err := s.Info(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{})
	if err != nil || result.Reclaimed == 0 || result.After.Retained != 0 {
		t.Fatalf("inspection refreshed recency: %+v / %v", result, err)
	}
	if err := s.RememberSource(t.Context(), key, "metadata"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * Grace)
	var value string
	if !s.RecallSource(t.Context(), key, &value) || value != "metadata" {
		t.Fatal("metadata miss")
	}
	result, err = s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{})
	if err != nil || result.Reclaimed != 0 {
		t.Fatalf("successful reuse not protected: %+v / %v", result, err)
	}
}

func TestMaintenanceLifecycle(t *testing.T) {
	for _, test := range []struct {
		name              string
		offline, disabled bool
	}{
		{"online", false, false}, {"offline", true, false}, {"disabled", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, now := testStore(t)
			ref := importObject(t, s, "old payload")
			*now = now.Add(2 * Grace)
			putFile(t, filepath.Join(s.Dir, "work", "crashed"), "scratch")
			policy := Policy{MaxSize: 1}
			if test.disabled {
				policy.MaxSize = 0
			}
			original := errors.New("publication failed")
			err := s.run(t.Context(), policy, test.offline, func(ctx context.Context) error {
				if _, err := os.Stat(filepath.Join(s.Dir, "work", "crashed")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("startup did not recover abandoned work")
				}
				if s.Has(ref) != (test.offline || test.disabled) {
					t.Fatal("incorrect startup eviction")
				}
				putFile(t, filepath.Join(s.Dir, "work", "failed"), "scratch")
				if _, err := s.collect(ctx, policy, PruneOptions{All: true}, false, true); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(filepath.Join(s.Dir, "work", "failed")); err != nil {
					t.Fatal("collection ran inside own lease")
				}
				return original
			})
			usage, infoErr := s.Info(t.Context())
			if !errors.Is(err, original) || infoErr != nil || usage.Work != 0 {
				t.Fatalf("completion: %+v / %v / %v", usage, err, infoErr)
			}
		})
	}
}

func TestReturnedMaterializationSurvivesConsecutiveCommands(t *testing.T) {
	s, now := testStore(t)
	var ref Ref
	var path string
	policy := Policy{MaxSize: 1}
	err := s.run(t.Context(), policy, false, func(ctx context.Context) error {
		ref = importObject(t, s, "fresh installer")
		path = filepath.Join(s.Dir, "materialized", ref.SHA256, "setup.pkg")
		return s.Materialize(ctx, ref, path)
	})
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	if err := s.run(t.Context(), policy, false, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("next command removed returned path", err)
	}
	*now = now.Add(2 * Grace)
	if err := s.run(t.Context(), policy, true, func(ctx context.Context) error { return s.Verify(ctx, ref) }); err != nil {
		t.Fatal("offline reuse lost prepared content", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceFailureDoesNotReplaceCommandError(t *testing.T) {
	s, _ := testStore(t)
	var logs bytes.Buffer
	ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)))
	original := errors.New("original failure")
	err := s.run(ctx, Policy{MaxSize: 1}, false, func(context.Context) error {
		if err := os.Remove(filepath.Join(s.Dir, "uses")); err != nil {
			t.Fatal(err)
		}
		putFile(t, filepath.Join(s.Dir, "uses"), "blocked maintenance")
		return original
	})
	if !errors.Is(err, original) || !strings.Contains(logs.String(), "maintenance incomplete") {
		t.Fatalf("error=%v logs=%s", err, logs.String())
	}
}

func TestCancellationStillCleansTemporaryWork(t *testing.T) {
	s, _ := testStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	err := s.run(ctx, Policy{}, false, func(context.Context) error {
		putFile(t, filepath.Join(s.Dir, "work", "cancelled"), "temporary")
		cancel()
		return ctx.Err()
	})
	usage, infoErr := s.Info(t.Context())
	if !errors.Is(err, context.Canceled) || infoErr != nil || usage.Work != 0 {
		t.Fatalf("cancellation cleanup: %+v / %v / %v", usage, err, infoErr)
	}
}

func TestRecencyFailurePreservesSuccessfulUse(t *testing.T) {
	s, now := testStore(t)
	ref := importObject(t, s, "cached installer")
	*now = now.Add(2 * Grace)
	// Another run prevents startup collection, then finishes during our command.
	release, err := s.Lease(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	err = s.run(t.Context(), Policy{MaxSize: 1}, false, func(ctx context.Context) error {
		if err := release(); err != nil {
			return err
		}
		marker := filepath.Join(s.Dir, "uses", "objects-"+ref.SHA256)
		if err := os.Remove(marker); err != nil {
			return err
		}
		if err := os.Mkdir(marker, 0o700); err != nil {
			return err
		}
		return s.Verify(ctx, ref)
	})
	if err != nil || !s.Has(ref) {
		t.Fatalf("recency failure lost a successful result: %v", err)
	}
}

func TestDanglingIndexesBecomeMissesAndUntrackedObjectsAreEligible(t *testing.T) {
	s, now := testStore(t)
	ref := importObject(t, s, "installer")
	descriptor := importObject(t, s, "descriptor")
	key := strings.Repeat("a", 64)
	if err := s.Remember(t.Context(), key, descriptor, ref); err != nil {
		t.Fatal(err)
	}
	if err := s.RememberSource(t.Context(), key, "metadata", ref.SHA256); err != nil {
		t.Fatal(err)
	}
	path, _ := s.Path(ref)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	var value string
	if s.RecallSource(t.Context(), key, &value) {
		t.Fatal("dangling source was a hit")
	}
	if _, hit := s.Recall(t.Context(), key); hit {
		t.Fatal("dangling derivation was a hit")
	}
	if _, err := s.Prune(t.Context(), Policy{MaxSize: DefaultMaxSize}, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "derivations", key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dangling index was retained")
	}
	// Missing recency is not permission to pin an object indefinitely.
	if err := os.Remove(filepath.Join(s.Dir, "uses", "objects-"+descriptor.SHA256)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * Grace)
	if _, err := s.Info(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if s.Has(descriptor) {
		t.Fatal("inventory protected an untracked object")
	}
}

func TestCacheLeaseProcess(t *testing.T) {
	dir := os.Getenv("STEMMA_TEST_LEASE")
	if dir == "" {
		return
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.Lease(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	putFile(t, filepath.Join(dir, "work", "interrupted-run"), "temporary workspace")
	putFile(t, filepath.Join(dir, "objects", ".import-interrupted"), "partial download")
	_, _ = os.Stdout.WriteString("leased\n")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestOverlappingProcessAndCrashRecovery(t *testing.T) {
	s, now := testStore(t)
	ref := importObject(t, s, "old installer")
	*now = now.Add(2 * Grace)
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCacheLeaseProcess$")
	child.Env = append(os.Environ(), "STEMMA_TEST_LEASE="+s.Dir)
	child.Stderr = os.Stderr
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "leased\n" {
		t.Fatalf("child readiness: %q / %v", line, err)
	}
	lockBefore, err := os.Stat(filepath.Join(s.Dir, "cache.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.run(t.Context(), Policy{MaxSize: 1}, false, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !s.Has(ref) {
		t.Fatal("automatic maintenance evicted during active process")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "work", "interrupted-run")); err != nil {
		t.Fatal("active workspace removed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Prune(ctx, Policy{}, PruneOptions{All: true}); !errors.Is(err, context.Canceled) {
		t.Fatalf("explicit waiting prune ignored cancellation: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if err := s.run(t.Context(), Policy{MaxSize: 1}, false, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	usage, err := s.Info(t.Context())
	if err != nil || usage != (Usage{}) {
		t.Fatalf("crash leftovers: %+v / %v", usage, err)
	}
	lockAfter, err := os.Stat(filepath.Join(s.Dir, "cache.lock"))
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("pruning replaced lock infrastructure")
	}
}

func TestPrunePreservesUnownedFilesAndDoesNotFollowLinks(t *testing.T) {
	s, _ := testStore(t)
	external := filepath.Join(t.TempDir(), "export.pkg")
	putFile(t, external, "user export")
	putFile(t, filepath.Join(s.Dir, "reconcile.json"), "state")
	putFile(t, filepath.Join(s.Dir, "stemma.lock.yaml"), "reviewed")
	if err := os.Symlink(filepath.Dir(external), filepath.Join(s.Dir, "work", "link")); err != nil {
		t.Skip(err)
	}
	if _, err := s.Prune(t.Context(), Policy{}, PruneOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{external, filepath.Join(s.Dir, "reconcile.json"), filepath.Join(s.Dir, "stemma.lock.yaml")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("prune touched unowned file", path, err)
		}
	}
}
