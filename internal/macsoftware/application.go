package macsoftware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/plugin"
)

// ErrNoApplication reports an installer whose preparation selected no application.
var ErrNoApplication = errors.New("installer has no selected application")

// Icon extracts artwork for Raw or stages a bundle for Glassy. Presentation
// must already be resolved. Installer bytes stay untouched.
func Icon(ctx context.Context, installer plugin.Artifact, workspace string, presentation icon.Presentation) (icon.Subject, error) {
	data, ok := installer.Evidence["macos.application"]
	if !ok {
		return icon.Subject{}, ErrNoApplication
	}
	var app plugin.Subject
	if err := json.Unmarshal(data, &app); err != nil || app.App == nil {
		return icon.Subject{}, errors.New("macos.application evidence requires an application subject")
	}
	return applicationIcon(ctx, installer, app, nil, workspace, presentation)
}

// IconFromSource reads a selected application from an already open input.
func IconFromSource(ctx context.Context, source *contents.Source, app plugin.Subject, workspace string, presentation icon.Presentation) (icon.Subject, error) {
	if app.App == nil {
		return icon.Subject{}, ErrNoApplication
	}
	input := source.Artifact()
	if input.Format == "" {
		input.Format = strings.TrimPrefix(strings.ToLower(filepath.Ext(input.Filename)), ".")
	}
	return applicationIcon(ctx, input, app, source, workspace, presentation)
}

func applicationIcon(ctx context.Context, installer plugin.Artifact, app plugin.Subject, source *contents.Source, workspace string, presentation icon.Presentation) (icon.Subject, error) {
	if presentation != icon.Glassy && presentation != icon.Raw {
		return icon.Subject{}, fmt.Errorf("unresolved icon presentation %q", presentation)
	}
	resource, err := apple.IconPath(app.App.IconFile)
	if err != nil {
		return icon.Subject{}, err
	}
	hasNamedIcon := presentation == icon.Glassy && app.App.IconName != ""
	if resource == "" && !hasNamedIcon {
		if app.App.IconName != "" {
			return icon.Subject{}, fmt.Errorf("asset catalog icon requires the glassy presentation on macOS: %w", icon.ErrUnsupportedArtwork)
		}
		return icon.Subject{}, icon.ErrNoArtwork
	}
	var files []string
	if resource != "" {
		files = append(files, resource)
	}
	if presentation == icon.Glassy {
		files = append(files, "Contents/Info.plist", "Contents/PkgInfo")
		if hasNamedIcon {
			files = append(files, "Contents/Resources/Assets.car")
		}
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return icon.Subject{}, err
	}
	bundle, err := extractBundle(ctx, installer, app, source, filepath.Join(workspace, "expanded"), files)
	if err != nil {
		return icon.Subject{}, err
	}
	data, err := apple.AppIconFile(bundle, app.App.IconFile)
	if err != nil {
		return icon.Subject{}, err
	}
	hasAssets := false
	if hasNamedIcon {
		info, err := os.Lstat(filepath.Join(bundle, "Contents/Resources/Assets.car"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return icon.Subject{}, err
		}
		if err == nil {
			if !info.Mode().IsRegular() {
				return icon.Subject{}, errors.New("icon asset catalog must be a regular file")
			}
			hasAssets = true
		}
	}
	if data == nil && !hasAssets {
		return icon.Subject{}, icon.ErrNoArtwork
	}
	if presentation == icon.Raw {
		artwork, err := icon.FromImage(data)
		return icon.Subject{Artwork: artwork}, err
	}
	executable, err := apple.ResolveExecutable(app.App.Executable, filepath.Base(bundle))
	if err != nil {
		return icon.Subject{}, err
	}
	if err := icon.StageExecutable(bundle, executable); err != nil {
		return icon.Subject{}, err
	}
	return icon.Subject{Path: bundle}, nil
}

func extractBundle(ctx context.Context, installer plugin.Artifact, app plugin.Subject, source *contents.Source, destination string, files []string) (string, error) {
	var keep archive.Leaves
	for _, name := range files {
		keep = append(keep, archive.Literal(name))
	}
	switch strings.ToLower(installer.Format) {
	case "dmg":
		return diskimage.Extract(ctx, installer.Path, destination, app.Path, keep)
	case "pkg":
		return apple.ExtractApplication(ctx, installer.Path, app.Path, app.InstalledPath, destination, keep)
	}
	if source == nil {
		var err error
		if err := os.MkdirAll(destination+"-source", 0o700); err != nil {
			return "", err
		}
		source, err = contents.Open(ctx, installer, destination+"-source")
		if err != nil {
			return "", err
		}
		defer func() { _ = source.Close() }()
	}
	name := path.Base(app.Path)
	if name == "." {
		name = path.Base(installer.ContentRoot)
		if name == "." {
			name = installer.Filename
		}
	}
	bundle := filepath.Join(destination, name)
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		return "", err
	}
	for _, name := range files {
		node, err := source.At(ctx, path.Join(app.Path, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		info, err := node.Stat()
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Size() > 32<<20 {
			return "", fmt.Errorf("icon resource %s must be a regular file of at most 32 MiB", name)
		}
		file, err := node.FS.Open(node.Path)
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(fileio.Reader{Context: ctx, Reader: file}, info.Size()+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return "", errors.Join(err, closeErr)
		}
		if int64(len(data)) != info.Size() {
			return "", errors.New("icon resource changed size")
		}
		if err := fileio.Write(filepath.Join(bundle, filepath.FromSlash(name)), data, 0o644); err != nil {
			return "", err
		}
	}
	return bundle, nil
}
