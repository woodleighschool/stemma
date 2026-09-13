// Package pkgbuild creates unsigned component packages from declared local files.
package pkgbuild

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/deploymenttheory/go-macos-pkg/pkg/bom"
	"github.com/deploymenttheory/go-macos-pkg/pkg/cpio"
	"github.com/deploymenttheory/go-macos-pkg/pkg/flatpkg"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

// Version identifies the package derivation, including its format and metadata policy.
const Version = "stemma.pkgbuild/0.1.5"

// BOM's 32-bit size field is narrower than ODC's 33-bit file length.
const maxFileSize int64 = math.MaxUint32

// MaxPayloadSize bounds the expanded payload bytes.
const MaxPayloadSize int64 = 16 << 30

// MaxEntries bounds the in-memory package inventory.
const MaxEntries = 100000

// MaxScriptSize bounds each packaged endpoint hook.
const MaxScriptSize = 1 << 20

// Options declares one component package. Paths are relative to the input root.
// Payload is optional; Scripts accepts preinstall and postinstall hooks only.
// Files default to root:wheel; Metadata overrides archive ownership and modes.
// Hook contents are packaged, never executed.
// Timestamp normalizes all package dates when supplied. Otherwise source mtimes
// are preserved and generated archive members use the captured build time.
type Options struct {
	Identifier      string                   `json:"identifier"`
	Version         string                   `json:"version"`
	InstallLocation string                   `json:"install_location,omitempty"`
	Payload         string                   `json:"payload,omitempty"`
	Scripts         map[string]string        `json:"scripts,omitempty"`
	Metadata        map[string]EntryMetadata `json:"metadata,omitempty"`
	Timestamp       time.Time                `json:"timestamp,omitzero"`
}

// EntryMetadata controls archive metadata at an exact payload-relative path.
// A nil mode preserves source permissions. UID and GID default to root:wheel.
type EntryMetadata struct {
	Mode *uint32 `json:"mode,omitempty"`
	UID  uint32  `json:"uid,omitempty"`
	GID  uint32  `json:"gid,omitempty"`
}

var packageIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// Build writes a new unsigned PKG outside root without changing the input tree.
// It supports ordinary payload files/directories and declared install hooks.
// Relative symlinks are retained; unrepresentable filesystem metadata is rejected.
func Build(ctx context.Context, root, output string, opts Options) error {
	plugin.Stage(ctx, "Building Apple package")
	if err := Validate(opts); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	generated := opts.Timestamp
	if generated.IsZero() {
		generated = time.Now()
	}
	generated, err := packageTime(generated, time.Time{})
	if err != nil {
		return err
	}
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootPath, err = filepath.EvalSymlinks(rootPath)
	if err != nil {
		return err
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	outputParent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return err
	}
	output = filepath.Join(outputParent, filepath.Base(output))
	if relative, err := filepath.Rel(rootPath, output); err != nil || filepath.IsLocal(relative) {
		return errors.New("package output must be outside the input root")
	}
	if _, err := os.Lstat(output); !errors.Is(err, fs.ErrNotExist) {
		return errors.New("package output already exists or cannot be checked")
	}
	source, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() { _ = source.Close() }()
	workspace, err := os.MkdirTemp(filepath.Dir(output), ".pkgbuild-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	info := flatpkg.PackageInfo{FormatVersion: 2, Identifier: opts.Identifier, Version: opts.Version, InstallLocation: opts.InstallLocation, Auth: "root", OverwritePermissions: new(true), Relocatable: new(false), GeneratorVersion: Version}
	if info.InstallLocation == "" {
		info.InstallLocation = "/"
	}
	if opts.Payload != "" {
		paths, size, err := writePayload(ctx, source, opts.Payload, filepath.Join(workspace, "Payload"), opts.Timestamp, opts.Metadata)
		if err != nil {
			return err
		}
		builder := bom.NewBuilder()
		for _, entry := range paths {
			if err := builder.Add(entry); err != nil {
				return err
			}
		}
		var data bytes.Buffer
		if err := builder.Build(&data); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace, "Bom"), data.Bytes(), 0o644); err != nil {
			return err
		}
		info.Payload = &flatpkg.Payload{NumberOfFiles: len(paths), InstallKBytes: int((size + 1023) / 1024)}
	}
	if len(opts.Scripts) > 0 {
		if err := writeScripts(ctx, source, opts.Scripts, filepath.Join(workspace, "Scripts"), opts.Timestamp, generated); err != nil {
			return err
		}
		info.Scripts = &flatpkg.Scripts{}
		if _, ok := opts.Scripts["preinstall"]; ok {
			info.Scripts.Preinstall = []flatpkg.Script{{File: "./preinstall", Timeout: flatpkg.DefaultScriptTimeout}}
		}
		if _, ok := opts.Scripts["postinstall"]; ok {
			info.Scripts.Postinstall = []flatpkg.Script{{File: "./postinstall", Timeout: flatpkg.DefaultScriptTimeout}}
		}
	}
	metadata, err := info.Marshal()
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(workspace, "PackageInfo"), metadata, 0o644); err != nil {
		return err
	}
	temporary := filepath.Join(workspace, "output.pkg")
	if err := writeXar(ctx, workspace, temporary, generated); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Link(temporary, output)
}

// Validate checks the package declaration without reading its input files.
func Validate(opts Options) error {
	if !opts.Timestamp.IsZero() {
		if _, err := packageTime(opts.Timestamp, time.Time{}); err != nil {
			return err
		}
	}
	if !packageIdentifier.MatchString(opts.Identifier) || len(opts.Identifier) > 255 {
		return errors.New("package identifier must be a nonempty reverse-domain identifier")
	}
	if opts.Version == "" || len(opts.Version) > 128 || !utf8.ValidString(opts.Version) || strings.ContainsAny(opts.Version, "\x00\r\n\t") {
		return errors.New("package version must be a nonempty single-line string")
	}
	if opts.InstallLocation != "" && (!path.IsAbs(opts.InstallLocation) || path.Clean(opts.InstallLocation) != opts.InstallLocation || strings.ContainsAny(opts.InstallLocation, "\x00\\\r\n\t")) {
		return errors.New("install location must be a clean absolute POSIX path")
	}
	if opts.Payload == "" && len(opts.Scripts) == 0 {
		return errors.New("package requires payload or install scripts")
	}
	if opts.Payload != "" && !validPath(opts.Payload) {
		return errors.New("payload must be a confined relative path")
	}
	for name, metadata := range opts.Metadata {
		if opts.Payload == "" || !validPath(name) {
			return errors.New("archive metadata requires a confined payload-relative path")
		}
		if metadata.Mode != nil && *metadata.Mode > 0o777 {
			return errors.New("archive mode must contain ordinary permission bits only")
		}
		if metadata.UID > 0o777777 || metadata.GID > 0o777777 {
			return errors.New("archive UID/GID exceeds the ODC 18-bit field")
		}
	}
	for name, filename := range opts.Scripts {
		if name != "preinstall" && name != "postinstall" {
			return fmt.Errorf("unsupported package script %q", name)
		}
		if !validPath(filename) {
			return fmt.Errorf("script %s must be a confined relative path", name)
		}
	}
	return nil
}
func validPath(name string) bool {
	return fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00\r\n\t") && utf8.ValidString(name) && len(name) <= 4096
}

func checkedFile(source *os.Root, name string) (*os.File, os.FileInfo, error) {
	for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
		info, err := source.Lstat(parent)
		if err != nil {
			return nil, nil, err
		}
		if !info.IsDir() {
			return nil, nil, fmt.Errorf("package input ancestor is not a directory: %s", parent)
		}
	}
	info, err := source.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("unsupported package file type: %s", name)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return nil, nil, fmt.Errorf("unsupported package special permission bits: %s", name)
	}
	f, err := source.Open(name)
	if err != nil {
		return nil, nil, err
	}
	actual, err := f.Stat()
	if err == nil && !os.SameFile(info, actual) {
		err = errors.New("package input changed while opening")
	}
	if err == nil {
		err = archive.CheckMetadata(f, actual)
	}
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("package input %s: %w", name, err)
	}
	return f, actual, nil
}

func copyContents(ctx context.Context, destination io.Writer, f *os.File, info os.FileInfo) error {
	n, err := io.Copy(destination, io.LimitReader(fileio.Reader{Context: ctx, Reader: f}, info.Size()+1))
	if err != nil {
		return err
	}
	if n != info.Size() {
		return errors.New("package input changed size while reading")
	}
	return nil
}

func writePayload(ctx context.Context, source *os.Root, prefix, destination string, timestamp time.Time, metadata map[string]EntryMetadata) ([]bom.Entry, int64, error) {
	f, err := os.Create(destination)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	writer := cpio.NewWriter(gz)
	var paths []bom.Entry
	var total int64
	used := map[string]bool{}
	var walk func(string, string) error
	walk = func(name, relative string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(paths) >= MaxEntries {
			return errors.New("package exceeds entry limit")
		}
		if !validPath(name) || len(path.Base(name)) > 255 {
			return errors.New("unsupported package path")
		}
		info, err := source.Lstat(name)
		if err != nil {
			return err
		}
		var file *os.File
		var link string
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = archive.Readlink(source, name)
			if err != nil {
				return err
			}
			if !validPath(path.Join(path.Dir(relative), link)) {
				return fmt.Errorf("escaping payload symlink %s", relative)
			}
		} else {
			file, info, err = checkedFile(source, name)
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
		}
		if relative == "." && !info.IsDir() {
			return errors.New("payload must be a directory")
		}
		var size int64
		if link != "" {
			size = int64(len(link))
		}
		if info.Mode().IsRegular() {
			size = info.Size()
			if size < 0 || size > maxFileSize {
				return errors.New("package input exceeds BOM file size limit")
			}
			if size > MaxPayloadSize-total {
				return errors.New("package exceeds total payload size limit")
			}
			total += size
		}
		id := uint32(len(paths) + 1)
		attrs := metadata[relative]
		used[relative] = true
		permissions := uint32(info.Mode().Perm())
		if attrs.Mode != nil {
			permissions = *attrs.Mode
		}
		mode := permissions | cpio.ModeRegular
		typ := bom.TypeFile
		if link != "" {
			mode = permissions | cpio.ModeSymlink
			typ = bom.TypeLink
		}
		if info.IsDir() {
			mode = permissions | cpio.ModeDir
			typ = bom.TypeDirectory
		}
		modified, err := packageTime(info.ModTime(), timestamp)
		if err != nil {
			return fmt.Errorf("package input %s: %w", name, err)
		}
		cfile := &cpio.Header{Inode: uint64(id), Mode: mode, UID: attrs.UID, GID: attrs.GID, NLink: 1, Name: "./" + relative, Size: size, ModTime: modified}
		if relative == "." {
			cfile.Name = "."
		}
		if err := writer.WriteHeader(cfile); err != nil {
			return err
		}
		item := bom.Entry{Path: cfile.Name, Type: typ, Architecture: 15, Mode: uint16(mode), UID: attrs.UID, GID: attrs.GID, Size: size, ModTime: modified, LinkTarget: link}
		if info.Mode().IsRegular() || link != "" {
			digest := bom.NewCksum()
			if link != "" {
				if _, err := io.WriteString(io.MultiWriter(writer, digest), link); err != nil {
					return err
				}
			} else if err := copyContents(ctx, io.MultiWriter(writer, digest), file, info); err != nil {
				return err
			}
			item.Checksum = digest.Sum32()
		}
		paths = append(paths, item)
		if info.IsDir() {
			children, err := file.ReadDir(MaxEntries - len(paths) + 1)
			if err != nil && err != io.EOF {
				return err
			}
			if len(children) > MaxEntries-len(paths) {
				return errors.New("package exceeds entry limit")
			}
			slices.SortFunc(children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			for _, child := range children {
				if err := walk(path.Join(name, child.Name()), path.Join(relative, child.Name())); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(prefix, "."); err != nil {
		return nil, 0, err
	}
	for name := range metadata {
		if !used[name] {
			return nil, 0, fmt.Errorf("archive metadata names missing payload path %s", name)
		}
	}
	if err := writer.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	if err := f.Close(); err != nil {
		return nil, 0, err
	}
	return paths, total, nil
}

func writeScripts(ctx context.Context, source *os.Root, scripts map[string]string, destination string, timestamp, generated time.Time) error {
	f, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	writer := cpio.NewWriter(gz)
	if err := writer.WriteHeader(&cpio.Header{Inode: 1, Mode: cpio.ModeDir | 0o755, NLink: 1, Name: ".", ModTime: generated}); err != nil {
		return err
	}
	for i, name := range []string{"preinstall", "postinstall"} {
		filename, ok := scripts[name]
		if !ok {
			continue
		}
		file, info, err := checkedFile(source, filename)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > MaxScriptSize {
			return errors.New("package script is not a regular file within the size limit")
		}
		modified, err := packageTime(info.ModTime(), timestamp)
		if err != nil {
			return fmt.Errorf("package script %s: %w", name, err)
		}
		if err := writer.WriteHeader(&cpio.Header{Inode: uint64(i + 2), Mode: cpio.ModeRegular | 0o755, NLink: 1, Name: "./" + name, Size: info.Size(), ModTime: modified}); err != nil {
			return err
		}
		if err := copyContents(ctx, writer, file, info); err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

// CPIO and BOM share second precision; BOM's unsigned field is the tighter bound.
func packageTime(source, override time.Time) (time.Time, error) {
	if !override.IsZero() {
		source = override
	}
	seconds := source.Unix()
	if seconds < 0 || seconds > math.MaxUint32 {
		return time.Time{}, errors.New("package timestamp must be representable as unsigned 32-bit Unix seconds")
	}
	return time.Unix(seconds, 0).UTC(), nil
}
