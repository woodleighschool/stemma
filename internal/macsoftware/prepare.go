package macsoftware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

// Request carries the leased input and workspace for one preparation.
// DeriveSignature verifies the published artifact against its observed signer
// instead of the configured one and reports it as evidence.
type Request struct {
	Input           plugin.Artifact
	Workspace       string
	DeriveSignature bool
}

// Prepare retains vendor installer bytes and places a selected archive
// application in a disk image. It never executes applications or hooks.
func Prepare(ctx context.Context, spec Spec, request Request) (map[string]plugin.Artifact, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	input, workspace := request.Input, request.Workspace
	if input.Path == "" {
		return map[string]plugin.Artifact{}, nil
	}
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(input.Path) {
		return nil, errors.New("preparation requires absolute workspace and leased input paths")
	}
	source, err := contents.Open(input, workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	if spec.PackagePath != "" && !source.Traversable() {
		return nil, errors.New("package_path requires an archive source")
	}
	done := plugin.Stage(ctx, "Inspecting installer")
	inventory, err := inspect.Source(ctx, source)
	done(err)
	if err != nil {
		return nil, err
	}
	pkg, app, err := choose(spec, source.Traversable(), inventory)
	if err != nil {
		return nil, err
	}
	var installer plugin.Artifact
	var verified *signature.Result
	if app != nil {
		installer, app, verified, err = publishApplication(ctx, spec, request, source, inventory, *app)
	} else {
		installer, app, verified, err = publishPackage(ctx, spec, request, source, inventory, pkg)
	}
	if err != nil {
		return nil, err
	}
	installer.Version = installerVersion(installer.Facts)
	installer.Evidence = maps.Clone(input.Evidence)
	if installer.Evidence == nil {
		installer.Evidence = map[string]json.RawMessage{}
	}
	if app != nil {
		installer.Version = appVersion(*app, spec.Application)
		installer.Evidence["macos.application"], _ = json.Marshal(app)
		installer.Evidence["macos.version_key"], _ = json.Marshal(versionKey(spec.Application, *app))
	}
	if verified != nil {
		installer.Evidence["signature"], _ = json.Marshal(verified)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return map[string]plugin.Artifact{"installer": installer}, nil
}

// publishApplication publishes a vendor disk image as it is, or places an
// archive or tree application alone in a new one.
func publishApplication(ctx context.Context, spec Spec, request Request, source *contents.Source, inventory plugin.Facts, app plugin.Subject) (plugin.Artifact, *plugin.Subject, *signature.Result, error) {
	input := request.Input
	name, local := path.Base(app.Path), input.Path
	var node contents.Node
	if app.ID == "." {
		name = filepath.Base(input.Path)
	} else {
		var err error
		if node, err = source.At(ctx, app.Path); err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		local = node.Local
	}
	if options := spec.Application; options != nil && options.InstalledPath != "" {
		app.InstalledPath = options.InstalledPath
	}
	if app.InstalledPath == "" {
		app.InstalledPath = path.Join("/Applications", name)
	}
	var verified *signature.Result
	if spec.Signature != nil || request.DeriveSignature {
		var err error
		verified, err = verify(ctx, spec, func(want signature.Signer) (signature.Result, error) {
			if source.IsImage() {
				return verifyApplications(ctx, node.FS, topLevel(inventory), want)
			}
			return apple.VerifyApp(ctx, local, want)
		})
		if err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
	}
	var installer plugin.Artifact
	var err error
	if source.IsImage() {
		if installer, err = retain(ctx, input.Path, input.Filename, request.Workspace); err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		installer.Facts = plugin.Facts{Version: plugin.FactsVersion, Subjects: slices.Clone(inventory.Subjects)}
		for i, subject := range installer.Facts.Subjects {
			switch subject.ID {
			case ".":
				installer.Facts.Subjects[i].SHA256 = installer.SHA256
			case app.ID:
				installer.Facts.Subjects[i] = app
			}
		}
	} else {
		if installer, err = writeImage(ctx, local, request.Workspace); err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		app.ID, app.Path, app.Parent = name, name, "."
		installer.Facts = plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Path: ".", Kind: "container", SHA256: installer.SHA256}, app}}
	}
	installer.Format = "dmg"
	return installer, &app, verified, nil
}

// publishPackage retains a PKG source, or extracts the selected nested package.
func publishPackage(ctx context.Context, spec Spec, request Request, source *contents.Source, inventory plugin.Facts, pkg string) (plugin.Artifact, *plugin.Subject, *signature.Result, error) {
	local, facts := request.Input.Path, inventory
	if pkg != "." {
		node, err := source.At(ctx, pkg)
		if err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		local = node.Local
		if local == "" {
			done := plugin.Stage(ctx, "Extracting package from disk image")
			local, err = node.Materialize(ctx, filepath.Join(request.Workspace, "expanded"))
			done(err)
			if err != nil {
				return plugin.Artifact{}, nil, nil, err
			}
		}
		done := plugin.Stage(ctx, "Inspecting package")
		facts, err = inspect.Read(ctx, local)
		done(err)
		if err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
	}
	app, err := selectApp(facts, spec.Application)
	if err != nil {
		return plugin.Artifact{}, nil, nil, err
	}
	if options := spec.Application; app != nil && options != nil && options.InstalledPath != "" {
		app.InstalledPath = options.InstalledPath
	}
	var verified *signature.Result
	if spec.Signature != nil || request.DeriveSignature {
		verified, err = verify(ctx, spec, func(want signature.Signer) (signature.Result, error) {
			return apple.VerifyPackage(ctx, local, want)
		})
		if err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
	}
	installer, err := retain(ctx, local, filepath.Base(local), request.Workspace)
	if err != nil {
		return plugin.Artifact{}, nil, nil, err
	}
	installer.Format, installer.Facts = "pkg", facts
	return installer, app, verified, nil
}

// verifyApplications verifies every application of a vendor disk image
// outside another application, which must share one signer. The result names
// them all.
func verifyApplications(ctx context.Context, fsys fs.ReadLinkFS, apps []plugin.Subject, want signature.Signer) (signature.Result, error) {
	var result signature.Result
	var targets []string
	for _, app := range apps {
		observed, err := apple.VerifyAppFS(ctx, fsys, app.Path, want)
		if err != nil {
			return signature.Result{}, fmt.Errorf("%s: %w", app.Path, err)
		}
		if len(targets) == 0 {
			result = observed
			if want, err = signature.Parse(observed.Signer); err != nil {
				return signature.Result{}, err
			}
		}
		targets = append(targets, observed.Target)
	}
	result.Target = strings.Join(targets, ", ")
	return result, nil
}

// verify checks a signature against the declared signer, or derives the
// observed signer when none is declared.
func verify(ctx context.Context, spec Spec, check func(signature.Signer) (signature.Result, error)) (*signature.Result, error) {
	var want signature.Signer
	if spec.Signature != nil {
		var err error
		if want, err = signature.Parse(spec.Signature.Signer); err != nil {
			return nil, err
		}
	}
	done := plugin.Stage(ctx, "Verifying signature")
	result, err := check(want)
	done(err)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// writeImage places the selected application alone at the root of a new disk
// image. The image is our container around the publisher's software and carries
// no signature of its own. A fixed date keeps the image a function of the
// application alone.
func writeImage(ctx context.Context, app, workspace string) (plugin.Artifact, error) {
	output := filepath.Join(workspace, strings.TrimSuffix(filepath.Base(app), filepath.Ext(app))+".dmg")
	if err := diskimage.WriteApplication(ctx, app, output, time.Unix(0, 0).UTC()); err != nil {
		return plugin.Artifact{}, err
	}
	return describeArtifact(ctx, output, "dmg")
}

func retain(ctx context.Context, source, filename, workspace string) (plugin.Artifact, error) {
	if filename == "" || filepath.Base(filename) != filename {
		return plugin.Artifact{}, errors.New("installer requires a base filename")
	}
	destination := filepath.Join(workspace, filename)
	if filepath.Clean(source) == filepath.Clean(destination) {
		return describeArtifact(ctx, destination, strings.TrimPrefix(filepath.Ext(filename), "."))
	}
	in, err := os.Open(source)
	if err != nil {
		return plugin.Artifact{}, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return plugin.Artifact{}, err
	}
	_, copyErr := io.Copy(out, fileio.Reader{Context: ctx, Reader: in})
	if err := errors.Join(copyErr, out.Close()); err != nil {
		_ = os.Remove(destination)
		return plugin.Artifact{}, err
	}
	return describeArtifact(ctx, destination, strings.TrimPrefix(filepath.Ext(filename), "."))
}

func describeArtifact(ctx context.Context, name, format string) (plugin.Artifact, error) {
	file, err := os.Open(name)
	if err != nil {
		return plugin.Artifact{}, err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, fileio.Reader{Context: ctx, Reader: file})
	if err != nil {
		return plugin.Artifact{}, err
	}
	return plugin.Artifact{Path: name, Filename: filepath.Base(name), Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), Format: format}, nil
}

func versionKey(options *Application, subject plugin.Subject) string {
	if options != nil && options.VersionKey != "" {
		return options.VersionKey
	}
	return subject.App.VersionKey()
}

func appVersion(subject plugin.Subject, options *Application) string {
	if versionKey(options, subject) == "CFBundleVersion" {
		return subject.App.Build
	}
	return subject.App.Version
}

// installerVersion uses a Distribution's product version, otherwise the version
// shared by every component receipt.
func installerVersion(facts plugin.Facts) string {
	for _, subject := range facts.Subjects {
		if subject.Installer != nil && subject.Installer.Version != "" {
			return subject.Installer.Version
		}
	}
	version := ""
	for _, subject := range facts.Subjects {
		if subject.Package == nil {
			continue
		}
		candidate := subject.Package.Version
		if candidate == "" || version != "" && version != candidate {
			return ""
		}
		version = candidate
	}
	return version
}
