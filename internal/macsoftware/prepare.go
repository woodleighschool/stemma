package macsoftware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	source, err := contents.Open(ctx, input, workspace)
	if err != nil {
		return nil, err
	}
	defer func() { _ = source.Close() }()
	if spec.PackagePath != "" && !source.Traversable() {
		return nil, errors.New("package_path requires an archive source")
	}
	done := plugin.Stage(ctx, "Inspecting installer", plugin.Detail(input.Filename))
	inventory, err := inspect.Source(ctx, source)
	done(err)
	if err != nil {
		return nil, err
	}
	pkg, app, err := choose(spec, source.Traversable(), inventory)
	if err != nil {
		return nil, err
	}
	if spec.DiskImage != nil && (app == nil || source.IsImage() && input.ContentRoot == "") {
		return nil, errors.New("disk_image requires an application from an archive or tree")
	}
	var installer plugin.Artifact
	var verified []signature.Observation
	if app != nil {
		installer, app, verified, err = publishApplication(ctx, spec, request, source, inventory, *app)
	} else {
		installer, app, verified, err = publishPackage(ctx, spec, request, source, inventory, pkg)
	}
	if err != nil {
		return nil, err
	}
	installer.Version = installerVersion(installer.Facts)
	// Application selection and signing evidence belong to the prepared output.
	installer.Evidence = maps.Clone(input.Evidence)
	delete(installer.Evidence, "macos.application")
	delete(installer.Evidence, "macos.version_key")
	delete(installer.Evidence, "signatures")
	delete(installer.Evidence, signature.BuildEvidence)
	delete(installer.Evidence, signature.UnverifiedEvidence)
	if installer.Evidence == nil {
		installer.Evidence = map[string]json.RawMessage{}
	}
	if app != nil {
		installer.Version = appVersion(*app, spec.Application)
		installer.Evidence["macos.application"], _ = json.Marshal(app)
		installer.Evidence["macos.version_key"], _ = json.Marshal(versionKey(spec.Application, *app))
	}
	if verified != nil {
		installer.Evidence["signatures"], _ = json.Marshal(verified)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return map[string]plugin.Artifact{"installer": installer}, nil
}

// publishApplication publishes a vendor disk image as it is, or places an
// archive or tree application alone in a new one.
func publishApplication(ctx context.Context, spec Spec, request Request, source *contents.Source, inventory plugin.Facts, app plugin.Subject) (plugin.Artifact, *plugin.Subject, []signature.Observation, error) {
	input := request.Input
	retainImage := source.IsImage() && input.ContentRoot == ""
	selection := app.Path
	if app.ID == "." {
		selection = ""
	}
	node, err := source.At(ctx, selection)
	if err != nil {
		return plugin.Artifact{}, nil, nil, err
	}
	name := path.Base(node.Path)
	if options := spec.Application; options != nil && options.InstalledPath != "" {
		app.InstalledPath = options.InstalledPath
	}
	if app.InstalledPath == "" {
		app.InstalledPath = path.Join("/Applications", name)
	}
	var verified []signature.Observation
	if len(spec.Signatures) > 0 || request.DeriveSignature {
		var err error
		targets := []plugin.Subject{app}
		if retainImage {
			targets = topLevel(inventory)
		} else {
			// The selected archive app is published alone at the image root.
			targets[0].ID, targets[0].Path, targets[0].Parent = name, name, "."
		}
		verified, err = signature.Verify(ctx, spec.Signatures, targets, request.DeriveSignature, func(subject plugin.Subject) (signature.Result, error) {
			var result signature.Result
			var err error
			if retainImage {
				result, err = apple.VerifySubject(ctx, source, subject, request.Workspace)
			} else {
				result, err = apple.VerifyAppFS(ctx, node.FS, node.Path, signature.Signer{})
			}
			return result, err
		})
		if err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
	}
	var installer plugin.Artifact
	if retainImage {
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
		if installer, err = writeImage(ctx, node, request.Workspace, spec.DiskImage.compression()); err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		app.ID, app.Path, app.Parent = name, name, "."
		installer.Facts = plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Path: ".", Kind: "container", SHA256: installer.SHA256}, app}}
	}
	installer.Format = "dmg"
	return installer, &app, verified, nil
}

// publishPackage retains a PKG source, or extracts the selected nested package.
func publishPackage(ctx context.Context, spec Spec, request Request, source *contents.Source, inventory plugin.Facts, pkg string) (plugin.Artifact, *plugin.Subject, []signature.Observation, error) {
	local, facts := request.Input.Path, inventory
	if pkg != "." {
		node, err := source.At(ctx, pkg)
		if err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		local = node.Local
		if local == "" {
			done := plugin.Stage(ctx, "Extracting package from disk image", plugin.Detail(path.Base(pkg)))
			local, err = node.Materialize(ctx, filepath.Join(request.Workspace, "expanded"))
			done(err)
			if err != nil {
				return plugin.Artifact{}, nil, nil, err
			}
		}
		done := plugin.Stage(ctx, "Inspecting package", plugin.Detail(path.Base(pkg)))
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
	// A package its builder left unsigned has no publisher to derive.
	built := pkg == "." && signature.BuiltUnsigned(request.Input)
	enforce := !request.DeriveSignature && len(spec.Signatures) > 0
	derive := request.DeriveSignature && !built
	var verified []signature.Observation
	if enforce || derive {
		verified, err = signature.Verify(ctx, spec.Signatures, facts.Subjects[:1], derive, func(plugin.Subject) (signature.Result, error) {
			return apple.VerifyPackage(ctx, local, signature.Signer{})
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

// writeImage places the selected application alone at the root of a new disk
// image. The image is our container around the publisher's software and carries
// no signature of its own. A fixed date keeps the image a function of the
// application and compression alone.
func writeImage(ctx context.Context, node contents.Node, workspace string, compression diskimage.Compression) (plugin.Artifact, error) {
	output := filepath.Join(workspace, strings.TrimSuffix(path.Base(node.Path), path.Ext(node.Path))+".dmg")
	if err := diskimage.WriteApplication(ctx, node.FS, node.Path, output, compression, time.Unix(0, 0).UTC()); err != nil {
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
