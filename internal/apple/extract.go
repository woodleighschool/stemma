package apple

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// ExtractApplication writes one application bundle from a flat package payload
// into destination and returns the bundle's path. appPath is the payload path
// inspection reported for the application; installedPath names the bundle when
// the payload root is the bundle itself. Entries outside the bundle are skipped
// and scripts are never run.
func ExtractApplication(ctx context.Context, filePath, appPath, installedPath, destination string) (string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxEntrySize {
		return "", fmt.Errorf("PKG input must be a regular file no larger than 16 GiB")
	}
	archive, err := openXAR(contextReaderAt{ctx, f}, info.Size())
	if err != nil {
		return "", err
	}
	payload, inner, err := payloadOf(archive, appPath)
	if err != nil {
		return "", err
	}
	name := path.Base(inner)
	if inner == "" {
		name = path.Base(installedPath)
	}
	if len(name) <= len(".app") || !strings.HasSuffix(strings.ToLower(name), ".app") || strings.ContainsAny(name, "/\\:\x00") {
		return "", fmt.Errorf("application %q is not a bundle path", appPath)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(destination)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(name, 0o700); err != nil {
		return "", err
	}
	bundle, err := root.OpenRoot(name)
	if err != nil {
		return "", err
	}
	defer func() { _ = bundle.Close() }()
	entry, err := archive.openEntry(payload, maxEntrySize)
	if err != nil {
		return "", err
	}
	defer func() { _ = entry.Close() }()
	budget := newPayloadBudget()
	err = streamPayload(ctx, io.NopCloser(entry), budget, func(content io.Reader) error {
		return extractBundle(content, budget, inner, bundle)
	})
	if err != nil {
		return "", fmt.Errorf("%s: %w", payload, err)
	}
	return filepath.Join(destination, name), nil
}

// payloadOf splits an inspected application path into its component payload
// entry and the bundle path inside that payload.
func payloadOf(archive *xarArchive, appPath string) (payload, inner string, err error) {
	for _, entry := range archive.entries {
		if entry.Type != "file" || path.Base(entry.Path) != "Payload" {
			continue
		}
		if appPath == entry.Path {
			return entry.Path, "", nil
		}
		if rest, ok := strings.CutPrefix(appPath, entry.Path+"/"); ok {
			return entry.Path, rest, nil
		}
	}
	return "", "", fmt.Errorf("package has no payload holding application %q", appPath)
}

// extractBundle writes the entries under prefix into bundle. An empty prefix
// means the payload root is the bundle.
func extractBundle(r io.Reader, budget *payloadBudget, prefix string, bundle *os.Root) error {
	type directory struct {
		name string
		perm fs.FileMode
	}
	var directories []directory
	entries := newCPIOReader(r, budget)
	written := 0
	for {
		entry, err := entries.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		relative, inside := bundlePath(entry.name, prefix)
		if !inside {
			if err := entries.skip(entry); err != nil {
				return err
			}
			continue
		}
		written++
		switch entry.kind() {
		case cpioDirectory:
			if err := bundle.MkdirAll(relative, 0o700); err != nil {
				return err
			}
			directories = append(directories, directory{relative, entry.perm()})
			if err := entries.skip(entry); err != nil {
				return err
			}
		case cpioRegular:
			// ODC entries carry their own content, so linked names, which vendor
			// payloads use for metadata sidecars, are written as separate files.
			if err := writeBundleFile(bundle, relative, entry, r); err != nil {
				return err
			}
		case cpioSymlink:
			if err := writeBundleLink(bundle, relative, entry, r); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: CPIO entry %q has an unsupported type", ErrUnsupported, entry.name)
		}
	}
	if written == 0 {
		return fmt.Errorf("payload holds no application %q", prefix)
	}
	// Directories stayed writable while their children arrived; restore the
	// payload's modes deepest first, keeping them searchable for rendering.
	slices.SortFunc(directories, func(a, b directory) int { return cmp.Compare(len(b.name), len(a.name)) })
	for _, dir := range directories {
		if err := bundle.Chmod(dir.name, dir.perm|0o700); err != nil {
			return err
		}
	}
	return nil
}

func bundlePath(name, prefix string) (string, bool) {
	if prefix == "" {
		return name, true
	}
	if name == prefix {
		return ".", true
	}
	if rest, ok := strings.CutPrefix(name, prefix+"/"); ok {
		return rest, true
	}
	return "", false
}

func writeBundleFile(bundle *os.Root, name string, entry cpioEntry, r io.Reader) error {
	if err := bundle.MkdirAll(path.Dir(name), 0o700); err != nil {
		return err
	}
	out, err := bundle.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, entry.perm()|0o600)
	if err != nil {
		return err
	}
	n, err := io.CopyN(out, r, entry.size)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("CPIO %s: %w", entry.name, err)
	}
	if n != entry.size {
		return fmt.Errorf("CPIO %s: short content", entry.name)
	}
	return nil
}

func writeBundleLink(bundle *os.Root, name string, entry cpioEntry, r io.Reader) error {
	if entry.size == 0 || entry.size > 4096 {
		return fmt.Errorf("%w: symlink %q target length", ErrUnsupported, entry.name)
	}
	target := make([]byte, entry.size)
	if _, err := io.ReadFull(r, target); err != nil {
		return fmt.Errorf("CPIO %s: %w", entry.name, err)
	}
	link := string(target)
	if path.IsAbs(link) || strings.ContainsAny(link, "\x00") {
		return fmt.Errorf("%w: symlink %q targets %q", ErrUnsupported, entry.name, link)
	}
	if err := bundle.MkdirAll(path.Dir(name), 0o700); err != nil {
		return err
	}
	return bundle.Symlink(link, name)
}
