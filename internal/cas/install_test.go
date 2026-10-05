package cas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/sync/errgroup"
)

func TestPluginInstallationsStageOnceAndAreCollected(t *testing.T) {
	s, now := testStore(t)
	key, interrupted := strings.Repeat("a", 64), strings.Repeat("b", 64)
	dir, err := s.Install(t.Context(), key, func(dir string) error {
		putFile(t, filepath.Join(dir, "plugin"), "executable")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "plugin")); err != nil || string(data) != "executable" {
		t.Fatalf("installed %q: %v", data, err)
	}
	again, err := s.Install(t.Context(), key, func(string) error {
		return errors.New("staged a complete installation again")
	})
	if err != nil || again != dir {
		t.Fatalf("second install = %s: %v", again, err)
	}

	failed := errors.New("bundle is unreadable")
	_, err = s.Install(t.Context(), interrupted, func(dir string) error {
		putFile(t, filepath.Join(dir, "plugin"), "partial")
		return failed
	})
	if !errors.Is(err, failed) {
		t.Fatalf("failed stage: %v", err)
	}
	if _, ok := s.Installed(t.Context(), interrupted); ok {
		t.Fatal("failed install is used")
	}
	// A crash leaves files without the completion marker.
	putFile(t, filepath.Join(s.Dir, "plugins", interrupted, "files", "plugin"), "partial")
	if _, ok := s.Installed(t.Context(), interrupted); ok {
		t.Fatal("interrupted install is used")
	}
	usage, err := s.Info(t.Context())
	if err != nil || usage.Materialized != int64(len("executable")+len("partial")) {
		t.Fatalf("usage: %+v / %v", usage, err)
	}
	if _, err := s.Prune(t.Context(), Policy{MaxSize: DefaultMaxSize}, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "plugins", interrupted)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted install was retained: %v", err)
	}

	*now = now.Add(2 * Grace)
	if _, ok := s.Installed(t.Context(), key); !ok {
		t.Fatal("prune removed a complete installation")
	}
	if result, err := s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{}); err != nil || result.Removed != 0 {
		t.Fatalf("a load did not protect its installation: %+v / %v", result, err)
	}
	*now = now.Add(2 * Grace)
	if _, err := s.Prune(t.Context(), Policy{MaxSize: 1}, PruneOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Installed(t.Context(), key); ok {
		t.Fatal("unused installation survived eviction")
	}
	usage, err = s.Info(t.Context())
	if err != nil || usage != (Usage{}) {
		t.Fatalf("eviction leftovers: %+v / %v", usage, err)
	}
}

func TestConcurrentPluginInstallation(t *testing.T) {
	s, _ := testStore(t)
	key := strings.Repeat("c", 64)
	var stages atomic.Int32
	var group errgroup.Group
	start := make(chan struct{})
	for range 8 {
		group.Go(func() error {
			<-start
			dir, err := s.Install(t.Context(), key, func(dir string) error {
				stages.Add(1)
				if err := os.Mkdir(dir, 0o700); err != nil {
					return err
				}
				return os.WriteFile(filepath.Join(dir, "plugin"), []byte("complete"), 0o600)
			})
			if err != nil {
				return err
			}
			data, err := os.ReadFile(filepath.Join(dir, "plugin"))
			if err != nil {
				return err
			}
			if string(data) != "complete" {
				return errors.New("installation returned incomplete files")
			}
			return nil
		})
	}
	close(start)
	if err := group.Wait(); err != nil {
		t.Fatal(err)
	}
	if stages.Load() != 1 {
		t.Fatalf("staged %d times, want once", stages.Load())
	}
}

func TestCanceledInstallPreservesActiveInstallation(t *testing.T) {
	s, _ := testStore(t)
	key := strings.Repeat("d", 64)
	staged, finish := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := s.Install(t.Context(), key, func(dir string) error {
			if err := os.Mkdir(dir, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte("complete"), 0o600); err != nil {
				return err
			}
			close(staged)
			<-finish
			return nil
		})
		result <- err
	}()
	select {
	case <-staged:
	case err := <-result:
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := s.Install(ctx, key, func(string) error { return errors.New("canceled caller staged files") })
	_, visible := s.Installed(t.Context(), key)
	close(finish)
	if firstErr := <-result; firstErr != nil {
		t.Fatal(firstErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("install error = %v, want cancellation", err)
	}
	if visible {
		t.Fatal("unfinished installation was visible")
	}
	dir, ok := s.Installed(t.Context(), key)
	if !ok {
		t.Fatal("active installation was lost")
	}
	data, err := os.ReadFile(filepath.Join(dir, "plugin"))
	if err != nil || string(data) != "complete" {
		t.Fatalf("installed files = %q: %v", data, err)
	}
}
