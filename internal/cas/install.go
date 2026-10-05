package cas

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

// Installed returns a completed plugin installation and records its use.
func (s *Store) Installed(ctx context.Context, key string) (string, bool) {
	if !validDigest(key) {
		return "", false
	}
	root := filepath.Join(s.Dir, "plugins", key)
	if _, err := os.Lstat(filepath.Join(root, "complete")); err != nil {
		return "", false
	}
	s.touch(ctx, "plugins", key)
	return filepath.Join(root, "files"), true
}

// Install reuses a completed installation or calls stage to create its files.
// Callers must hold a cache lease. Installs of the same key are serialized;
// a completion marker makes staged files available only after success.
// Files stay in place because Windows scanners can prevent directory renames
// while holding a newly created executable open.
func (s *Store) Install(ctx context.Context, key string, stage func(dir string) error) (string, error) {
	if dir, ok := s.Installed(ctx, key); ok {
		return dir, nil
	}
	if !validDigest(key) {
		return "", errors.New("invalid plugin installation key")
	}
	root := filepath.Join(s.Dir, "plugins", key)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", err
	}
	l := flock.New(filepath.Join(root, "lock"))
	defer func() { _ = l.Close() }()
	if _, err := l.TryLockContext(ctx, 50*time.Millisecond); err != nil {
		return "", err
	}
	if dir, ok := s.Installed(ctx, key); ok {
		return dir, nil
	}
	dir := filepath.Join(root, "files")
	// An install that stopped partway left files without the marker.
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if err := stage(dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "complete"), nil, 0o600); err != nil {
		return "", err
	}
	s.touch(ctx, "plugins", key)
	return dir, nil
}
