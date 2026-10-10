package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/config"
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
	// From is a local application, installer or artwork file that supplies one
	// resource's artwork in place of the software the resource prepares.
	From string
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
	name := plan.Icon
	if name == "" {
		return "no icon declared"
	}
	if exists, err := icon.Exists(root, name); err == nil && exists && !options.Force {
		return "unchanged"
	}
	return ""
}

// createIcon writes one resource's declared asset from its prepared installer,
// a selected input or a local source, and reports the outcome in the words the
// CLI prints: the presentation created, or why nothing could be.
func createIcon(ctx context.Context, options IconOptions, root string, plan resourcePlan, installer Prepared, selection InputSelection, work string) (string, error) {
	name := plan.Icon
	if installer.Path == "" {
		return "no installer output", nil
	}
	workspace := filepath.Join(work, "icon")
	done := plugin.Stage(ctx, "Extracting artwork", plugin.Detail(installer.Filename))
	var subject icon.Subject
	var err error
	if selection.Name != "" || options.From != "" {
		subject, err = inputIconSubject(ctx, installer.artifact(), selection.Path, workspace, options.Presentation)
	} else {
		subject, err = iconSubject(ctx, plan.Resource.Kind, installer.artifact(), workspace, options.Presentation)
	}
	done(err)
	if errors.Is(err, macsoftware.ErrNoApplication) && options.From == "" {
		return "no application selected; choose an input with --input and --path", nil
	}
	// A local source that holds no application has no artwork to offer.
	if errors.Is(err, icon.ErrNoArtwork) || errors.Is(err, macsoftware.ErrNoApplication) {
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

func inputIconSubject(ctx context.Context, input plugin.Artifact, selection, workspace string, presentation icon.Presentation) (result icon.Subject, err error) {
	defer func() {
		if err != nil && selection != "" {
			err = fmt.Errorf("icon path %q: %w", selection, err)
		}
	}()
	contentWork := filepath.Join(workspace, "source")
	if err := os.MkdirAll(contentWork, 0o700); err != nil {
		return icon.Subject{}, err
	}
	source, err := contents.Open(ctx, input, contentWork)
	if err != nil {
		return icon.Subject{}, err
	}
	defer func() { _ = source.Close() }()
	if selection != "" && !source.Traversable() {
		// A package is not a tree to walk; its inventory locates the application.
		facts, err := inspect.Source(ctx, source)
		if err != nil {
			return icon.Subject{}, err
		}
		var names []string
		for _, subject := range facts.Subjects {
			if subject.App == nil {
				continue
			}
			if subject.Path == selection {
				return macsoftware.IconFromSource(ctx, source, subject, workspace, presentation)
			}
			names = append(names, subject.Path)
		}
		if len(names) > 0 {
			return icon.Subject{}, fmt.Errorf("no such application; the package holds %s", inspect.FormatSubjectIDs(names))
		}
	}
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
		return icon.Subject{}, fmt.Errorf("multiple applications; select an icon path: %s", inspect.FormatSubjectIDs(names))
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
	if !info.Mode().IsRegular() || source.Traversable() {
		return icon.Subject{}, errors.New("select an application, installer or artwork file with --path")
	}
	local, err := node.Materialize(ctx, filepath.Join(workspace, "selected"))
	if err != nil {
		return icon.Subject{}, err
	}
	input.Path, input.Tree = local, false
	if root := facts.Subjects[0]; root.MSI != nil {
		return windowssoftware.Icon(ctx, input)
	} else if root.Kind == "container" {
		return icon.Subject{}, macsoftware.ErrNoApplication
	}
	artwork, err := icon.FromFile(ctx, local)
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
	return e.withInput(ctx, func(ctx context.Context, input Prepared, work string, item *ResourceReport) error {
		var err error
		item.Icon, err = createIcon(ctx, e.opts.Icons, e.session.root, plan, input, e.opts.Input, work)
		return err
	})
}

// iconFrom creates one resource's declared asset from a local application,
// installer or artwork file. The project supplies only the asset's name: no
// lock entry, cached input or plugin takes part, so the resource needs neither
// a source nor an earlier update.
func iconFrom(ctx context.Context, opts Options, report *Report) error {
	if opts.Input.Name != "" {
		return errors.New("from and input name different artwork sources")
	}
	project, err := config.Load(opts.ConfigPath)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return err
	}
	roots, err := selectResources(project.Resources, opts.Resources, opts.Profiles)
	if err != nil {
		return err
	}
	if len(roots) != 1 {
		return errors.New("from requires one resource selector")
	}
	key := roots[0]
	resource := project.Resources[key]
	ops, err := builtins(nil)
	if err != nil {
		return err
	}
	// Reading a plugin kind's declaration would mean loading its plugin.
	kinds := resourceKinds(ops)
	if _, builtin := kinds[plugin.ResourceKind{APIVersion: resource.APIVersion, Kind: resource.Kind}]; !builtin {
		return fmt.Errorf("resource %s: from supports only built-in kinds", key)
	}
	plan, err := discoverKind(ctx, ops, kinds, key, resource, false, "")
	if err != nil {
		return err
	}
	if plan.Icon == "" {
		return errors.New("selected resource must declare an icon")
	}
	// The path itself may be a link; what lies beneath it is read like any input.
	source, err := filepath.EvalSymlinks(opts.Icons.From)
	if err != nil {
		return err
	}
	if source, err = filepath.Abs(source); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	report.Resources = []ResourceReport{{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Key: key}}
	item := &report.Resources[0]
	var failure error
	if item.Icon = iconOutcome(opts.Icons, root, plan); item.Icon == "" {
		work, err := os.MkdirTemp("", "stemma-icon-*")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(work) }()
		local := Prepared{Path: source, Filename: filepath.Base(source), Tree: info.IsDir()}
		item.Icon, err = createIcon(resourceContext(ctx, plan.Resource), opts.Icons, root, plan, local, InputSelection{Path: opts.Input.Path}, work)
		if err != nil {
			item.Error = err.Error()
			failure = ResourceError{Resource: key, Err: err}
		}
	}
	if item.Icon == "unchanged" || strings.HasPrefix(item.Icon, "created ") {
		item.IconPath = icon.Relative(plan.Icon)
	}
	if opts.ResourceDone != nil {
		if err := opts.ResourceDone(*item); err != nil {
			return err
		}
	}
	return failure
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
