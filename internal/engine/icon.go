package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/inspect"
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
	// Input selects a locked input instead of the prepared installer.
	Input string
	// Path selects a file or application within Input.
	Path string
}

func iconName(options IconOptions, plan resourcePlan) string {
	if plan.Icon == "" && options.Input != "" {
		return plan.Resource.Metadata.Name
	}
	return plan.Icon
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
	name := iconName(options, plan)
	if name == "" {
		return "no icon declared"
	}
	if exists, err := icon.Exists(root, name); err == nil && exists && !options.Force {
		return "unchanged"
	}
	return ""
}

// createIcon writes the declared asset for one prepared resource and reports
// the outcome in the words the CLI prints: the presentation created, or why
// nothing could be.
func createIcon(ctx context.Context, options IconOptions, root string, plan resourcePlan, installer Prepared, work string) (string, error) {
	name := iconName(options, plan)
	if installer.Path == "" {
		return "no installer output", nil
	}
	workspace := filepath.Join(work, "icon")
	done := plugin.Stage(ctx, "Extracting artwork", plugin.Detail(installer.Filename))
	var subject icon.Subject
	var err error
	if options.Input != "" {
		subject, err = inputIconSubject(ctx, installer.artifact(), options.Path, workspace, options.Presentation)
	} else {
		subject, err = iconSubject(ctx, plan.Resource.Kind, installer.artifact(), workspace, options.Presentation)
	}
	done(err)
	if errors.Is(err, macsoftware.ErrNoApplication) {
		return "no application selected; choose an input with --input and --path", nil
	}
	if errors.Is(err, icon.ErrNoArtwork) {
		return "no artwork", nil
	}
	if err != nil {
		return "", err
	}
	done = plugin.Stage(ctx, "Presenting icon", plugin.Detail(string(options.Presentation)))
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

func inputIconSubject(ctx context.Context, input plugin.Artifact, selection, workspace string, presentation icon.Presentation) (icon.Subject, error) {
	contentWork := filepath.Join(workspace, "source")
	if err := os.MkdirAll(contentWork, 0o700); err != nil {
		return icon.Subject{}, err
	}
	source, err := contents.Open(ctx, input, contentWork)
	if err != nil {
		return icon.Subject{}, err
	}
	defer func() { _ = source.Close() }()
	if selection != "" {
		node, err := source.At(ctx, selection)
		if err != nil {
			return icon.Subject{}, err
		}
		info, err := node.Stat()
		if err != nil {
			return icon.Subject{}, err
		}
		if info.Mode().IsRegular() {
			local, err := node.Materialize(ctx, filepath.Join(workspace, "selection"))
			if err != nil {
				return icon.Subject{}, err
			}
			return inputIconSubject(ctx, plugin.Artifact{Path: local, Filename: filepath.Base(local)}, "", filepath.Join(workspace, "file"), presentation)
		}
	}
	facts, err := inspect.Selection(ctx, source, selection)
	if err != nil {
		return icon.Subject{}, err
	}
	var apps []plugin.Subject
	var names []string
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			apps = append(apps, subject)
			names = append(names, subject.Path)
		}
	}
	if len(apps) > 1 {
		return icon.Subject{}, fmt.Errorf("multiple applications; select an icon path: %s", strings.Join(names, ", "))
	}
	if len(apps) == 1 {
		return macsoftware.IconFromSource(ctx, source, apps[0], workspace, presentation)
	}
	node, err := source.At(ctx, selection)
	if err != nil {
		return icon.Subject{}, err
	}
	info, err := node.Stat()
	if err != nil {
		return icon.Subject{}, err
	}
	if !info.Mode().IsRegular() {
		return icon.Subject{}, errors.New("select an application, installer or artwork file with --path")
	}
	local, err := node.Materialize(ctx, filepath.Join(workspace, "selected"))
	if err != nil {
		return icon.Subject{}, err
	}
	input.Path, input.Tree = local, false
	file, err := os.Open(local)
	if err != nil {
		return icon.Subject{}, err
	}
	var header [8]byte
	n, readErr := file.Read(header[:])
	closeErr := file.Close()
	if readErr != nil && n == 0 {
		return icon.Subject{}, fmt.Errorf("read artwork: %w", readErr)
	}
	if closeErr != nil {
		return icon.Subject{}, closeErr
	}
	if bytes.HasPrefix(header[:n], []byte("MZ")) || bytes.Equal(header[:n], []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) {
		return windowssoftware.Icon(ctx, input)
	}
	if info.Size() > 32<<20 {
		return icon.Subject{}, errors.New("artwork file exceeds 32 MiB")
	}
	data, err := os.ReadFile(local)
	if err != nil {
		return icon.Subject{}, err
	}
	artwork, err := icon.FromImage(data)
	return icon.Subject{Artwork: artwork}, err
}

// inputIcon acquires the selected input; only resource inputs need preparation.
func (e *execution) inputIcon(ctx context.Context) error {
	key := e.roots[0]
	plan := e.plans[key]
	if outcome := iconOutcome(e.opts.Icons, e.session.root, plan); outcome != "" {
		e.report.Resources = append(e.report.Resources, ResourceReport{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Key: key, Icon: outcome})
		return e.complete(ctx, &e.report.Resources[len(e.report.Resources)-1])
	}
	e.pending[key] = 1
	if err := e.prepare(ctx, key); err != nil {
		return err
	}
	prepared := e.prepared[key]
	if !prepared.ready {
		return nil
	}
	item := &e.report.Resources[prepared.report]
	input, err := materialize(ctx, e.session.store, prepared.inputs[e.opts.Icons.Input], filepath.Join(prepared.work, "icon-input"))
	if err == nil {
		item.Icon, err = createIcon(resourceContext(ctx, plan.Resource), e.opts.Icons, e.session.root, plan, input, prepared.work)
	}
	if err != nil {
		item.Error = err.Error()
		e.fail(ctx, ResourceError{Resource: key, Err: err})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.complete(ctx, item)
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
