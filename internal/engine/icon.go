package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/macsoftware"
	"github.com/woodleighschool/stemma/internal/windowssoftware"
	"github.com/woodleighschool/stemma/plugin"
)

// IconOptions configures the icon method, which extracts the artwork prepared
// software carries and presents it as the declared asset.
type IconOptions struct {
	// Force replaces assets that already exist.
	Force bool
	// Size is the glassy edge in pixels; zero selects icon.Size.
	Size int
	// Presentation styles the artwork; icon.Auto follows the host.
	Presentation icon.Presentation
}

// verifyIcons rejects declared assets that are missing or invalid before any
// input is acquired, since publication sends exactly those bytes.
func verifyIcons(root string, plans map[string]resourcePlan, keys []string) error {
	for _, key := range keys {
		name := plans[key].Icon
		if name == "" {
			continue
		}
		if _, err := icon.Read(root, name); err != nil {
			if errors.Is(err, icon.ErrMissing) {
				return fmt.Errorf("resource %s: %w; run stemma icon %s to create it or commit the file", key, err, key)
			}
			return fmt.Errorf("resource %s: %w", key, err)
		}
	}
	return nil
}

// iconInput leases a copy of the declared asset for one destination.
func iconInput(root, name, dir string) (plugin.Artifact, error) {
	data, err := icon.Read(root, name)
	if err != nil {
		return plugin.Artifact{}, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return plugin.Artifact{}, err
	}
	target := filepath.Join(dir, name+".png")
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return plugin.Artifact{}, err
	}
	info, err := os.Stat(target)
	if err != nil {
		return plugin.Artifact{}, err
	}
	digest := sha256.Sum256(data)
	return plugin.Artifact{Path: target, Filename: name + ".png", Format: "png", Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:]), Mode: uint32(info.Mode().Perm())}, nil
}

// iconOutcome reports, in the words the CLI prints, why a resource needs no
// icon created, or "" when it does. Existing files stay unless forced, so
// committed artwork survives a catalog-wide run.
func iconOutcome(options IconOptions, root string, plan resourcePlan) string {
	if plan.Icon == "" {
		return "no icon declared"
	}
	if exists, err := icon.Exists(root, plan.Icon); err == nil && exists && !options.Force {
		return "unchanged"
	}
	return ""
}

// createIcon writes the declared asset for one prepared resource and reports
// the outcome in the words the CLI prints: the presentation created, or why
// nothing could be.
func createIcon(ctx context.Context, options IconOptions, root string, plan resourcePlan, outputs map[string]Prepared, work string) (string, error) {
	name := plan.Icon
	installer, ok := outputs["installer"]
	if !ok || installer.Path == "" {
		return "no artwork", nil
	}
	workspace := filepath.Join(work, "icon")
	done := plugin.Stage(ctx, "Extracting artwork")
	subject, err := iconSubject(ctx, plan.Resource.Kind, installer.artifact(), workspace, options.Presentation)
	done(err)
	if errors.Is(err, icon.ErrNoArtwork) || errors.Is(err, macsoftware.ErrNoApplication) {
		return "no artwork", nil
	}
	if err != nil {
		return "", err
	}
	done = plugin.Stage(ctx, "Presenting icon")
	data, presentation, err := icon.Present(ctx, subject, icon.Options{Presentation: options.Presentation, Size: options.Size, Workspace: workspace})
	done(err)
	if errors.Is(err, icon.ErrNoArtwork) {
		return "no artwork", nil
	}
	if err != nil {
		return "", err
	}
	if err := icon.Write(root, name, data); err != nil {
		return "", err
	}
	return "created " + string(presentation), nil
}

// iconSubject reads the installer artwork or bundle that presentation needs.
func iconSubject(ctx context.Context, kind string, installer plugin.Artifact, workspace string, presentation icon.Presentation) (icon.Subject, error) {
	switch kind {
	case "MacSoftware":
		return macsoftware.Icon(ctx, installer, workspace, presentation)
	case "WindowsSoftware":
		return windowssoftware.Icon(ctx, installer)
	}
	return icon.Subject{}, icon.ErrNoArtwork
}
