package macsoftware

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
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

// maxImageDepth bounds the disk images opened inside one another. Vendors nest
// one; the bound keeps a crafted image from being opened without end.
const maxImageDepth = 8

// Prepare retains vendor installer bytes, places a selected archive
// application in a disk image, or places the selected application in a
// declared component package. It never executes applications or hooks.
func Prepare(ctx context.Context, spec Spec, request Request) (map[string]plugin.Artifact, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if request.Input.Path == "" {
		return map[string]plugin.Artifact{}, nil
	}
	if !filepath.IsAbs(request.Workspace) || !filepath.IsAbs(request.Input.Path) {
		return nil, errors.New("preparation requires absolute workspace and leased input paths")
	}
	return prepare(ctx, spec, request, 0)
}

// prepare publishes from one source: the leased input, or a disk image depth
// levels inside it.
func prepare(ctx context.Context, spec Spec, request Request, depth int) (outputs map[string]plugin.Artifact, err error) {
	input, workspace := request.Input, request.Workspace
	source, err := contents.Open(ctx, input, workspace)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := source.Close(); closeErr != nil {
			err = closeErr
		}
	}()
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
	if diskImage(pkg) {
		outputs, err := prepareImage(ctx, spec, request, source, pkg, depth)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", pkg, err)
		}
		return outputs, nil
	}
	if spec.DiskImage != nil && (app == nil || source.IsImage() && input.ContentRoot == "") {
		return nil, errors.New("disk_image requires an application from an archive or tree")
	}
	if spec.Package != nil && app == nil {
		return nil, errors.New("package requires an application from an archive, tree or disk image; a vendor package is published as it is")
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
	if spec.Package != nil {
		// Preparation never signs, so the package's signing state is its own to
		// state, apart from the application's publisher.
		installer.Evidence[signature.BuildEvidence] = json.RawMessage(`"unsigned"`)
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

// prepareImage prepares a disk image found inside the source as the image
// alone would be prepared, keeping the input's evidence.
func prepareImage(ctx context.Context, spec Spec, request Request, source *contents.Source, name string, depth int) (map[string]plugin.Artifact, error) {
	if depth == maxImageDepth {
		return nil, errors.New("disk images are nested too deeply")
	}
	node, err := source.At(ctx, name)
	if err != nil {
		return nil, err
	}
	// The nested image belongs to the workspace, which outlives the source.
	local := filepath.Join(request.Workspace, "image")
	if node.Local == "" {
		done := plugin.Stage(ctx, "Extracting disk image", plugin.Detail(path.Base(name)))
		local, err = node.Materialize(ctx, local)
		done(err)
	} else {
		local, err = link(node.Local, local)
	}
	if err != nil {
		return nil, err
	}
	// A path that named the image is spent; every other selector applies inside it.
	if matchPath(spec.PackagePath, name) {
		spec.PackagePath = ""
	}
	request.Input = plugin.Artifact{Path: local, Filename: path.Base(name), Format: "dmg", Evidence: request.Input.Evidence}
	request.Workspace = filepath.Join(request.Workspace, "nested")
	if err := os.Mkdir(request.Workspace, 0o700); err != nil {
		return nil, err
	}
	return prepare(ctx, spec, request, depth+1)
}

// publishApplication publishes a vendor disk image as it is, places an archive
// or tree application alone in a new one, or places the selected application
// in a declared component package.
func publishApplication(ctx context.Context, spec Spec, request Request, source *contents.Source, inventory plugin.Facts, app plugin.Subject) (plugin.Artifact, *plugin.Subject, []signature.Observation, error) {
	input := request.Input
	retainImage := spec.Package == nil && source.IsImage() && input.ContentRoot == ""
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
	// published is the application's path in what preparation creates: alone at
	// an image's root, or at its installed path in a package's payload.
	published := name
	if spec.Package != nil {
		published = path.Join("Payload", app.InstalledPath)
	}
	var verified []signature.Observation
	if len(spec.Signatures) > 0 || request.DeriveSignature {
		var err error
		targets := []plugin.Subject{app}
		if retainImage {
			targets = topLevel(inventory)
		} else {
			targets[0].ID, targets[0].Path, targets[0].Parent = published, published, "."
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
	switch {
	case retainImage:
		if installer, err = retain(ctx, input); err != nil {
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
		installer.Format = "dmg"
	case spec.Package != nil:
		if installer, err = writePackage(ctx, node, app, appVersion(app, spec.Application), spec.Package, request.Workspace); err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		// The package is read back as a vendor package is, so destinations
		// derive from what it holds.
		done := plugin.Stage(ctx, "Inspecting package", plugin.Detail(installer.Filename))
		installer.Facts, err = inspect.Read(ctx, installer.Path, installer.SHA256)
		done(err)
		if err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		installed := slices.IndexFunc(installer.Facts.Subjects, func(subject plugin.Subject) bool {
			return subject.App != nil && subject.Path == published && subject.InstalledPath == app.InstalledPath
		})
		if installed < 0 {
			return plugin.Artifact{}, nil, nil, fmt.Errorf("the built package does not install %s", app.InstalledPath)
		}
		app = installer.Facts.Subjects[installed]
	default:
		if installer, err = writeImage(ctx, node, request.Workspace, spec.DiskImage.compression()); err != nil {
			return plugin.Artifact{}, nil, nil, err
		}
		app.ID, app.Path, app.Parent = published, published, "."
		installer.Facts = plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Path: ".", Kind: "container", SHA256: installer.SHA256}, app}}
		installer.Format = "dmg"
	}
	return installer, &app, verified, nil
}

// publishPackage retains a PKG source, or the selected nested package.
func publishPackage(ctx context.Context, spec Spec, request Request, source *contents.Source, inventory plugin.Facts, pkg string) (plugin.Artifact, *plugin.Subject, []signature.Observation, error) {
	facts := inventory
	var installer plugin.Artifact
	var err error
	if pkg == "." {
		installer, err = retain(ctx, request.Input)
	} else {
		installer, err = extractPackage(ctx, source, pkg, request.Workspace)
		if err == nil {
			done := plugin.Stage(ctx, "Inspecting package", plugin.Detail(path.Base(pkg)))
			facts, err = inspect.Read(ctx, installer.Path, installer.SHA256)
			done(err)
		}
	}
	if err != nil {
		return plugin.Artifact{}, nil, nil, err
	}
	local := installer.Path
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
	installer.Format, installer.Facts = "pkg", facts
	return installer, app, verified, nil
}

// extractPackage places a package the source holds in the workspace, which
// outlives the source: a disk image's member is extracted, and an archive or
// tree member is linked.
func extractPackage(ctx context.Context, source *contents.Source, pkg, workspace string) (plugin.Artifact, error) {
	node, err := source.At(ctx, pkg)
	if err != nil {
		return plugin.Artifact{}, err
	}
	local := filepath.Join(workspace, "expanded")
	if node.Local == "" {
		done := plugin.Stage(ctx, "Extracting package from disk image", plugin.Detail(path.Base(pkg)))
		local, err = node.Materialize(ctx, local)
		done(err)
	} else {
		local, err = link(node.Local, local)
	}
	if err != nil {
		return plugin.Artifact{}, err
	}
	return describeArtifact(ctx, local, "pkg")
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

// writePackage places the selected application at its installed path in a new
// component package. The declared identifier, or else the application's bundle
// identifier, names its receipt. Like a disk image preparation creates, the
// package is our container and carries no signature of its own, and a fixed
// date keeps it a function of the application and the declaration alone.
func writePackage(ctx context.Context, node contents.Node, app plugin.Subject, version string, options *Package, workspace string) (plugin.Artifact, error) {
	identifier := cmp.Or(options.Identifier, app.App.BundleID)
	switch {
	case identifier == "":
		return plugin.Artifact{}, errors.New("the selected application has no bundle identifier; set package.identifier")
	case version == "":
		return plugin.Artifact{}, errors.New("the selected application has no version for the package receipt")
	}
	root, err := os.MkdirTemp(workspace, ".package-*")
	if err != nil {
		return plugin.Artifact{}, err
	}
	defer func() { _ = os.RemoveAll(root) }()
	stage, err := pkgbuild.NewLayout(filepath.Join(root, "Payload"))
	if err != nil {
		return plugin.Artifact{}, err
	}
	if err := stage.Copy(ctx, node, strings.TrimPrefix(app.InstalledPath, "/"), pkgbuild.EntryMetadata{}); err != nil {
		return plugin.Artifact{}, err
	}
	output := filepath.Join(workspace, strings.TrimSuffix(path.Base(node.Path), path.Ext(node.Path))+".pkg")
	opts := pkgbuild.Options{Identifier: identifier, Version: version, Payload: "Payload", Compression: options.compression(), Metadata: stage.Metadata(), Timestamp: time.Unix(0, 0).UTC()}
	if err := pkgbuild.Build(ctx, root, output, opts); err != nil {
		return plugin.Artifact{}, err
	}
	return describeArtifact(ctx, output, "pkg")
}

// retain publishes the input as it is. A disk image extracted from another
// container has no digest yet; it already belongs to the workspace.
func retain(ctx context.Context, input plugin.Artifact) (plugin.Artifact, error) {
	if input.SHA256 == "" {
		return describeArtifact(ctx, input.Path, input.Format)
	}
	return plugin.Artifact{Path: input.Path, Filename: input.Filename, Size: input.Size, SHA256: input.SHA256, Format: input.Format}, nil
}

// link places a file an archive or tree holds in dir under its own name.
func link(name, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(dir, filepath.Base(name))
	return target, os.Link(name, target)
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
