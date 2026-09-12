package macsoftware

import (
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
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/plugin"
)

// Prepare retains vendor installer bytes and wraps selected archive applications
// in an unsigned component package. It never executes applications or hooks.
func Prepare(ctx context.Context, spec Spec, input plugin.Artifact, workspace string, timestamp time.Time) (map[string]plugin.Artifact, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if input.Path == "" {
		return map[string]plugin.Artifact{}, nil
	}
	if !filepath.IsAbs(workspace) || !filepath.IsAbs(input.Path) {
		return nil, errors.New("preparation requires absolute workspace and leased input paths")
	}
	selected, archivePath, dmg, err := selectPayload(ctx, spec, input, workspace)
	if err != nil {
		return nil, err
	}
	facts, err := inspect.Read(ctx, selected)
	if err != nil {
		return nil, err
	}
	options := spec.Application
	if archivePath != "" && options != nil && options.Path == archivePath {
		copyOptions := *options
		copyOptions.Path = "."
		options = &copyOptions
	}
	app, err := selectApp(facts, options)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(selected)
	if err != nil {
		return nil, err
	}
	if info.IsDir() && (app == nil || !strings.EqualFold(filepath.Ext(selected), ".app")) {
		return nil, errors.New("selected tree must be one application bundle")
	}
	appPath := ""
	if info.IsDir() {
		appPath = selected
	}
	if app != nil {
		if options != nil && options.InstalledPath != "" {
			app.InstalledPath = options.InstalledPath
		}
		if info.IsDir() && app.InstalledPath == "" {
			app.InstalledPath = path.Join("/Applications", filepath.Base(selected))
		}
	}
	var verification *apple.Evidence
	if spec.Verification.Subject != "installer" {
		verification, err = verifySelected(spec.Verification, input.Path, selected, appPath)
	}
	if err != nil {
		return nil, err
	}
	var installer plugin.Artifact
	switch {
	case info.IsDir() && dmg:
		installer, err = retain(ctx, input.Path, input.Filename, workspace)
		installer.Format = "dmg"
		app.ID, app.Path, app.Parent = archivePath, archivePath, "."
		facts = plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{{ID: ".", Path: ".", Kind: "container", SHA256: installer.SHA256}, *app}}
	case info.IsDir():
		installer, err = wrapApp(ctx, selected, *app, options, workspace, timestamp)
		if err == nil {
			facts, err = inspect.Read(ctx, installer.Path)
			if err == nil {
				app, err = selectApp(facts, &Application{BundleID: app.App.BundleID, InstalledPath: app.InstalledPath, VersionKey: versionKey(options)})
			}
		}
	default:
		if !strings.EqualFold(filepath.Ext(selected), ".pkg") {
			return nil, errors.New("macOS software requires an application, PKG or DMG installer")
		}
		installer, err = retain(ctx, selected, filepath.Base(selected), workspace)
		installer.Format = "pkg"
	}
	if err != nil {
		return nil, err
	}
	if spec.Verification.Subject == "installer" {
		verification, err = verifySelected(spec.Verification, input.Path, installer.Path, appPath)
		if err != nil {
			return nil, err
		}
	}
	installer.Facts = facts
	installer.Version = receiptVersion(facts)
	installer.Evidence = maps.Clone(input.Evidence)
	if installer.Evidence == nil {
		installer.Evidence = map[string]json.RawMessage{}
	}
	if app != nil {
		installer.Version = appVersion(*app, options)
		installer.Evidence["macos.application"], _ = json.Marshal(app)
		installer.Evidence["macos.version_key"], _ = json.Marshal(versionKey(options))
	}
	if verification != nil {
		installer.Evidence["macos.verification"], _ = json.Marshal(verification)
	}
	outputs := map[string]plugin.Artifact{"installer": installer}
	if appPath != "" {
		artwork, err := applicationIcon(ctx, appPath, workspace)
		if err != nil {
			return nil, err
		}
		if artwork.Path != "" {
			outputs["icon"] = artwork
		}
	}
	return outputs, nil
}

func selectPayload(ctx context.Context, spec Spec, input plugin.Artifact, workspace string) (string, string, bool, error) {
	selection := spec.PackagePath
	if selection == "" && spec.Application != nil {
		selection = spec.Application.Path
	}
	ext := strings.ToLower(filepath.Ext(input.Filename))
	if input.Tree {
		if strings.EqualFold(filepath.Ext(input.Path), ".app") && (selection == "" || selection == ".") {
			return input.Path, "", false, nil
		}
		selected, err := archive.Select(input.Path, selection)
		relative, _ := filepath.Rel(input.Path, selected)
		return selected, filepath.ToSlash(relative), false, err
	}
	expanded := filepath.Join(workspace, "expanded")
	if ext == ".dmg" || input.Format == "dmg" {
		selected, err := diskimage.Extract(ctx, input.Path, expanded, selection)
		relative, _ := filepath.Rel(expanded, selected)
		return selected, filepath.ToSlash(relative), true, err
	}
	if isArchive(input.Filename) {
		if err := archive.Extract(ctx, input.Path, expanded); err != nil {
			return "", "", false, err
		}
		selected, err := archive.Select(expanded, selection)
		relative, _ := filepath.Rel(expanded, selected)
		return selected, filepath.ToSlash(relative), false, err
	}
	if spec.PackagePath != "" {
		return "", "", false, errors.New("package_path requires an archive source")
	}
	return input.Path, "", false, nil
}

func selectApp(facts plugin.Facts, options *Application) (*plugin.Subject, error) {
	if options != nil && (options.Path != "" || options.BundleID != "") {
		subject, err := plugin.SelectSubject(facts, plugin.SubjectSelector{Kind: "app", Path: options.Path, BundleID: options.BundleID})
		if err != nil {
			return nil, err
		}
		return &subject, nil
	}
	appIDs := map[string]bool{}
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			appIDs[subject.ID] = true
		}
	}
	var selected *plugin.Subject
	for _, subject := range facts.Subjects {
		if subject.App == nil || appIDs[subject.Parent] {
			continue
		}
		if selected != nil {
			return nil, errors.New("multiple applications observed; select application.path or application.bundle_id")
		}
		selected = &subject
	}
	if options != nil && selected == nil {
		return nil, errors.New("application selector matched no application")
	}
	return selected, nil
}

func wrapApp(ctx context.Context, source string, subject plugin.Subject, options *Application, workspace string, timestamp time.Time) (plugin.Artifact, error) {
	version := appVersion(subject, options)
	if subject.App.BundleID == "" || version == "" {
		return plugin.Artifact{}, errors.New("application packaging requires bundle identifier and selected version")
	}
	if timestamp.IsZero() {
		timestamp = time.Unix(0, 0).UTC()
	}
	filename := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source)) + ".pkg"
	output := filepath.Join(workspace, filename)
	if err := pkgbuild.Build(ctx, source, output, pkgbuild.Options{Identifier: subject.App.BundleID, Version: version, InstallLocation: subject.InstalledPath, Payload: ".", Timestamp: timestamp}); err != nil {
		return plugin.Artifact{}, err
	}
	return describeArtifact(ctx, output, "pkg")
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

func isArchive(name string) bool {
	name = strings.ToLower(name)
	return strings.HasSuffix(name, ".zip") || strings.HasSuffix(name, ".tar") || strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".tgz")
}

func versionKey(options *Application) string {
	if options != nil && options.VersionKey != "" {
		return options.VersionKey
	}
	return "CFBundleShortVersionString"
}

func appVersion(subject plugin.Subject, options *Application) string {
	if versionKey(options) == "CFBundleVersion" {
		return subject.App.Build
	}
	return subject.App.Version
}

func receiptVersion(facts plugin.Facts) string {
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

func verifySelected(v Verification, source, selected, app string) (*apple.Evidence, error) {
	if !v.Integrity && !v.Signature && !v.Resources && !v.Identity && !v.Platform && v.CertificateSHA256 == "" {
		return nil, nil
	}
	target := selected
	if v.Subject == "source" {
		target = source
	}
	if v.Subject == "application" {
		if app == "" {
			return nil, errors.New("application verification requires an extracted application")
		}
		target = app
	}
	policy := apple.Policy{RequireIntegrity: v.Integrity, RequireSignature: v.Signature, RequireResources: v.Resources, RequireIdentity: v.Identity, RequirePlatform: v.Platform, CertificateSHA256: v.CertificateSHA256}
	var evidence apple.Evidence
	var err error
	switch strings.ToLower(filepath.Ext(target)) {
	case ".pkg":
		evidence, err = apple.VerifyPackage(target, policy)
	case ".app":
		evidence, err = apple.VerifyApp(target, policy)
	default:
		return nil, fmt.Errorf("verification is unsupported for %s", filepath.Ext(target))
	}
	return &evidence, err
}
