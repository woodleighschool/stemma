package macsoftware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/diskimage"
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
	keep := archive.Leaves{"Contents/Info.plist", "Contents/Resources/*.icns"}
	if presentation == icon.Glassy {
		if app.App.Executable == "" {
			return icon.Subject{}, errors.New("native icon rendering requires an application executable")
		}
		keep = apple.IconResources(app.App.Executable)
	} else if presentation != icon.Raw {
		return icon.Subject{}, fmt.Errorf("unresolved icon presentation %q", presentation)
	}
	// Extraction writes into a new directory and expects its parent to exist.
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return icon.Subject{}, err
	}
	expanded := filepath.Join(workspace, "expanded")
	bundle, err := extractBundle(ctx, installer, app, expanded, keep)
	if err != nil {
		return icon.Subject{}, err
	}
	if presentation == icon.Raw {
		data, err := apple.AppIconFile(bundle)
		if err != nil {
			return icon.Subject{}, err
		}
		if data == nil {
			return icon.Subject{}, icon.ErrNoArtwork
		}
		artwork, err := icon.FromICNS(data)
		return icon.Subject{Artwork: artwork}, err
	}
	info, err := os.Lstat(filepath.Join(bundle, "Contents", "MacOS", app.App.Executable))
	switch {
	case err != nil:
		return icon.Subject{}, fmt.Errorf("application executable: %w", err)
	case info.Mode().IsRegular():
		return icon.Subject{Path: bundle}, nil
	case info.Mode()&fs.ModeSymlink == 0:
		return icon.Subject{}, fmt.Errorf("application executable %q is not a regular file", app.App.Executable)
	}
	// A linked executable can depend on files outside the selected subset.
	if err := os.RemoveAll(expanded); err != nil {
		return icon.Subject{}, err
	}
	bundle, err = extractBundle(ctx, installer, app, expanded, nil)
	return icon.Subject{Path: bundle}, err
}

func extractBundle(ctx context.Context, installer plugin.Artifact, app plugin.Subject, destination string, keep archive.Leaves) (string, error) {
	switch strings.ToLower(installer.Format) {
	case "dmg":
		return diskimage.Extract(ctx, installer.Path, destination, app.Path, keep)
	case "pkg":
		return apple.ExtractApplication(ctx, installer.Path, app.Path, app.InstalledPath, destination, keep)
	default:
		return "", fmt.Errorf("installer format %q holds no application bundle", installer.Format)
	}
}
