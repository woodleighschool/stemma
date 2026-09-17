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
	"github.com/woodleighschool/stemma/plugin"
)

// IconOptions configures the icon method, which renders declared assets from
// the applications preparation selects.
type IconOptions struct {
	// Force replaces assets that already exist.
	Force bool
	// Size is the rendered edge in pixels; zero selects icon.Size.
	Size int
	// Renderer draws an application bundle's icon; nil selects the host renderer.
	Renderer func(ctx context.Context, app string, size int) ([]byte, error)
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
				return fmt.Errorf("resource %s: %w; run stemma icon %s to render it or commit the file", key, err, key)
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
// rendering, or "" when it does. Existing files stay unless forced, so
// committed artwork survives a catalog-wide run.
func iconOutcome(options IconOptions, root string, plan resourcePlan) string {
	if plan.Icon == "" {
		return "no icon declared"
	}
	if exists, err := icon.Exists(root, plan.Icon); err == nil && exists && !options.Force {
		return "exists"
	}
	return ""
}

// renderIcon writes the declared asset for one prepared resource and reports
// the outcome in the words the CLI prints.
func renderIcon(ctx context.Context, options IconOptions, root string, plan resourcePlan, outputs map[string]Prepared, work string) (string, error) {
	name := plan.Icon
	exists, err := icon.Exists(root, name)
	if err != nil {
		return "", err
	}
	installer, ok := outputs["installer"]
	if !ok || installer.Path == "" {
		return "no application", nil
	}
	if _, selected := installer.Evidence["macos.application"]; !selected {
		return "no application", nil
	}
	done := plugin.Stage(ctx, "Extracting application")
	bundle, err := macsoftware.Bundle(ctx, installer.artifact(), filepath.Join(work, "icon"))
	done(err)
	if err != nil {
		return "", err
	}
	render, size := options.Renderer, options.Size
	if render == nil {
		render = icon.Render
	}
	if size == 0 {
		size = icon.Size
	}
	done = plugin.Stage(ctx, "Rendering icon")
	data, err := render(ctx, bundle, size)
	done(err)
	if err != nil {
		return "", err
	}
	if err := icon.Write(root, name, data); err != nil {
		return "", err
	}
	if exists {
		return "replaced", nil
	}
	return "rendered", nil
}
