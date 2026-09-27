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
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/plugin"
)

// Build assembles a private layout from leased inputs and writes one unsigned
// component package. Input files and packaged endpoint scripts are never run.
func Build(ctx context.Context, spec Spec, inputs map[string]plugin.Artifact, workspace string) (plugin.Artifact, error) {
	sources := newSources(inputs, workspace)
	defer sources.close()
	return build(ctx, spec, sources, workspace)
}

func build(ctx context.Context, spec Spec, sources *sources, workspace string) (plugin.Artifact, error) {
	spec.Inputs = make(map[string]plugin.Input, len(sources.inputs))
	for name := range sources.inputs {
		spec.Inputs[name] = plugin.Input{}
	}
	if err := spec.Validate(); err != nil {
		return plugin.Artifact{}, err
	}
	if !filepath.IsAbs(workspace) {
		return plugin.Artifact{}, errors.New("package workspace must be absolute")
	}
	root, err := os.MkdirTemp(workspace, ".macpkg-*")
	if err != nil {
		return plugin.Artifact{}, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	// A fixed date keeps the package a function of its inputs alone.
	opts := pkgbuild.Options{Identifier: spec.Package.Identifier, Version: spec.Package.Version, Timestamp: time.Unix(0, 0).UTC()}
	for _, area := range []string{"Payload", "Scripts"} {
		names := make([]string, 0)
		if area == "Payload" {
			for name := range spec.Payload {
				names = append(names, name)
			}
		} else {
			for name := range spec.Scripts {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			continue
		}
		stage := layout{root: filepath.Join(root, area), metadata: map[string]pkgbuild.EntryMetadata{}, claimed: map[string]bool{}}
		if err := stage.parents("."); err != nil {
			return plugin.Artifact{}, err
		}
		slices.Sort(names)
		for _, name := range names {
			var err error
			if area == "Payload" {
				destination, _ := payloadPath(name)
				err = stage.entry(ctx, destination, spec.Payload[name], sources)
			} else {
				err = stage.script(ctx, name, spec.Scripts[name], sources)
			}
			if err != nil {
				return plugin.Artifact{}, fmt.Errorf("%s %q: %w", strings.ToLower(area), name, err)
			}
		}
		if area == "Payload" {
			opts.Payload, opts.Metadata = area, stage.metadata
		} else {
			opts.Scripts, opts.ScriptMetadata = area, stage.metadata
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

func (stage *layout) entry(ctx context.Context, name string, entry Entry, sources *sources) error {
	mode, _ := entry.mode()
	attrs := pkgbuild.EntryMetadata{Mode: mode, UID: entry.UID, GID: entry.GID}
	if entry.Content != nil {
		return stage.content(ctx, name, strings.NewReader(*entry.Content), int64(len(*entry.Content)), attrs, 0o644)
	}
	if entry.Input == "" {
		return stage.directory(name, attrs)
	}
	return stage.input(ctx, name, entry.Input, entry.Path, sources, attrs)
}

func (stage *layout) script(ctx context.Context, name string, script Script, sources *sources) error {
	if script.Content != nil {
		return stage.content(ctx, name, strings.NewReader(*script.Content), int64(len(*script.Content)), pkgbuild.EntryMetadata{}, 0o644)
	}
	return stage.input(ctx, name, script.Input, script.Path, sources, pkgbuild.EntryMetadata{})
}

func (stage *layout) input(ctx context.Context, name, inputName, selection string, sources *sources, attrs pkgbuild.EntryMetadata) error {
	source, err := sources.get(inputName)
	if err != nil {
		return err
	}
	node, err := source.At(ctx, selection)
	if err != nil {
		return fmt.Errorf("input %q path %q: %w", inputName, selection, err)
	}
	if err := stage.copy(ctx, node, name, attrs); err != nil {
		return fmt.Errorf("input %q path %q: %w", inputName, selection, err)
	}
	return nil
}

// sources opens and inspects each leased input once, for expressions,
// verification and composition.
type sources struct {
	inputs    map[string]plugin.Artifact
	workspace string
	open      map[string]*contents.Source
	facts     map[string]plugin.Facts
}

func newSources(inputs map[string]plugin.Artifact, workspace string) *sources {
	return &sources{inputs: inputs, workspace: workspace, open: map[string]*contents.Source{}, facts: map[string]plugin.Facts{}}
}

func (s *sources) inventory(ctx context.Context, name string) (plugin.Facts, error) {
	if facts, ok := s.facts[name]; ok {
		return facts, nil
	}
	source, err := s.get(name)
	if err != nil {
		return plugin.Facts{}, err
	}
	done := plugin.Stage(ctx, "Inspecting input", plugin.Detail(name))
	facts, err := inspect.Source(ctx, source)
	done(err)
	if err != nil {
		return plugin.Facts{}, err
	}
	s.facts[name] = facts
	return facts, nil
}

func (s *sources) get(name string) (*contents.Source, error) {
	if source := s.open[name]; source != nil {
		return source, nil
	}
	input, ok := s.inputs[name]
	if !ok {
		return nil, fmt.Errorf("unknown input %q", name)
	}
	source, err := contents.Open(input, s.workspace)
	if err != nil {
		return nil, fmt.Errorf("input %q: %w", name, err)
	}
	s.open[name] = source
	return source, nil
}

func (s *sources) close() {
	for _, source := range s.open {
		_ = source.Close()
	}
}
