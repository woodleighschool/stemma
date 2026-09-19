// Package inspect collects static artifact facts while retaining the input.
package inspect

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/msi"
	"github.com/woodleighschool/stemma/plugin"
)

const maxTreeEntries = 100000
const maxInspectedBytes = 16 << 30

// Read collects application, DMG payload, PKG receipt and payload application, or MSI facts.
// It distinguishes installers by their contents and rejects malformed files
// claiming supported formats. Unknown regular files retain their exact identity.
func Read(ctx context.Context, name string) (plugin.Facts, error) {
	return read(ctx, name, true)
}

// ReadMetadata reads root artifact and receipt facts without scanning package
// payloads or directory contents. A recognized DMG retains only its container
// identity; filesystem and application inspection requires Read.
func ReadMetadata(ctx context.Context, name string) (plugin.Facts, error) {
	return read(ctx, name, false)
}

func read(ctx context.Context, name string, contents bool) (plugin.Facts, error) {
	if err := ctx.Err(); err != nil {
		return plugin.Facts{}, err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return plugin.Facts{}, fmt.Errorf("inspect: %w", err)
	}
	if info.IsDir() {
		_, plistErr := os.Lstat(filepath.Join(name, "Contents", "Info.plist"))
		if !strings.EqualFold(filepath.Ext(name), ".app") && os.IsNotExist(plistErr) {
			if contents {
				return readDirectory(ctx, name)
			}
			return plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Kind: "directory", Path: "."}}}, nil
		}
		app, err := apple.InspectApp(name)
		if err != nil {
			return plugin.Facts{}, fmt.Errorf("inspect app: %w", err)
		}
		return plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Kind: "app", Path: ".", App: appFacts(app)}}}, ctx.Err()
	}
	if !info.Mode().IsRegular() {
		return plugin.Facts{}, fmt.Errorf("inspect: input must be a regular file or directory")
	}
	if info.Size() > maxInspectedBytes {
		return plugin.Facts{}, fmt.Errorf("inspect: artifact exceeds size limit")
	}
	f, err := os.Open(name)
	if err != nil {
		return plugin.Facts{}, fmt.Errorf("inspect: %w", err)
	}
	defer func() { _ = f.Close() }()
	var header [8]byte
	n, err := io.ReadFull(f, header[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return plugin.Facts{}, fmt.Errorf("inspect header: %w", err)
	}
	root := plugin.Subject{ID: ".", Path: ".", Kind: "file"}
	facts := plugin.Facts{Version: plugin.FactsVersion}
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case bytes.HasPrefix(header[:n], []byte("xar!")):
		var pkg apple.PackageFacts
		if contents {
			pkg, err = apple.InspectPackageContents(ctx, name)
		} else {
			pkg, err = apple.InspectPackageMetadata(ctx, name)
		}
		if err != nil {
			return plugin.Facts{}, fmt.Errorf("inspect pkg: %w", err)
		}
		root.Kind, root.Installer = "container", installerFacts(pkg)
		facts.Subjects, err = packageSubjects(pkg)
		if err != nil {
			return plugin.Facts{}, err
		}

	case bytes.Equal(header[:n], []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}):
		metadata, err := msi.Read(name)
		if err != nil {
			return plugin.Facts{}, fmt.Errorf("inspect msi: %w", err)
		}
		root.Kind = "msi"
		root.MSI = &plugin.MSIFacts{ProductCode: metadata.ProductCode, ProductVersion: metadata.ProductVersion, ProductName: metadata.ProductName, Manufacturer: metadata.Manufacturer, UpgradeCode: metadata.UpgradeCode, PackageCode: metadata.PackageCode, Properties: metadata.Properties}
	case ext == ".pkg" || ext == ".msi" || ext == ".app":
		return plugin.Facts{}, fmt.Errorf("inspect: malformed %s artifact", ext)
	case ext == ".dmg" || isDMG(f, info.Size()):
		if !isDMG(f, info.Size()) {
			return plugin.Facts{}, fmt.Errorf("inspect: malformed DMG artifact")
		}
		root.Kind = "container"
		if contents {
			facts.Subjects, err = readDMG(ctx, name)
			if err != nil {
				return plugin.Facts{}, fmt.Errorf("inspect dmg: %w", err)
			}
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return plugin.Facts{}, fmt.Errorf("inspect digest: %w", err)
	}
	digest := sha256.New()
	written, err := io.Copy(digest, fileio.Reader{Context: ctx, Reader: io.LimitReader(f, info.Size()+1)})
	if err != nil {
		return plugin.Facts{}, fmt.Errorf("inspect digest: %w", err)
	}
	if written != info.Size() {
		return plugin.Facts{}, fmt.Errorf("inspect: artifact changed size")
	}
	root.SHA256 = hex.EncodeToString(digest.Sum(nil))
	facts.Subjects = append([]plugin.Subject{root}, facts.Subjects...)
	return facts, ctx.Err()
}

func readDMG(ctx context.Context, name string) ([]plugin.Subject, error) {
	image, err := diskimage.Open(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = image.Close() }()
	selected, err := image.Select(ctx, "")
	if err != nil {
		return nil, err
	}
	facts, err := ReadFS(ctx, image, selected)
	if err != nil {
		return nil, err
	}
	prefix := selected

	for i := range facts.Subjects {
		subject := &facts.Subjects[i]
		subject.ID = path.Join(prefix, subject.ID)
		subject.Path = path.Join(prefix, subject.Path)
		if subject.Parent == "" {
			subject.Parent = "."
		} else {
			subject.Parent = path.Join(prefix, subject.Parent)
		}
	}
	return facts.Subjects, nil
}

// ReadFS inspects an application or flat package within a filesystem. It reads
// application metadata directly and does not materialize the selected payload.
func ReadFS(ctx context.Context, fsys fs.ReadLinkFS, name string) (plugin.Facts, error) {
	if err := ctx.Err(); err != nil {
		return plugin.Facts{}, err
	}
	info, err := fsys.Lstat(name)
	if err != nil {
		return plugin.Facts{}, err
	}
	facts := plugin.Facts{Version: plugin.FactsVersion}
	if info.IsDir() {
		app, err := apple.InspectAppFS(ctx, fsys, name)
		if err != nil {
			return plugin.Facts{}, err
		}
		facts.Subjects = []plugin.Subject{{ID: ".", Path: ".", Kind: "app", App: appFacts(app)}}
		return facts, ctx.Err()
	}
	if !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxInspectedBytes {
		return plugin.Facts{}, fmt.Errorf("inspection requires a regular package within the size limit")
	}
	f, err := fsys.Open(name)
	if err != nil {
		return plugin.Facts{}, err
	}
	defer func() { _ = f.Close() }()
	reader, ok := f.(io.ReaderAt)
	if !ok {
		return plugin.Facts{}, fmt.Errorf("package filesystem does not support random access")
	}
	pkg, err := apple.InspectPackageReader(ctx, reader, info.Size())
	if err != nil {
		return plugin.Facts{}, err
	}
	subjects, err := packageSubjects(pkg)
	if err != nil {
		return plugin.Facts{}, err
	}
	digest := sha256.New()
	n, err := io.Copy(digest, fileio.Reader{Context: ctx, Reader: io.NewSectionReader(reader, 0, info.Size())})
	if err != nil {
		return plugin.Facts{}, err
	}
	if n != info.Size() {
		return plugin.Facts{}, fmt.Errorf("package length mismatch")
	}
	facts.Subjects = append([]plugin.Subject{{ID: ".", Path: ".", Kind: "container", SHA256: hex.EncodeToString(digest.Sum(nil)), Installer: installerFacts(pkg)}}, subjects...)
	return facts, ctx.Err()
}

func installerFacts(pkg apple.PackageFacts) *plugin.InstallerFacts {
	if pkg.Version == "" && pkg.MinimumOS == "" && pkg.RestartAction == "" {
		return nil
	}
	return &plugin.InstallerFacts{Version: pkg.Version, MinimumOS: pkg.MinimumOS, RestartAction: pkg.RestartAction}
}

func packageSubjects(pkg apple.PackageFacts) ([]plugin.Subject, error) {
	if len(pkg.Packages) == 0 {
		return nil, fmt.Errorf("inspect pkg: %w: no component receipts", apple.ErrUnsupported)
	}
	var subjects []plugin.Subject
	for _, receipt := range pkg.Packages {
		subjects = append(subjects, plugin.Subject{
			ID: receipt.Path, Parent: ".", Kind: "package", Path: receipt.Path,
			Package: &plugin.PackageFacts{Identifier: receipt.Identifier, Version: receipt.Version, InstallLocation: receipt.InstallLocation, InstalledSize: receipt.InstalledSize, HasPayload: receipt.HasPayload},
		})
	}
	appPaths := make(map[string]bool, len(pkg.Applications))
	for _, app := range pkg.Applications {
		appPaths[app.Path] = true
	}
	for _, app := range pkg.Applications {
		parent := app.PackagePath
		for outer := path.Dir(app.Path); outer != "."; outer = path.Dir(outer) {
			if appPaths[outer] {
				parent = outer
				break
			}
		}
		subjects = append(subjects, plugin.Subject{ID: app.Path, Parent: parent, Kind: "app", Path: app.Path, InstalledPath: app.InstalledPath, App: appFacts(app.App)})
	}
	return subjects, nil
}

func readDirectory(ctx context.Context, name string) (plugin.Facts, error) {
	root, err := os.OpenRoot(name)
	if err != nil {
		return plugin.Facts{}, err
	}
	defer func() { _ = root.Close() }()
	facts := plugin.Facts{Version: plugin.FactsVersion}
	remaining := int64(maxInspectedBytes)
	metadataRemaining := 32 << 20
	err = fs.WalkDir(root.FS(), ".", func(relative string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(facts.Subjects) >= maxTreeEntries {
			return fmt.Errorf("inspection tree exceeds entry limit")
		}
		parent := path.Dir(relative)
		if relative == "." {
			parent = ""
		}
		subject := plugin.Subject{ID: relative, Path: relative, Parent: parent, Kind: "directory"}
		if entry.IsDir() {
			if relative != "." && strings.EqualFold(filepath.Ext(relative), ".app") {
				app, err := apple.InspectApp(filepath.Join(name, filepath.FromSlash(relative)))
				if err != nil {
					return fmt.Errorf("%s: %w", relative, err)
				}
				metadataRemaining -= len(app.BundleID) + len(app.Name) + len(app.Version) + len(app.Build) + len(app.Executable) + len(app.MinimumOS)
				if metadataRemaining < 0 {
					return fmt.Errorf("inspection tree metadata exceeds size limit")
				}
				subject.Kind, subject.App = "app", appFacts(app)
				facts.Subjects = append(facts.Subjects, subject)
				return filepath.SkipDir
			}
			facts.Subjects = append(facts.Subjects, subject)
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: inspection tree entry %q is not a regular file", apple.ErrUnsupported, relative)
		}
		if info.Size() > remaining {
			return fmt.Errorf("inspection tree exceeds size limit")
		}
		remaining -= info.Size()
		file, err := root.Open(filepath.FromSlash(relative))
		if err != nil {
			return err
		}
		digest := sha256.New()
		n, copyErr := io.Copy(digest, fileio.Reader{Context: ctx, Reader: io.LimitReader(file, info.Size()+1)})
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != info.Size() {
			return fmt.Errorf("inspection tree file %q changed size", relative)
		}
		subject.Kind, subject.SHA256 = "file", hex.EncodeToString(digest.Sum(nil))
		facts.Subjects = append(facts.Subjects, subject)
		return nil
	})
	if err != nil {
		return plugin.Facts{}, fmt.Errorf("inspect directory: %w", err)
	}
	return facts, nil
}

func appFacts(app apple.AppFacts) *plugin.AppFacts {
	return &plugin.AppFacts{BundleID: app.BundleID, Name: app.Name, Version: app.Version, Build: app.Build, Executable: app.Executable, MinimumOS: app.MinimumOS}
}

func isDMG(f *os.File, size int64) bool {
	if size < 512 {
		return false
	}
	var trailer [4]byte
	_, err := f.ReadAt(trailer[:], size-512)
	return err == nil && string(trailer[:]) == "koly"
}
