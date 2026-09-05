// Package pkgbuild creates unsigned component packages from declared local files.
package pkgbuild

import (
	"compress/gzip"
	"context"
	"encoding/xml"
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

	cpio "github.com/korylprince/go-cpio-odc"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/fileio"
)

// Version identifies the package derivation, including its format and metadata policy.
const Version = "stemma.pkgbuild/0.1.1"
const maxFileSize = 64 << 20
const maxTotalSize = 512 << 20

// The adopted BOM writer uses one 4 KiB leaf (12-byte header, 8 bytes per path).
const maxEntries = 500
const maxScriptSize = 1 << 20

// Options declares one component package. Paths are relative to the input root.
// Payload is optional; Scripts accepts preinstall and postinstall hooks only.
// Files install as root:wheel. Hook contents are packaged, never executed.
// Timestamp normalizes all package dates when supplied. Otherwise source mtimes
// are preserved and generated archive members use the captured build time.
type Options struct {
	Identifier      string            `json:"identifier"`
	Version         string            `json:"version"`
	InstallLocation string            `json:"install_location,omitempty"`
	Payload         string            `json:"payload,omitempty"`
	Scripts         map[string]string `json:"scripts,omitempty"`
	Timestamp       time.Time         `json:"timestamp,omitzero"`
}

type packageInfo struct {
	XMLName     xml.Name     `xml:"pkg-info"`
	Format      string       `xml:"format-version,attr"`
	Identifier  string       `xml:"identifier,attr"`
	Version     string       `xml:"version,attr"`
	Location    string       `xml:"install-location,attr,omitempty"`
	Auth        string       `xml:"auth,attr"`
	Overwrite   string       `xml:"overwrite-permissions,attr"`
	Relocatable string       `xml:"relocatable,attr"`
	Generator   string       `xml:"generator-version,attr"`
	Payload     *payloadInfo `xml:"payload,omitempty"`
	Scripts     *scriptInfo  `xml:"scripts,omitempty"`
}
type payloadInfo struct {
	Files     int   `xml:"numberOfFiles,attr"`
	Kilobytes int64 `xml:"installKBytes,attr"`
}
type scriptInfo struct {
	Preinstall  *hookInfo `xml:"preinstall,omitempty"`
	Postinstall *hookInfo `xml:"postinstall,omitempty"`
}
type hookInfo struct {
	File    string `xml:"file,attr"`
	Timeout int    `xml:"timeout,attr"`
}

var packageIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*$`)

// Build writes a new unsigned PKG outside root without changing the input tree.
// It supports ordinary payload files/directories and declared install hooks.
// Unsupported links, extended metadata and bundle installation policy are rejected.
func Build(ctx context.Context, root, output string, opts Options) error {
	if err := validate(opts); err != nil {
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
	info := packageInfo{Format: "2", Identifier: opts.Identifier, Version: opts.Version, Location: opts.InstallLocation, Auth: "root", Overwrite: "true", Relocatable: "false", Generator: Version}
	if info.Location == "" {
		info.Location = "/"
	}
	if opts.Payload != "" {
		paths, size, err := writePayload(ctx, source, opts.Payload, filepath.Join(workspace, "Payload"), opts.Timestamp)
		if err != nil {
			return err
		}
		bom := buildBom(paths)
		if err := os.WriteFile(filepath.Join(workspace, "Bom"), bom, 0o644); err != nil {
			return err
		}
		info.Payload = &payloadInfo{Files: len(paths), Kilobytes: (size + 1023) / 1024}
	}
	if len(opts.Scripts) > 0 {
		if err := writeScripts(ctx, source, opts.Scripts, filepath.Join(workspace, "Scripts"), opts.Timestamp, generated); err != nil {
			return err
		}
		info.Scripts = &scriptInfo{}
		if _, ok := opts.Scripts["preinstall"]; ok {
			info.Scripts.Preinstall = &hookInfo{File: "./preinstall", Timeout: 600}
		}
		if _, ok := opts.Scripts["postinstall"]; ok {
			info.Scripts.Postinstall = &hookInfo{File: "./postinstall", Timeout: 600}
		}
	}
	metadata, err := xml.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	if err = os.WriteFile(filepath.Join(workspace, "PackageInfo"), append([]byte(xml.Header), metadata...), 0o644); err != nil {
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

func validate(opts Options) error {
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

func readContents(ctx context.Context, f *os.File, info os.FileInfo, limit int64) ([]byte, error) {
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("package input is not a regular file within the size limit")
	}
	body, err := io.ReadAll(io.LimitReader(fileio.Reader{Context: ctx, Reader: f}, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != info.Size() {
		return nil, errors.New("package input changed while reading or exceeds its size limit")
	}
	return body, nil
}

func writePayload(ctx context.Context, source *os.Root, prefix, destination string, timestamp time.Time) ([]*bomPath, int64, error) {
	f, err := os.Create(destination)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	gz := gzip.NewWriter(f)
	writer := cpio.NewWriter(gz, 512)
	var paths []*bomPath
	var total int64
	var walk func(string, string, uint32) error
	walk = func(name, relative string, parentID uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(paths) >= maxEntries {
			return errors.New("package exceeds entry limit")
		}
		if !validPath(name) || len(path.Base(name)) > 255 {
			return errors.New("unsupported package path")
		}
		file, info, err := checkedFile(source, name)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		if relative == "." && !info.IsDir() {
			return errors.New("payload must be a directory")
		}
		if info.IsDir() && relative != "." {
			ext := strings.ToLower(path.Ext(relative))
			if slices.Contains([]string{".app", ".framework", ".bundle", ".plugin"}, ext) {
				return fmt.Errorf("bundle installation policy is unsupported: %s", relative)
			}
		}
		var body []byte
		if info.Mode().IsRegular() {
			body, err = readContents(ctx, file, info, maxFileSize)
			if err != nil {
				return err
			}
			total += int64(len(body))
			if total > maxTotalSize {
				return errors.New("package exceeds total payload size limit")
			}
		}
		id := uint32(len(paths) + 1)
		mode := info.Mode()
		modified, err := packageTime(info.ModTime(), timestamp)
		if err != nil {
			return fmt.Errorf("package input %s: %w", name, err)
		}
		cfile := &cpio.File{Inode: uint64(id), FileMode: mode, NLink: 1, Path: "./" + relative, Body: body, ModifiedTime: modified}
		if relative == "." {
			cfile.Path = "."
		}
		if err := writer.WriteFile(cfile); err != nil {
			return err
		}
		item := &bomPath{id: id, parentID: parentID, name: path.Base(relative), isDir: info.IsDir(), mode: uint16(cpio.MarshalFileMode(mode)), size: uint32(len(body)), modified: uint32(modified.Unix())}
		if info.Mode().IsRegular() {
			item.checksum = bomChecksum(body)
		}
		paths = append(paths, item)
		if info.IsDir() {
			children, err := file.ReadDir(maxEntries - len(paths) + 1)
			if err != nil && err != io.EOF {
				return err
			}
			if len(children) > maxEntries-len(paths) {
				return errors.New("package exceeds entry limit")
			}
			slices.SortFunc(children, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
			for _, child := range children {
				if err := walk(path.Join(name, child.Name()), path.Join(relative, child.Name()), id); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(prefix, ".", 0); err != nil {
		return nil, 0, err
	}
	if _, err := writer.Close(); err != nil {
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
	writer := cpio.NewWriter(gz, 512)
	if err := writer.WriteFile(&cpio.File{Inode: 1, FileMode: os.ModeDir | 0o755, NLink: 1, Path: ".", ModifiedTime: generated}); err != nil {
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
		body, err := readContents(ctx, file, info, maxScriptSize)
		_ = file.Close()
		if err != nil {
			return err
		}
		modified, err := packageTime(info.ModTime(), timestamp)
		if err != nil {
			return fmt.Errorf("package script %s: %w", name, err)
		}
		if err := writer.WriteFile(&cpio.File{Inode: uint64(i + 2), FileMode: 0o755, NLink: 1, Path: "./" + name, Body: body, ModifiedTime: modified}); err != nil {
			return err
		}
	}
	if _, err := writer.Close(); err != nil {
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
