package macpkg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/plugin"
)

// Build assembles a private layout from leased inputs and writes one unsigned
// component package. Input files and packaged endpoint scripts are never run.
func Build(ctx context.Context, spec Spec, inputs map[string]plugin.Artifact, workspace string, timestamp time.Time) (plugin.Artifact, error) {
	spec.Inputs = make(map[string]plugin.Input, len(inputs))
	for name := range inputs {
		spec.Inputs[name] = plugin.Input{}
	}
	if err := spec.Validate(); err != nil {
		return plugin.Artifact{}, err
	}
	if !filepath.IsAbs(workspace) {
		return plugin.Artifact{}, errors.New("package workspace must be absolute")
	}
	if timestamp.IsZero() {
		timestamp = time.Unix(0, 0).UTC()
	}
	root, err := os.MkdirTemp(workspace, ".macpkg-*")
	if err != nil {
		return plugin.Artifact{}, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	sources := map[string]*contents.Source{}
	defer func() {
		for _, source := range sources {
			_ = source.Close()
		}
	}()
	opts := pkgbuild.Options{Identifier: spec.Package.Identifier, Version: spec.Package.Version, Timestamp: timestamp}
	for _, area := range []struct {
		name    string
		entries map[string]Entry
	}{{"Payload", spec.Payload}, {"Scripts", spec.Scripts}} {
		if len(area.entries) == 0 {
			continue
		}
		stage := layout{root: filepath.Join(root, area.name), metadata: map[string]pkgbuild.EntryMetadata{}, claimed: map[string]bool{}}
		if err := stage.parents("."); err != nil {
			return plugin.Artifact{}, err
		}
		names := make([]string, 0, len(area.entries))
		for name := range area.entries {
			names = append(names, name)
		}
		slices.Sort(names)
		for _, name := range names {
			destination := name
			if area.name == "Payload" {
				destination, _ = payloadPath(name)
			}
			if err := stage.entry(ctx, destination, area.entries[name], inputs, sources, root); err != nil {
				return plugin.Artifact{}, fmt.Errorf("%s %q: %w", strings.ToLower(area.name), name, err)
			}
		}
		if area.name == "Payload" {
			opts.Payload = "Payload"
			opts.Metadata = stage.metadata
		} else {
			opts.Scripts = "Scripts"
			opts.ScriptMetadata = stage.metadata
		}
	}
	output := filepath.Join(workspace, spec.Filename())
	if err := pkgbuild.Build(ctx, root, output, opts); err != nil {
		return plugin.Artifact{}, err
	}
	artifact, err := describe(ctx, output, spec)
	if err != nil {
		_ = os.Remove(output)
		return plugin.Artifact{}, err
	}
	return artifact, nil
}

func describe(ctx context.Context, output string, spec Spec) (plugin.Artifact, error) {
	file, err := os.Open(output)
	if err != nil {
		return plugin.Artifact{}, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, fileio.Reader{Context: ctx, Reader: file})
	if err != nil {
		return plugin.Artifact{}, err
	}
	return plugin.Artifact{Path: output, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size, Filename: spec.Filename(), Version: spec.Package.Version, Format: "pkg", Facts: plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: "package", Kind: "package", Package: &plugin.PackageFacts{Identifier: spec.Package.Identifier, Version: spec.Package.Version, InstallLocation: "/", HasPayload: len(spec.Payload) > 0}}}}}, nil
}

func (stage *layout) entry(ctx context.Context, name string, entry Entry, inputs map[string]plugin.Artifact, sources map[string]*contents.Source, workspace string) error {
	mode, _ := entry.mode()
	attrs := pkgbuild.EntryMetadata{Mode: mode, UID: entry.UID, GID: entry.GID}
	if entry.Content != nil {
		return stage.content(ctx, name, strings.NewReader(*entry.Content), int64(len(*entry.Content)), attrs, 0o644)
	}
	if entry.Input == "" {
		return stage.directory(name, attrs)
	}
	source := sources[entry.Input]
	if source == nil {
		input, ok := inputs[entry.Input]
		if !ok {
			return fmt.Errorf("unknown input %q", entry.Input)
		}
		var err error
		source, err = contents.Open(input, workspace)
		if err != nil {
			return fmt.Errorf("input %q: %w", entry.Input, err)
		}
		sources[entry.Input] = source
	}
	node, err := source.At(ctx, entry.Path)
	if err != nil {
		return fmt.Errorf("input %q path %q: %w", entry.Input, entry.Path, err)
	}
	if err := stage.copy(ctx, node, name, attrs); err != nil {
		return fmt.Errorf("input %q path %q: %w", entry.Input, entry.Path, err)
	}
	return nil
}
