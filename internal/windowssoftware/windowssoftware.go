// Package windowssoftware prepares vendor installers and their accompanying files.
package windowssoftware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/authenticode"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/icon"
	inspection "github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

type Spec struct {
	Source  plugin.Input `json:"source,omitzero" yaml:"source" jsonschema:"required" jsonschema_description:"Vendor installer or a resource output. A WindowsSoftware document requires a source."`
	Content *Content     `json:"content,omitempty" yaml:"content,omitempty" jsonschema_description:"Assemble a setup tree from the vendor installer and optional supporting files."`
	// Signature requires the setup file to carry a complete Authenticode
	// signature from the expected publisher.
	Signature *signature.Policy `json:"signature,omitempty" yaml:"signature,omitempty" jsonschema_description:"Require a complete Authenticode signature from the configured publisher on the setup file."`
	// Icon names the catalog asset icons/<name>.png that destinations publish;
	// documents that name the same asset share it.
	Icon         string                    `json:"icon,omitempty" yaml:"icon,omitempty" jsonschema:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]*$,maxLength=128,description=Name of the icon asset icons/<name>.png that destinations publish. Create it with stemma icon or commit a square PNG."`
	Destinations map[string]map[string]any `json:"destinations" yaml:"destinations" jsonschema_description:"Native publication metadata keyed by a Project destination name."`
}

type Content struct {
	SetupFile string                  `json:"setup_file,omitempty" yaml:"setup_file,omitempty" jsonschema_description:"Relative Windows payload path of the executable to run. Required when the source tree has no selected entrypoint."`
	Files     map[string]plugin.Input `json:"files,omitempty" yaml:"files,omitempty" jsonschema_description:"Additional files keyed by their relative paths in the setup tree. Each value selects its own input."`
}

func (s Spec) Validate() error {
	if s.Source.Resolver == "" && s.Source.Resource == nil {
		return errors.New("WindowsSoftware requires one vendor source")
	}
	if err := validateContent(s.Content); err != nil {
		return err
	}
	if s.Signature != nil {
		signer, err := signature.Parse(s.Signature.Signer)
		if err != nil {
			return fmt.Errorf("signature: %w", err)
		}
		if signer.Scheme != signature.Authenticode {
			return errors.New("signature.signer must name an Authenticode publisher")
		}
	}
	if len(s.Destinations) == 0 {
		return errors.New("WindowsSoftware requires destinations")
	}
	if s.Icon != "" && !icon.ValidName(s.Icon) {
		return fmt.Errorf("icon must name an asset under %s/ without directories or extension", icon.Directory)
	}
	return nil
}

func relative(name string) bool {
	if name == "." || path.Clean(name) != name || !filepath.IsLocal(filepath.FromSlash(name)) || strings.ContainsAny(name, "\\:<>\"|?*\x00") {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		for _, r := range part {
			if r < 32 {
				return false
			}
		}
		stem, _, _ := strings.Cut(strings.ToUpper(part), ".")
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" || len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
			return false
		}
	}
	return true
}

// Prepare assembles the setup content. DeriveSignature verifies the setup
// file against its observed publisher instead of the configured one.
func Prepare(ctx context.Context, spec Spec, inputs map[string]plugin.Artifact, workspace string, deriveSignature bool) (artifacts map[string]plugin.Artifact, err error) {
	done := plugin.Stage(ctx, "Preparing Windows installer")
	defer func() { done(err) }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	source := inputs["source"]
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(source.Path) {
		return nil, errors.New("preparation requires absolute workspace and leased source paths")
	}
	if err := validateContent(spec.Content); err != nil {
		return nil, err
	}
	setup := source.Filename
	if source.Tree {
		setup = source.EntryPoint
	} else if !relative(source.Filename) || path.Base(source.Filename) != source.Filename {
		return nil, errors.New("vendor installer requires a safe base filename")
	}
	originalSetup := setup
	if spec.Content != nil && spec.Content.SetupFile != "" {
		setup = spec.Content.SetupFile
	}
	if !relative(setup) {
		return nil, errors.New("a setup directory requires a safe relative setup_file")
	}
	if !source.Tree && (spec.Content == nil || len(spec.Content.Files) == 0) && setup != source.Filename {
		return nil, errors.New("single-installer setup_file must match its filename")
	}
	artifact := source
	if setup != originalSetup {
		artifact.Version = ""
	}
	cleanup := func() {}
	if source.Tree || spec.Content != nil && len(spec.Content.Files) > 0 {
		if err := validateLayout(ctx, source, spec.Content, inputs); err != nil {
			return nil, err
		}
		stage, err := os.MkdirTemp(workspace, ".windows-")
		if err != nil {
			return nil, err
		}
		cleanup = func() { _ = os.RemoveAll(stage) }
		root := filepath.Join(stage, "setup")
		if err := assemble(ctx, root, stage, source, spec.Content, inputs); err != nil {
			cleanup()
			return nil, err
		}
		artifact.Path, artifact.Filename, artifact.Tree, artifact.Format = root, "setup", true, "directory"
		artifact.Size, artifact.SHA256, artifact.Mode = 0, "", 0
	}
	result, err := describe(ctx, artifact, setup)
	if err == nil && (spec.Signature != nil || deriveSignature) {
		err = verifySignature(ctx, spec, &result)
	}
	if err != nil {
		cleanup()
		return nil, err
	}
	return map[string]plugin.Artifact{"installer": result}, nil
}

func verifySignature(ctx context.Context, spec Spec, artifact *plugin.Artifact) error {
	var want signature.Signer
	if spec.Signature != nil {
		var err error
		if want, err = signature.Parse(spec.Signature.Signer); err != nil {
			return err
		}
	}
	setup := artifact.Path
	if artifact.Tree {
		setup = filepath.Join(artifact.Path, filepath.FromSlash(artifact.EntryPoint))
	}
	done := plugin.Stage(ctx, "Verifying signature")
	result, err := authenticode.Verify(ctx, setup, want)
	done(err)
	if err != nil {
		return err
	}
	if artifact.Evidence == nil {
		artifact.Evidence = map[string]json.RawMessage{}
	}
	artifact.Evidence["signature"], err = json.Marshal(result)
	return err
}

func assemble(ctx context.Context, root, workspace string, source plugin.Artifact, content *Content, inputs map[string]plugin.Artifact) error {
	if source.Tree {
		if err := copyTree(ctx, source.Path, root, workspace); err != nil {
			return err
		}
	} else {
		if err := copyFile(ctx, source.Path, filepath.Join(root, source.Filename)); err != nil {
			return err
		}
	}
	if content == nil {
		return nil
	}
	for _, name := range slices.Sorted(maps.Keys(content.Files)) {
		input := inputs["file:"+name]
		target := filepath.Join(root, filepath.FromSlash(name))
		if input.Tree {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := copyTree(ctx, input.Path, target, workspace); err != nil {
				return err
			}
		} else if err := copyFile(ctx, input.Path, target); err != nil {
			return err
		}
	}
	return nil
}

func describe(ctx context.Context, artifact plugin.Artifact, setup string) (plugin.Artifact, error) {
	installer := artifact.Path
	if artifact.Tree {
		installer = filepath.Join(artifact.Path, filepath.FromSlash(setup))
	}
	info, err := os.Lstat(installer)
	if err != nil {
		return artifact, fmt.Errorf("setup file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return artifact, errors.New("setup entry point must be a regular file")
	}
	facts, err := inspection.Read(ctx, installer)
	if err != nil {
		return artifact, err
	}
	artifact.Facts = facts
	artifact.Evidence = maps.Clone(artifact.Evidence)
	delete(artifact.Evidence, "windows.installer")
	for _, subject := range facts.Subjects {
		if subject.Parent != "" || subject.MSI == nil {
			continue
		}
		if artifact.Evidence == nil {
			artifact.Evidence = map[string]json.RawMessage{}
		}
		subject.Path = setup
		artifact.Evidence["windows.installer"], err = json.Marshal(subject)
		if err != nil {
			return artifact, err
		}
		artifact.Version = subject.MSI.ProductVersion
	}
	artifact.EntryPoint = setup
	return artifact, nil
}

func copyTree(ctx context.Context, source, target, workspace string) error {
	f, err := os.CreateTemp(workspace, "content-*.tar")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	err = archive.Pack(ctx, source, f)
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return archive.Extract(ctx, f.Name(), target)
}

func copyFile(ctx context.Context, source, target string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("content input must be a regular file")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(fileio.Reader{Context: ctx, Reader: in}, info.Size()+1))
	if err == nil && n != info.Size() {
		err = errors.New("content input changed during copy")
	}
	if err == nil {
		err = out.Chmod(info.Mode().Perm())
	}
	return errors.Join(err, out.Close())
}
