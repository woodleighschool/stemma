package archive

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/treefs"
	"golang.org/x/text/unicode/norm"
)

// Tree owns an extracted archive and its logical extended attributes. Metadata
// stays outside the payload, without depending on the host's xattr support.
// Borrowed attribute values remain valid until Close.
type Tree struct {
	fs.ReadLinkFS

	root       *os.Root
	storage    *os.File
	workspace  string
	attributes map[string]*fileAttributes
	bytes      int64
	count      int
}

type fileAttributes struct {
	owner  string
	values map[string]appledouble.Value
}

// Open extracts an archive into a temporary tree owned by the caller. AppleDouble
// and PAX attributes are retained for consumers that can preserve them.
func Open(ctx context.Context, input, workspace string) (_ *Tree, err error) {
	dir, err := os.MkdirTemp(workspace, ".contents-*")
	if err != nil {
		return nil, err
	}
	tree := &Tree{workspace: dir, attributes: map[string]*fileAttributes{}}
	defer func() {
		if err != nil {
			_ = tree.Close()
		}
	}()
	tree.storage, err = os.CreateTemp(dir, ".metadata-*")
	if err != nil {
		return nil, err
	}
	payload := filepath.Join(dir, "tree")
	if err := extractArchive(ctx, input, payload, tree); err != nil {
		return nil, err
	}
	tree.root, err = os.OpenRoot(payload)
	if err != nil {
		return nil, err
	}
	tree.ReadLinkFS = treefs.Local(tree.root)
	for _, attrs := range tree.attributes {
		name := attrs.owner
		if _, err := tree.Lstat(name); err != nil {
			return nil, fmt.Errorf("metadata owner %s: %w", name, err)
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			info, err := tree.Lstat(parent)
			if err != nil {
				return nil, err
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("metadata owner %s traverses a symlink or nondirectory", name)
			}
		}
	}
	return tree, nil
}

// Name returns the materialized payload directory, excluding metadata storage.
func (t *Tree) Name() string { return t.root.Name() }

func (t *Tree) ReadLink(name string) (string, error) {
	target, err := t.ReadLinkFS.ReadLink(name)
	return filepath.ToSlash(target), err
}

func (t *Tree) XattrValues(name string) (map[string]appledouble.Value, error) {
	if _, err := t.Lstat(name); err != nil {
		return nil, err
	}
	attrs := t.attributes[norm.NFD.String(name)]
	if attrs == nil {
		return nil, nil
	}
	return maps.Clone(attrs.values), nil
}

func (t *Tree) Close() error {
	var err error
	if t.root != nil {
		err = t.root.Close()
		t.root = nil
	}
	if t.storage != nil {
		err = errors.Join(err, t.storage.Close())
		t.storage = nil
	}
	return errors.Join(err, os.RemoveAll(t.workspace))
}

func (t *Tree) store(ctx context.Context, r io.Reader, size int64) (appledouble.Value, error) {
	if size < 0 || size > maxBytes-t.bytes {
		return nil, errors.New("archive metadata exceeds size limit")
	}
	start := t.bytes
	n, err := io.Copy(t.storage, io.LimitReader(fileio.Reader{Context: ctx, Reader: r}, size+1))
	t.bytes += n
	if err != nil {
		return nil, err
	}
	if n != size {
		return nil, errors.New("archive metadata length mismatch")
	}
	return io.NewSectionReader(t.storage, start, size), nil
}

func (t *Tree) add(ctx context.Context, owner, name string, value appledouble.Value) error {
	if _, err := safeName(owner); err != nil {
		return err
	}
	if name == "" || strings.ContainsRune(name, 0) {
		return errors.New("invalid archive attribute name")
	}
	if t.count >= maxEntries {
		return errors.New("archive exceeds metadata entry limit")
	}
	attrs := t.forOwner(owner).values
	if value == nil {
		value = bytes.NewReader(nil)
	}
	t.count++
	if prior, exists := attrs[name]; exists {
		equal, err := equalValues(ctx, prior, value)
		if err != nil {
			return err
		}
		if !equal {
			return fmt.Errorf("conflicting archive attribute %s on %s", name, owner)
		}
		return nil
	}
	attrs[name] = value
	return nil
}

func (t *Tree) sidecar(ctx context.Context, name string, content io.Reader, size int64) (bool, io.Reader, error) {
	r := bufio.NewReader(content)
	magic, err := r.Peek(8)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, nil, err
	}
	if !appledouble.Sniff(magic) {
		return false, r, nil
	}
	owner, _ := appledouble.OwnerName(strings.TrimPrefix(name, "__MACOSX/"))
	if _, err := safeName(owner); err != nil {
		return true, nil, err
	}
	if size > maxSidecar {
		return true, nil, errors.New("AppleDouble sidecar exceeds size limit")
	}
	if t.count >= maxEntries {
		return true, nil, errors.New("archive exceeds metadata entry limit")
	}
	t.count++
	t.forOwner(owner)
	value, err := t.store(ctx, r, size)
	if err != nil {
		return true, nil, err
	}
	decoded, err := appledouble.DecodeStream(ctx, value, appledouble.StreamLimits{MaxFileBytes: maxSidecar, MaxValueBytes: maxSidecar, MaxTotalValueBytes: maxSidecar})
	if err != nil {
		return true, nil, fmt.Errorf("AppleDouble sidecar %s: %w", name, err)
	}
	for _, attr := range decoded.Attrs {
		if err := t.add(ctx, owner, attr.Name, attr.Value); err != nil {
			return true, nil, err
		}
	}
	if decoded.FinderInfo != [32]byte{} {
		if err := t.add(ctx, owner, appledouble.FinderInfoName, bytes.NewReader(decoded.FinderInfo[:])); err != nil {
			return true, nil, err
		}
	}
	if decoded.ResourceFork != nil && decoded.ResourceFork.Size() > 0 {
		if err := t.add(ctx, owner, appledouble.ResourceForkName, decoded.ResourceFork); err != nil {
			return true, nil, err
		}
	}
	return true, nil, nil
}

func (t *Tree) pax(ctx context.Context, owner string, records map[string]string) error {
	for key, data := range records {
		name, ok := strings.CutPrefix(key, "SCHILY.xattr.")
		if !ok {
			name, ok = strings.CutPrefix(key, "LIBARCHIVE.xattr.")
			if !ok {
				continue
			}
			var err error
			name, err = url.PathUnescape(name)
			if err != nil {
				return fmt.Errorf("PAX attribute name %s: %w", key, err)
			}
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				decoded, err = base64.RawStdEncoding.DecodeString(data)
			}
			if err != nil {
				return fmt.Errorf("PAX attribute %s: %w", key, err)
			}
			data = string(decoded)
		}
		value, err := t.store(ctx, strings.NewReader(data), int64(len(data)))
		if err != nil {
			return err
		}
		if err := t.add(ctx, strings.TrimSuffix(owner, "/"), name, value); err != nil {
			return err
		}
	}
	return nil
}

// Archives may encode the same attribute through both PAX conventions.
func equalValues(ctx context.Context, a, b appledouble.Value) (bool, error) {
	if a.Size() != b.Size() {
		return false, nil
	}
	var left, right [32 << 10]byte
	for offset := int64(0); offset < a.Size(); {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		size := min(int64(len(left)), a.Size()-offset)
		if _, err := io.ReadFull(io.NewSectionReader(a, offset, size), left[:size]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(io.NewSectionReader(b, offset, size), right[:size]); err != nil {
			return false, err
		}
		if !bytes.Equal(left[:size], right[:size]) {
			return false, nil
		}
		offset += size
	}
	return true, nil
}

// HFS+ normalizes names when materializing them. Keep attributes associated with
// that same logical path even when the host returns a different Unicode form.
func (t *Tree) forOwner(name string) *fileAttributes {
	key := norm.NFD.String(name)
	attrs := t.attributes[key]
	if attrs == nil {
		attrs = &fileAttributes{owner: name, values: map[string]appledouble.Value{}}
		t.attributes[key] = attrs
	}
	return attrs
}

func (t *Tree) Sub(name string) (fs.FS, error) { return fs.Sub(t.ReadLinkFS, name) }
