package plugins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oras-project/oras-go/v3/registry/remote/properties"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/source"
)

// Fingerprint identifies the declaration a lock entry answers.
func Fingerprint(declaration config.Plugin) string {
	return config.Fingerprint(struct {
		Image, Path, Entrypoint string
	}{declaration.Image, declaration.Path, declaration.Entrypoint})
}

// Pin names the code a declaration selects: the digest an image names, or the
// digest of the lock entry that answers the declaration. It is empty when the
// declaration is not locked.
func Pin(declaration config.Plugin, entry Entry) string {
	if _, digest, pinned := strings.Cut(declaration.Image, "@"); pinned {
		return digest
	}
	if entry.Declaration == Fingerprint(declaration) {
		return entry.Digest
	}
	return ""
}

// Load selects a plugin's code, installs it when the cache has not, and
// returns the lock entry that selects it. An image that names its digest needs
// no entry. A tag or local path uses the previous entry for its declaration;
// with resolve, a missing or stale entry is resolved instead: the tag from the
// registry, the path from its current files. Local files are hashed on every
// load, before any plugin code runs.
func (s *Store) Load(ctx context.Context, root string, declaration config.Plugin, previous Entry, resolve bool) (Bundle, Entry, error) {
	if err := declaration.Validate(); err != nil {
		return Bundle{}, Entry{}, err
	}
	fingerprint := Fingerprint(declaration)
	if previous.Declaration != fingerprint {
		previous = Entry{}
	}
	if declaration.Path == "" {
		ref, err := properties.NewReference(declaration.Image)
		if err != nil {
			return Bundle{}, Entry{}, err
		}
		if ref.Digest != "" {
			bundle, err := s.Acquire(ctx, declaration.Image, ref.Digest)
			return bundle, Entry{}, err
		}
		entry := previous
		if entry.Digest == "" {
			if !resolve {
				return Bundle{}, Entry{}, errors.New("image tag is not locked; run stemma plugins update")
			}
			digest, err := s.Resolve(ctx, declaration.Image)
			if err != nil {
				return Bundle{}, Entry{}, err
			}
			entry = Entry{Declaration: fingerprint, Digest: digest}
		}
		bundle, err := s.Acquire(ctx, declaration.Image, entry.Digest)
		return bundle, entry, err
	}
	path := declaration.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	current, err := snapshot(ctx, path)
	if err != nil {
		return Bundle{}, Entry{}, err
	}
	if !current.Tree && declaration.Entrypoint != "" {
		return Bundle{}, Entry{}, errors.New("entrypoint requires a directory; path already selects an executable")
	}
	entry := Entry{Declaration: fingerprint, Digest: "sha256:" + current.SHA256}
	switch {
	case resolve || entry == previous:
	case previous.Digest == "":
		return Bundle{}, Entry{}, errors.New("local files are not locked; run stemma plugins update")
	default:
		return Bundle{}, Entry{}, errors.New("local files changed since they were locked; run stemma plugins update")
	}
	name := declaration.Entrypoint
	if name == "" {
		name = entrypoint(s.platform.OS)
		if !current.Tree {
			name = current.Filename
		}
	}
	identity := config.Fingerprint(struct {
		Content    source.Content
		Entrypoint string
	}{current, name})
	executable, err := s.install(ctx, identity, name, func(dir string) error {
		return s.stageLocal(ctx, path, current, name, dir)
	})
	if err != nil {
		return Bundle{}, entry, err
	}
	return Bundle{Manifest: identity, Executable: executable}, entry, nil
}

// snapshot hashes the local file or directory at path as it is now: a file's
// bytes, or a directory's canonical archive.
func snapshot(ctx context.Context, path string) (source.Content, error) {
	f, err := os.Open(path)
	if err != nil {
		return source.Content{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return source.Content{}, err
	}
	content := source.Content{Filename: filepath.Base(path), Tree: info.IsDir(), Mode: uint32(info.Mode().Perm())}
	switch {
	case info.IsDir():
		content.SHA256, err = treeDigest(ctx, path)
	case info.Mode().IsRegular():
		if err := archive.CheckXattrs(ctx, f); err != nil {
			return source.Content{}, err
		}
		h := sha256.New()
		_, err = io.Copy(h, fileio.Reader{Context: ctx, Reader: f})
		content.SHA256 = hex.EncodeToString(h.Sum(nil))
	default:
		err = errors.New("plugin path is not a regular file or directory")
	}
	return content, err
}

// stageLocal copies the local files at path into the new directory dir. The
// copy must be the content the snapshot named, so an installation never holds
// files that changed after they were hashed.
func (s *Store) stageLocal(ctx context.Context, path string, content source.Content, name, dir string) error {
	h := sha256.New()
	if content.Tree {
		packed, err := os.CreateTemp(filepath.Join(s.cache.Dir, "work"), "plugin-*.tar")
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(packed.Name()) }()
		err = archive.Pack(ctx, path, io.MultiWriter(packed, h))
		if closeErr := packed.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = archive.Extract(ctx, packed.Name(), dir)
		}
		if err != nil {
			return err
		}
	} else {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = in.Close() }()
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(io.MultiWriter(out, h), fileio.Reader{Context: ctx, Reader: in})
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	if hex.EncodeToString(h.Sum(nil)) != content.SHA256 {
		return errors.New("local files changed while they were installed; run again")
	}
	return nil
}
