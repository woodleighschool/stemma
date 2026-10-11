package apple

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/deploymenttheory/go-macos-pkg/pkg/xar"
)

var errMemberWritten = errors.New("member written")

// ReadPackageMember writes the file at name inside a flat package to w. name is
// an archive entry, such as Distribution or Component.pkg/PackageInfo, or a
// path below a component's Scripts or Payload archive, such as
// Component.pkg/Scripts/postinstall. A directory or one of those archives
// writes the names it holds, one a line, and "." names the package itself.
//
// A file below Scripts or Payload is found by decoding that archive up to it.
func ReadPackageMember(ctx context.Context, r io.ReaderAt, size int64, name string, w io.Writer) error {
	if size < 0 || size > maxEntrySize {
		return fmt.Errorf("PKG exceeds size limit")
	}
	archive, err := openXAR(contextReaderAt{ctx, r}, size)
	if err != nil {
		return err
	}
	file := archive.files[name]
	switch {
	case name == "." || file != nil && file.IsDir():
		return WriteNames(w, archive.children(name))
	case file != nil && !archive.packed(name):
		return archive.readEntry(name, w, maxEntrySize)
	}
	for entry := name; entry != "."; entry = path.Dir(entry) {
		if archive.packed(entry) {
			return archive.readPacked(ctx, entry, strings.TrimPrefix(strings.TrimPrefix(name, entry), "/"), w)
		}
	}
	dir, holder := ".", "the package"
	for part := range strings.SplitSeq(name, "/") {
		if archive.files[path.Join(dir, part)] == nil {
			return NoMember(holder, part, archive.children(dir))
		}
		dir = path.Join(dir, part)
		holder = dir
	}
	return fmt.Errorf("%s is not a regular file or directory", name)
}

// packed reports whether an entry is a component's Scripts or Payload archive.
func (a *xarArchive) packed(name string) bool {
	file := a.files[name]
	base := path.Base(name)
	return file != nil && file.Type.Value == xar.TypeFile && (base == "Payload" || base == "Scripts")
}

func (a *xarArchive) children(dir string) []string {
	var names []string
	for _, entry := range a.entries {
		if path.Dir(entry.Path) != dir {
			continue
		}
		name := path.Base(entry.Path)
		if entry.Type == xar.TypeDirectory || a.packed(entry.Path) {
			name += "/"
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func (a *xarArchive) readPacked(ctx context.Context, entry, inner string, w io.Writer) error {
	source, err := a.openEntry(entry, maxEntrySize)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	want := cmp.Or(inner, ".")
	// held lists what the wanted path and each directory above it hold, so a
	// miss names the first component that is absent.
	held := map[string]map[string]bool{".": {}}
	for dir := want; dir != "."; dir = path.Dir(dir) {
		held[dir] = map[string]bool{}
	}
	directories := map[string]bool{".": true}
	budget := newPayloadBudget()
	err = streamPayload(ctx, io.NopCloser(source), budget, func(content io.Reader) error {
		entries := newCPIOReader(content, budget)
		for {
			file, err := entries.next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			directory := file.kind() == cpioDirectory
			switch {
			case file.name == want && file.kind() == cpioRegular:
				if _, err := io.CopyN(w, content, file.size); err != nil {
					return fmt.Errorf("CPIO %s: %w", file.name, err)
				}
				// The rest of the archive is not decoded.
				return errMemberWritten
			case file.name == want && !directory:
				return fmt.Errorf("%s is not a regular file or directory", file.name)
			}
			// Some packagers write no entries for directories, so a name also
			// implies every directory above it.
			for name := file.name; name != "."; name = path.Dir(name) {
				if _, watched := held[name]; watched && directory {
					directories[name] = true
				}
				if names, watched := held[path.Dir(name)]; watched {
					label := path.Base(name)
					if directory {
						label += "/"
					}
					names[label] = true
				}
				directory = true
			}
			if err := entries.skip(file); err != nil {
				return err
			}
		}
	})
	if errors.Is(err, errMemberWritten) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s: %w", entry, err)
	}
	dir := "."
	for part := range strings.SplitSeq(want, "/") {
		next := path.Join(dir, part)
		if !directories[next] {
			return NoMember(path.Join(entry, dir), part, slices.Sorted(maps.Keys(held[dir])))
		}
		dir = next
	}
	return WriteNames(w, slices.Sorted(maps.Keys(held[want])))
}

// WriteNames writes a directory listing, one name a line.
func WriteNames(w io.Writer, names []string) error {
	for _, name := range names {
		if _, err := io.WriteString(w, name+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// NoMember reports a name that holder lacks, with the names it holds.
func NoMember(holder, name string, held []string) error {
	if len(held) == 0 {
		return fmt.Errorf("%s has no %q", holder, name)
	}
	const shown = 20
	list := strings.Join(held[:min(len(held), shown)], ", ")
	if len(held) > shown {
		list += fmt.Sprintf(" and %d more", len(held)-shown)
	}
	return fmt.Errorf("%s has no %q; it holds %s", holder, name, list)
}
