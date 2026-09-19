package macsoftware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if presentation != icon.Glassy && presentation != icon.Raw {
		return icon.Subject{}, fmt.Errorf("unresolved icon presentation %q", presentation)
	}
	resource, err := apple.IconPath(app.App.IconFile)
	if err != nil {
		return icon.Subject{}, err
	}
	hasNamedIcon := presentation == icon.Glassy && app.App.IconName != ""
	if resource == "" && !hasNamedIcon {
		return icon.Subject{}, icon.ErrNoArtwork
	}
	var keep archive.Leaves
	if resource != "" {
		keep = append(keep, archive.Literal(resource))
	}
	if presentation == icon.Glassy {
		keep = append(keep, "Contents/Info.plist", "Contents/PkgInfo")
		if hasNamedIcon {
			keep = append(keep, "Contents/Resources/Assets.car")
		}
	}
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return icon.Subject{}, err
	}
	bundle, err := extractBundle(ctx, installer, app, filepath.Join(workspace, "expanded"), keep)
	if err != nil {
		return icon.Subject{}, err
	}
	data, err = apple.AppIconFile(bundle, app.App.IconFile)
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
		artwork, err := icon.FromICNS(data)
		return icon.Subject{Artwork: artwork}, err
	}
	if err := icon.StageExecutable(bundle, app.App.Executable); err != nil {
		return icon.Subject{}, err
	}
	return icon.Subject{Path: bundle}, nil
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
