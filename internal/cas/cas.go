// Package cas owns immutable downloaded and derived objects in the disposable cache.
package cas

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

// MaxObjectSize bounds downloads and individual cache objects to 16 GiB.
const MaxObjectSize int64 = 16 << 30

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

// Ref binds an immutable object to its digest and byte length.
type Ref struct {
	SHA256 string `json:"sha256" yaml:"sha256"`
	Size   int64  `json:"size" yaml:"size"`
}

// Store contains content-addressed objects and disposable source and
// derivation indexes.
type Store struct {
	Dir string
	now func() time.Time
}

// dirs are the cache's disposable directories. Materialized copies of prepared
// artifacts are for the operator; no run reads them back. Plugin installations
// are executed in place.
var dirs = []string{"objects", "work", "sources", "derivations", "materialized", "plugins", "uses"}

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
	// #nosec G703 -- The operator explicitly selects the cache root; subdirectories are fixed.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	for _, sub := range dirs {
		if err := root.MkdirAll(sub, 0o700); err != nil {
			return nil, err
		}
		info, err := root.Lstat(sub)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("cache directory %s is not a directory", sub)
		}
	}
	return &Store{Dir: dir, now: time.Now}, nil
}

// Lease prevents garbage collection while a run is active. OS locks release after crashes.
func (s *Store) Lease(ctx context.Context) (release func() error, err error) {
	l := flock.New(filepath.Join(s.Dir, "cache.lock"))
	ok, err := l.TryRLock()
	if err == nil && !ok {
		done := plugin.Stage(ctx, "Waiting for cache prune")
		ok, err = l.TryRLockContext(ctx, 50*time.Millisecond)
		done(err)
	}
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
	if !validDigest(ref.SHA256) || ref.Size < 0 || ref.Size > MaxObjectSize {
		return "", errors.New("invalid artifact reference")
	}
	return filepath.Join(s.Dir, "objects", ref.SHA256), nil
}

// Lookup describes a stored object without certifying its bytes. Read verifies
// bytes when a consumer needs them.
func (s *Store) Lookup(digest string) (Ref, error) {
	ref := Ref{SHA256: digest}
	path, err := s.Path(ref)
	if err != nil {
		return Ref{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return Ref{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxObjectSize {
		return Ref{}, errors.New("invalid cached object")
	}
	ref.Size = info.Size()
	return ref, nil
}

// HasDigest reports availability, not integrity. Consumers must verify bytes.
func (s *Store) HasDigest(digest string) bool {
	_, err := s.Lookup(digest)
	return err == nil
}

// Reuse records metadata-only reuse of an available object, such as an unchanged
// source observation. It does not certify bytes; consumers still call Verify.
func (s *Store) Reuse(ctx context.Context, digest string) bool {
	if !s.HasDigest(digest) {
		return false
	}
	s.touch(ctx, "objects", digest)
	return true
}

// Verify hashes actual stored bytes instead of trusting file existence or size.
func (s *Store) Verify(ctx context.Context, ref Ref) error {
	return s.Read(ctx, ref, func(r io.Reader) error { _, err := io.Copy(io.Discard, r); return err })
}

// Read verifies the exact stream consumed by read, including any unread tail.
// The caller must keep derived outputs private until Read succeeds.
// An object that fails integrity verification is discarded so a later run can
// acquire or rebuild it. Consumer errors and cancellation do not discard it.
func (s *Store) Read(ctx context.Context, ref Ref, read func(io.Reader) error) error {
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
	r := &io.LimitedReader{R: fileio.Reader{Context: ctx, Reader: f}, N: ref.Size + 1}
	stream := io.TeeReader(bufio.NewReaderSize(r, 1<<20), h)
	err = read(stream)
	if err == nil {
		_, err = io.Copy(io.Discard, stream)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return err
	}
	if r.N != 1 || hex.EncodeToString(h.Sum(nil)) != ref.SHA256 {
		failure := fmt.Errorf("cache object %s failed integrity verification", ref.SHA256)
		if err := f.Close(); err != nil {
			return errors.Join(failure, err)
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.Join(failure, err)
		}
		return fmt.Errorf("%w; discarded corrupt content, retry to acquire or rebuild it", failure)
	}
	s.touch(ctx, "objects", ref.SHA256)
	return nil
}

// Has reports whether an object is stored at its recorded size. It reads no
// bytes; Verify checks them before anything uses the object.
func (s *Store) Has(ref Ref) bool {
	path, err := s.Path(ref)
	if err != nil {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() == ref.Size
}

// Import streams bytes to a temporary object and atomically publishes the digest.
func (s *Store) Import(ctx context.Context, r io.Reader, expected string) (Ref, error) {
	return s.Write(ctx, expected, func(w io.Writer) error {
		_, err := io.Copy(w, r)
		return err
	})
}

// Write produces an object directly into private CAS storage. Failed producers,
// cancellations and digest mismatches never publish the object.
func (s *Store) Write(ctx context.Context, expected string, write func(io.Writer) error) (Ref, error) {
	if expected != "" && !validDigest(expected) {
		return Ref{}, errors.New("invalid expected SHA-256")
	}
	f, err := os.CreateTemp(filepath.Join(s.Dir, "objects"), ".import-*")
	if err != nil {
		return Ref{}, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	buffer := bufio.NewWriterSize(f, 1<<20)
	ref, err := Digest(ctx, func(w io.Writer) error { return write(io.MultiWriter(w, buffer)) })
	if err == nil {
		err = buffer.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return Ref{}, err
	}
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
	s.touch(ctx, "objects", ref.SHA256)
	return ref, nil
}

// Capture stores content that write can produce again, such as a local file
// or tree read on every run. It measures the content and writes it only when
// the store lacks it; consumers verify stored bytes, as for any object.
func (s *Store) Capture(ctx context.Context, expected string, write func(io.Writer) error) (Ref, error) {
	ref, err := Digest(ctx, write)
	if err != nil {
		return Ref{}, err
	}
	if (expected == "" || expected == ref.SHA256) && s.Has(ref) {
		s.touch(ctx, "objects", ref.SHA256)
		return ref, nil
	}
	// Write checks the expected digest, and the measured one when none was
	// expected, so content that changes between the passes is not stored.
	return s.Write(ctx, cmp.Or(expected, ref.SHA256), write)
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".materialize-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(out.Name()) }()
	err = s.Read(ctx, ref, func(r io.Reader) error {
		_, err := io.Copy(out, r)
		return err
	})
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return fileio.RenameExclusive(out.Name(), path)
}

// Adopt stores a regular file its caller wrote and will not change again by
// linking it into the store, so its bytes are not written twice. Windows keeps
// the read-only attribute on the file rather than the link, so there the file
// is copied instead.
func (s *Store) Adopt(ctx context.Context, path string) (Ref, error) {
	if runtime.GOOS == "windows" {
		return s.ImportFile(ctx, path, "")
	}
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
	ref, err := Digest(ctx, func(w io.Writer) error {
		_, err := io.Copy(w, f)
		return err
	})
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		return Ref{}, err
	}
	object, err := s.Path(ref)
	if err != nil {
		return Ref{}, err
	}
	if err := os.Chmod(path, 0o444); err != nil {
		return Ref{}, err
	}
	// Link under a private name, then replace any object already stored, as
	// Write does.
	temporary := filepath.Join(s.Dir, "objects", ".import-"+rand.Text())
	if err := os.Link(path, temporary); err != nil {
		return Ref{}, err
	}
	defer func() { _ = os.Remove(temporary) }()
	if err := os.Rename(temporary, object); err != nil {
		return Ref{}, err
	}
	s.touch(ctx, "objects", ref.SHA256)
	return ref, nil
}

// Lend verifies an object and links it at path, for Stemma's own code to read
// in place. The link is the object, so nothing may write through it. Windows
// keeps the read-only attribute on the file rather than the link, and removing
// a link there would leave the object writable; it gets a copy instead.
func (s *Store) Lend(ctx context.Context, ref Ref, path string) error {
	if runtime.GOOS == "windows" {
		return s.Materialize(ctx, ref, path)
	}
	if err := s.Verify(ctx, ref); err != nil {
		return err
	}
	object, err := s.Path(ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.Link(object, path)
}

// Digest measures a canonical byte stream without storing another object.
func Digest(ctx context.Context, write func(io.Writer) error) (Ref, error) {
	h := sha256.New()
	w := &boundedWriter{writer: fileio.Writer{Context: ctx, Writer: h}}
	if err := write(w); err != nil {
		return Ref{}, err
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, err
	}
	return Ref{SHA256: hex.EncodeToString(h.Sum(nil)), Size: w.size}, nil
}

type boundedWriter struct {
	writer io.Writer
	size   int64
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > MaxObjectSize-w.size {
		return 0, errors.New("artifact exceeds 16 GiB")
	}
	n, err := w.writer.Write(p)
	w.size += int64(n)
	return n, err
}
