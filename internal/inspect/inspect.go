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
			subjects, err := readTree(ctx, name)
			if err != nil {
				return plugin.Facts{}, err
			}
			return plugin.Facts{Version: plugin.FactsVersion, Subjects: append([]plugin.Subject{{ID: ".", Kind: "directory", Path: "."}}, subjects...)}, nil
		}
		app, err := apple.InspectApp(ctx, name)
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
		pkg, err := apple.InspectPackageContents(ctx, name)
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
	case ext == ".dmg" || diskimage.HasTrailer(f, info.Size()):
		if !diskimage.HasTrailer(f, info.Size()) {
			return plugin.Facts{}, fmt.Errorf("inspect: malformed DMG artifact")
		}
		root.Kind = "container"
		facts.Subjects, err = readDMG(ctx, name)
		if err != nil {
			return plugin.Facts{}, fmt.Errorf("inspect dmg: %w", err)
		}
	}
	root.SHA256, err = fileDigest(ctx, f, info.Size())
	if err != nil {
		return plugin.Facts{}, err
	}
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
	if input.ContentRoot != "" {
		return Selection(ctx, source, ".")
	}
	if input.Tree {
		node, err := source.At(ctx, "")
		if err != nil {
			return plugin.Facts{}, err
		}
		return inspectNode(ctx, node)
	}
	if !source.Traversable() {
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
	if input.SHA256 == "" {
		file, err := os.Open(input.Path)
		if err != nil {
			return plugin.Facts{}, err
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return plugin.Facts{}, err
		}
		input.SHA256, err = fileDigest(ctx, file, info.Size())
		if err != nil {
			return plugin.Facts{}, err
		}
	}
	root := plugin.Subject{ID: ".", Path: ".", Kind: "container", SHA256: input.SHA256}
	return plugin.Facts{Version: plugin.FactsVersion, Subjects: append([]plugin.Subject{root}, subjects...)}, nil
}

// Contents inventories applications, flat packages and disk images in a tree,
// archive or disk image. It does not follow symlinks or descend into bundles,
// and reads a package's receipts and payload, or a nested image's contents,
// only when that package or image is inspected itself.
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
		case (ext == ".pkg" || ext == ".dmg") && entry.Type().IsRegular():
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

// Selection inventories only the subtree a consumer copies, retaining paths
// relative to the source. Unselected siblings do not participate in inspection.
func Selection(ctx context.Context, source *contents.Source, selection string) (plugin.Facts, error) {
	if (selection == "" || selection == ".") && source.Artifact().ContentRoot == "" {
		return Source(ctx, source)
	}
	if selection == "" {
		selection = "."
	}
	node, err := source.At(ctx, selection)
	if err != nil {
		return plugin.Facts{}, err
	}
	facts, err := inspectNode(ctx, node)
	if err != nil {
		return plugin.Facts{}, err
	}
	for i := range facts.Subjects {
		subject := &facts.Subjects[i]
		subject.ID, subject.Path = path.Join(selection, subject.ID), path.Join(selection, subject.Path)
		if subject.Parent != "" {
			subject.Parent = path.Join(selection, subject.Parent)
		}
	}
	return facts, nil
}

func inspectNode(ctx context.Context, node contents.Node) (plugin.Facts, error) {
	var facts plugin.Facts
	var err error
	switch {
	case node.Local != "":
		facts, err = Read(ctx, node.Local)
	default:
		var info fs.FileInfo
		info, err = node.Stat()
		if err != nil {
			return plugin.Facts{}, err
		}
		root := plugin.Subject{ID: ".", Path: ".", Kind: "file"}
		switch {
		case info.IsDir() && applicationRoot(node):
			var app apple.AppFacts
			app, err = apple.InspectAppFS(ctx, node.FS, node.Path)
			root.Kind, root.App = "app", appFacts(app)
		case info.IsDir():
			var sub fs.FS
			sub, err = fs.Sub(node.FS, node.Path)
			if err == nil {
				links, ok := sub.(fs.ReadLinkFS)
				if !ok {
					return plugin.Facts{}, fmt.Errorf("selected filesystem does not report symlinks")
				}
				facts.Subjects, err = Contents(ctx, links)
			}
			root.Kind = "directory"
		case strings.EqualFold(path.Ext(node.Path), ".pkg"):
			root.Kind = "container"
		}
		facts.Subjects = append([]plugin.Subject{root}, facts.Subjects...)
	}
	if err != nil {
		return plugin.Facts{}, err
	}
	facts.Version = plugin.FactsVersion
	return facts, nil
}

func applicationRoot(node contents.Node) bool {
	_, err := node.FS.Lstat(path.Join(node.Path, "Contents/Info.plist"))
	return strings.EqualFold(path.Ext(node.Path), ".app") || !errors.Is(err, fs.ErrNotExist)
}

func fileDigest(ctx context.Context, file *os.File, size int64) (string, error) {
	if size > maxInspectedBytes {
		return "", errors.New("inspect digest: artifact exceeds size limit")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("inspect digest: %w", err)
	}
	digest := sha256.New()
	written, err := io.Copy(digest, fileio.Reader{Context: ctx, Reader: io.LimitReader(file, size+1)})
	if err != nil {
		return "", fmt.Errorf("inspect digest: %w", err)
	}
	if written != size {
		return "", errors.New("inspect: artifact changed size")
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
