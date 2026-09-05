// Package cas owns immutable downloaded and derived objects in the disposable cache.
package cas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/fileio"
)

// MaxObjectSize bounds downloads and individual cache objects to 16 GiB.
const MaxObjectSize int64 = 16 << 30

// Ref binds an immutable object to its digest and byte length.
type Ref struct {
	SHA256 string `json:"sha256" yaml:"sha256"`
	Size   int64  `json:"size" yaml:"size"`
}

// Store contains content-addressed objects and disposable derivation indexes.
type Store struct{ Dir string }

// Open creates the cache directories. Call Lease while using cache objects.
func Open(dir string) (*Store, error) {
	if dir == "" {
		root, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		dir = filepath.Join(root, "stemma")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"objects", "work", "derivations"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{Dir: dir}, nil
}

// Lease prevents garbage collection while a run is active. OS locks release after crashes.
func (s *Store) Lease(ctx context.Context) (func() error, error) {
	l := flock.New(filepath.Join(s.Dir, "cache.lock"))
	ok, err := l.TryRLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	return l.Close, nil
}

// Path locates an object; callers must treat the returned file as read-only.
func (s *Store) Path(ref Ref) (string, error) {
	if !config.ValidDigest(ref.SHA256) || ref.Size < 0 || ref.Size > MaxObjectSize {
		return "", errors.New("invalid artifact reference")
	}
	return filepath.Join(s.Dir, "objects", ref.SHA256), nil
}

// Verify hashes actual stored bytes instead of trusting file existence or size.
func (s *Store) Verify(ctx context.Context, ref Ref) error {
	path, err := s.Path(ref)
	if err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(fileio.Reader{Context: ctx, Reader: f}, ref.Size+1))
	if err != nil {
		return err
	}
	if n != ref.Size || hex.EncodeToString(h.Sum(nil)) != ref.SHA256 {
		return fmt.Errorf("cache object %s failed integrity verification", ref.SHA256)
	}
	return nil
}

// Import streams bytes to a temporary object and atomically publishes the digest.
func (s *Store) Import(ctx context.Context, r io.Reader, expected string) (Ref, error) {
	if expected != "" && !config.ValidDigest(expected) {
		return Ref{}, errors.New("invalid expected SHA-256")
	}
	f, err := os.CreateTemp(filepath.Join(s.Dir, "objects"), ".import-*")
	if err != nil {
		return Ref{}, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(fileio.Reader{Context: ctx, Reader: r}, MaxObjectSize+1))
	if err == nil && n > MaxObjectSize {
		err = errors.New("artifact exceeds 16 GiB")
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return Ref{}, err
	}
	ref := Ref{SHA256: hex.EncodeToString(h.Sum(nil)), Size: n}
	if expected != "" && expected != ref.SHA256 {
		return Ref{}, fmt.Errorf("source integrity mismatch: expected %s, received %s; review the source before updating the lockfile", expected, ref.SHA256)
	}
	path, err := s.Path(ref)
	if err != nil {
		return Ref{}, err
	}
	if err := os.Chmod(f.Name(), 0o444); err != nil {
		return Ref{}, err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		if verifyErr := s.Verify(ctx, ref); verifyErr != nil {
			return Ref{}, err
		}
	}
	return ref, nil
}

// ImportFile imports a regular file into the cache without retaining caller ownership.
func (s *Store) ImportFile(ctx context.Context, path, expected string) (Ref, error) {
	f, err := os.Open(path)
	if err != nil {
		return Ref{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return Ref{}, err
	}
	if !info.Mode().IsRegular() {
		return Ref{}, errors.New("artifact must be a regular file")
	}
	return s.Import(ctx, f, expected)
}

// Materialize copies an object into a workspace, never exposing a writable hard link.
func (s *Store) Materialize(ctx context.Context, ref Ref, path string) error {
	if err := s.Verify(ctx, ref); err != nil {
		return err
	}
	source, err := s.Path(ref)
	if err != nil {
		return err
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, fileio.Reader{Context: ctx, Reader: in})
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Recall reads a derivation result only if the referenced object still verifies.
func (s *Store) Recall(ctx context.Context, key string) (Ref, bool) {
	if !config.ValidDigest(key) {
		return Ref{}, false
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, "derivations", key))
	if err != nil {
		return Ref{}, false
	}
	var ref Ref
	if json.Unmarshal(data, &ref) != nil || s.Verify(ctx, ref) != nil {
		return Ref{}, false
	}
	return ref, true
}

// Remember indexes a completed derivation. Configuration keys exclude destination metadata.
func (s *Store) Remember(key string, ref Ref) error {
	if !config.ValidDigest(key) {
		return errors.New("invalid derivation key")
	}
	data, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	return fileio.Write(filepath.Join(s.Dir, "derivations", key), data, 0o600)
}

// Prune removes disposable cache contents only when no run holds a lease.
func (s *Store) Prune(ctx context.Context) error {
	l := flock.New(filepath.Join(s.Dir, "cache.lock"))
	ok, err := l.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return ctx.Err()
	}
	defer func() { _ = l.Close() }()
	for _, sub := range []string{"objects", "work", "derivations"} {
		path := filepath.Join(s.Dir, sub)
		if err := os.RemoveAll(path); err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
	}
	return nil
}
