package archive

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
	"github.com/mholt/archives"
	"golang.org/x/text/unicode/norm"
)

// IndexTar borrows canonical tree bytes produced by Pack. The caller verifies
// their digest and retains the reader for the filesystem's lifetime. Indexing
// reads headers and signature sidecars; files are bounded views into the reader.
// rootName and mode describe the directory omitted by the canonical TAR.
func IndexTar(ctx context.Context, reader io.ReaderAt, size int64, rootName string, mode fs.FileMode) (fs.ReadLinkFS, error) {
	if !fs.ValidPath(rootName) || rootName == "." || strings.ContainsAny(rootName, "/\x00") || size < 1024 || size > maxBytes || mode != mode.Perm() {
		return nil, errors.New("invalid canonical tree root or size")
	}
	reader = tarReaderAt{ReaderAt: reader, ctx: ctx}
	stream := io.NewSectionReader(reader, 0, size)
	tr := tar.NewReader(stream)
	tree := &tarFS{reader: reader, nodes: map[string]*tarNode{}, root: "."}
	tree.nodes["."] = &tarNode{name: ".", mode: fs.ModeDir | 0o755}
	tree.nodes[rootName] = &tarNode{name: rootName, mode: fs.ModeDir | mode}
	tree.nodes["."].children = []fs.DirEntry{fs.FileInfoToDirEntry(tree.nodes[rootName])}
	spellings := map[string]string{}
	var total, metadata int64
	for {
		before, _ := stream.Seek(0, io.SeekCurrent)
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			offset, _ := stream.Seek(0, io.SeekCurrent)
			if offset-before != 1024 || offset != size {
				return nil, errors.New("invalid canonical tree terminator")
			}
			break
		}
		if err != nil {
			return nil, fmt.Errorf("index tree: %w", err)
		}
		offset, _ := stream.Seek(0, io.SeekCurrent)
		metadata += offset - before
		if len(tree.nodes)-2 >= maxEntries || metadata > 128<<20 {
			return nil, errors.New("canonical tree exceeds metadata limit")
		}
		name, err := safeName(header.Name)
		if err != nil || name == "." {
			return nil, fmt.Errorf("invalid canonical tree entry %q", header.Name)
		}
		folded := strings.ToLower(norm.NFD.String(name))
		if prior, exists := spellings[folded]; exists {
			return nil, fmt.Errorf("duplicate or case-conflicting tree paths %q and %q", prior, name)
		}
		spellings[folded] = name
		full := path.Join(rootName, name)
		parent := tree.nodes[path.Dir(full)]
		if parent == nil || !parent.IsDir() {
			return nil, fmt.Errorf("canonical tree entry %q requires its directory first", name)
		}
		for key := range header.PAXRecords {
			if key != "path" && key != "linkpath" && key != "size" {
				return nil, fmt.Errorf("unsupported canonical tree attribute %q", key)
			}
		}
		if header.Mode < 0 || header.Mode > 0o777 || header.Size < 0 || header.Size > maxBytes-total || header.Size > size-offset {
			return nil, fmt.Errorf("unsupported mode or size for tree entry %q", name)
		}
		node := &tarNode{name: path.Base(name), mode: fs.FileMode(header.Mode), size: header.Size, offset: offset, target: header.Linkname}
		switch header.Typeflag {
		case tar.TypeReg:
			total += header.Size
		case tar.TypeDir:
			node.mode |= fs.ModeDir
		case tar.TypeSymlink:
			if err := checkLink(name, header.Linkname); err != nil {
				return nil, err
			}
			node.mode |= fs.ModeSymlink
		default:
			return nil, fmt.Errorf("unsupported canonical tree entry %q", name)
		}
		if !node.mode.IsRegular() && header.Size != 0 {
			return nil, fmt.Errorf("non-file tree entry %q has content", name)
		}
		if appleDouble(name) && node.mode.IsRegular() {
			entry := archives.FileInfo{FileInfo: node, NameInArchive: name, Open: func() (fs.File, error) {
				return &tarFile{SectionReader: io.NewSectionReader(reader, offset, header.Size), node: node}, nil
			}}
			if err := sidecarSignature(entry); err != nil {
				return nil, err
			}
		}
		tree.nodes[full] = node
		parent.children = append(parent.children, fs.FileInfoToDirEntry(node))
		// The canonical format has no sparse entries. Skip payloads without
		// reading them; the lease's digest covers every byte independently.
		if _, err := stream.Seek(offset+header.Size+(512-header.Size%512)%512, io.SeekStart); err != nil {
			return nil, err
		}
		tr = tar.NewReader(stream)
	}
	for _, node := range tree.nodes {
		slices.SortFunc(node.children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	}
	return tree, ctx.Err()
}

type tarReaderAt struct {
	io.ReaderAt

	ctx context.Context
}

func (r tarReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReaderAt.ReadAt(p, offset)
}

type tarFS struct {
	reader io.ReaderAt
	nodes  map[string]*tarNode
	root   string
}

func (t *tarFS) lookup(name string, follow bool) (*tarNode, string, error) {
	if !fs.ValidPath(name) {
		return nil, "", fs.ErrInvalid
	}
	current := t.root
	pending := strings.Split(name, "/")
	links := 0
	for len(pending) > 0 {
		if !t.nodes[current].IsDir() {
			return nil, "", fs.ErrInvalid
		}
		part := pending[0]
		pending = pending[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if current == t.root {
				return nil, "", fs.ErrPermission
			}
			current = path.Dir(current)
			continue
		}
		next := path.Join(current, part)
		node := t.nodes[next]
		if node == nil {
			return nil, "", fs.ErrNotExist
		}
		if node.mode&fs.ModeSymlink != 0 && (follow || len(pending) > 0) {
			if links++; links > 40 {
				return nil, "", errors.New("too many symlinks")
			}
			// Resolve target components before cleaning any "..": an earlier
			// component can itself be a link to a different directory depth.
			pending = append(strings.Split(node.target, "/"), pending...)
			continue
		}
		current = next
	}
	return t.nodes[current], current, nil
}

func (t *tarFS) Open(name string) (fs.File, error) {
	node, _, err := t.lookup(name, true)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return &tarFile{SectionReader: io.NewSectionReader(t.reader, node.offset, node.size), node: node}, nil
}

func (t *tarFS) Lstat(name string) (fs.FileInfo, error) {
	node, _, err := t.lookup(name, false)
	if err != nil {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	return node, nil
}

func (t *tarFS) ReadLink(name string) (string, error) {
	node, _, err := t.lookup(name, false)
	if err == nil && node.mode&fs.ModeSymlink == 0 {
		err = fs.ErrInvalid
	}
	if err != nil {
		return "", &fs.PathError{Op: "readlink", Path: name, Err: err}
	}
	return node.target, nil
}

func (t *tarFS) Sub(name string) (fs.FS, error) {
	node, full, err := t.lookup(name, false)
	if err == nil && !node.IsDir() {
		err = fs.ErrInvalid
	}
	if err != nil {
		return nil, &fs.PathError{Op: "sub", Path: name, Err: err}
	}
	return &tarFS{reader: t.reader, nodes: t.nodes, root: full}, nil
}

// Canonical trees contain no extended attributes. Pack rejects native code
// signatures in xattrs; indexing also rejects signatures in PAX or AppleDouble.
func (t *tarFS) XattrValues(name string) (map[string]appledouble.Value, error) {
	_, err := t.Lstat(name)
	return nil, err
}

type tarNode struct {
	name         string
	mode         fs.FileMode
	size, offset int64
	target       string
	children     []fs.DirEntry
}

func (n *tarNode) Name() string       { return n.name }
func (n *tarNode) Size() int64        { return n.size }
func (n *tarNode) Mode() fs.FileMode  { return n.mode }
func (n *tarNode) ModTime() time.Time { return time.Unix(0, 0) }
func (n *tarNode) IsDir() bool        { return n.mode.IsDir() }
func (n *tarNode) Sys() any           { return nil }

type tarFile struct {
	*io.SectionReader

	node     *tarNode
	position int
}

func (f *tarFile) Stat() (fs.FileInfo, error) { return f.node, nil }
func (f *tarFile) Close() error               { return nil }
func (f *tarFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if !f.node.IsDir() {
		return nil, fs.ErrInvalid
	}
	if n > 0 && f.position == len(f.node.children) {
		return nil, io.EOF
	}
	end := len(f.node.children)
	if n > 0 {
		end = min(end, f.position+n)
	}
	entries := slices.Clone(f.node.children[f.position:end])
	f.position = end
	return entries, nil
}
