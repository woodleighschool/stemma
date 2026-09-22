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
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/msi"
	"github.com/woodleighschool/stemma/plugin"
)

const maxTreeEntries = 100000
const maxTreeMetadata = 32 << 20
const maxInspectedBytes = 16 << 30

// Read collects application, PKG receipt and payload application, or MSI facts,
// and inventories the applications and packages in a DMG or directory tree.
// It distinguishes installers by their contents and rejects malformed files
// claiming supported formats. Unknown regular files retain their exact identity.
func Read(ctx context.Context, name string) (plugin.Facts, error) {
	return read(ctx, name, true)
}

// ReadMetadata reads root artifact and receipt facts without scanning package
// payloads or directory contents. A recognized DMG retains only its container
// identity; its inventory requires Read.
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
			facts := plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Kind: "directory", Path: "."}}}
			if contents {
				subjects, err := readTree(ctx, name)
				if err != nil {
					return plugin.Facts{}, err
				}
				facts.Subjects = append(facts.Subjects, subjects...)
			}
			return facts, nil
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
	return Contents(ctx, image)
}

func readTree(ctx context.Context, name string) ([]plugin.Subject, error) {
	root, err := os.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("inspect directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	return Contents(ctx, root.FS().(fs.ReadLinkFS))
}

// Source inventories a leased input. A disk image or archive is read through
// its open contents; a local tree or file is read in place.
func Source(ctx context.Context, source *contents.Source) (plugin.Facts, error) {
	input := source.Artifact()
	if input.Tree || !source.Traversable() {
		return Read(ctx, input.Path)
	}
	node, err := source.At(ctx, ".")
	if err != nil {
		return plugin.Facts{}, err
	}
	subjects, err := Contents(ctx, node.FS)
	if err != nil {
		return plugin.Facts{}, err
	}
	root := plugin.Subject{ID: ".", Path: ".", Kind: "container", SHA256: input.SHA256}
	return plugin.Facts{Version: plugin.FactsVersion, Subjects: append([]plugin.Subject{root}, subjects...)}, nil
}

// Contents inventories the applications and flat packages in a tree, archive
// or disk image. It does not follow symlinks or descend into bundles, and reads
// a package's receipts and payload only when that package is inspected itself.
// Subject IDs are paths below the root, which is each subject's parent.
func Contents(ctx context.Context, fsys fs.ReadLinkFS) ([]plugin.Subject, error) {
	var subjects []plugin.Subject
	entries := 0
	metadataRemaining := int64(maxTreeMetadata)
	err := fs.WalkDir(fsys, ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entries++; entries > maxTreeEntries {
			return errors.New("entry limit exceeded")
		}
		if name == "." || entry.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		switch ext := strings.ToLower(path.Ext(name)); {
		case ext == ".app" && entry.IsDir():
			info, err := fsys.Lstat(path.Join(name, "Contents/Info.plist"))
			if err != nil {
				return err
			}
			if info.Size() > metadataRemaining {
				return errors.New("application metadata exceeds size limit")
			}
			metadataRemaining -= info.Size()
			app, err := apple.InspectAppFS(ctx, fsys, name)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			subjects = append(subjects, plugin.Subject{ID: name, Parent: ".", Kind: "app", Path: name, App: appFacts(app)})
			return fs.SkipDir
		case ext == ".pkg" && entry.Type().IsRegular():
			subjects = append(subjects, plugin.Subject{ID: name, Parent: ".", Kind: "container", Path: name})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inspect contents: %w", err)
	}
	return subjects, nil
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

func appFacts(app apple.AppFacts) *plugin.AppFacts {
	return &plugin.AppFacts{BundleID: app.BundleID, Name: app.Name, Version: app.Version, Build: app.Build, Executable: app.Executable, IconFile: app.IconFile, IconName: app.IconName, MinimumOS: app.MinimumOS}
}

func isDMG(f *os.File, size int64) bool {
	if size < 512 {
		return false
	}
	var trailer [4]byte
	_, err := f.ReadAt(trailer[:], size-512)
	return err == nil && string(trailer[:]) == "koly"
}
