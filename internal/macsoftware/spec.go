// Package macsoftware prepares macOS installers and their selected application evidence.
package macsoftware

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

const Version = "stemma.macsoftware/14"

// Spec declares a macOS installer, how preparation selects from it and how
// destinations publish it.
type Spec struct {
	Source      *plugin.Input `json:"source,omitempty" yaml:"source,omitempty" jsonschema_description:"Installer input from a built-in or loaded resolver, or a named resource output. Omit for source-free destination policies."`
	Application *Application  `json:"application,omitempty" yaml:"application,omitempty" jsonschema_description:"Select the application that supplies version, detection and icon metadata, and that an archive publishes in a new disk image."`
	// PackagePath selects one installer by archive-relative path or glob.
	PackagePath string                  `json:"package_path,omitempty" yaml:"package_path,omitempty" jsonschema_description:"Archive-relative path or glob selecting one installer package. Selection must be unambiguous."`
	DiskImage   *DiskImage              `json:"disk_image,omitempty" yaml:"disk_image,omitempty" jsonschema_description:"How preparation encodes the disk image it creates for an application from an archive or tree."`
	Signatures  []signature.Expectation `json:"signatures,omitempty" yaml:"signatures,omitempty" jsonschema:"minItems=1" jsonschema_description:"Signing expectations for the published PKG or every top-level application in the published disk image. Omit to make no signing assertion; a package BuildMacPkg built needs none. Derive with stemma signature."`
	// MinimumOS replaces the installer's and the selected application's macOS
	// requirements for every destination.
	MinimumOS string `json:"minimum_os,omitempty" yaml:"minimum_os,omitempty" jsonschema:"pattern=^[0-9]+([.][0-9]+)?([.][0-9]+)?$" jsonschema_description:"Minimum macOS release, such as 14.0. Destinations receive this instead of the installer's and the selected application's requirements. Omit to use the latest of those."`
	// Icon names the catalog asset icons/<name>.png that destinations publish.
	Icon         string                            `json:"icon,omitempty" yaml:"icon,omitempty" jsonschema:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]*$,maxLength=128,description=Name of the icon asset icons/<name>.png that destinations publish. Create it with stemma icon or commit a square PNG."`
	Destinations map[string]map[string]any         `json:"destinations,omitempty" yaml:"destinations,omitempty" jsonschema_description:"Native publication metadata keyed by a Project destination name. Explicit values override derived values."`
	Subjects     map[string]plugin.SubjectSelector `json:"subjects,omitempty" yaml:"subjects,omitempty" jsonschema_description:"Named selectors exposed as facts in destination expressions. Unaliased subjects remain addressable by their observed ID."`
}

type Application struct {
	Path          string `json:"path,omitempty" yaml:"path,omitempty" jsonschema_description:"Archive-relative application path or glob. Must stay inside the input."`
	InstalledPath string `json:"installed_path,omitempty" yaml:"installed_path,omitempty" jsonschema_description:"Absolute application path on managed devices, used for packaging and detection."`
	BundleID      string `json:"bundle_id,omitempty" yaml:"bundle_id,omitempty" jsonschema_description:"Bundle identifier used to select one application when an input contains several."`
	VersionKey    string `json:"version_key,omitempty" yaml:"version_key,omitempty" jsonschema:"enum=CFBundleShortVersionString,enum=CFBundleVersion" jsonschema_description:"Info.plist key used as the managed version. Omit to prefer CFBundleShortVersionString, then CFBundleVersion."`
}

// DiskImage declares how preparation encodes the disk image it creates.
type DiskImage struct {
	Compression diskimage.Compression `json:"compression,omitempty" yaml:"compression,omitempty" jsonschema:"enum=lzfse,enum=zlib,enum=lzma" jsonschema_description:"Chunk compression: lzfse, zlib or lzma. Omit for lzfse. lzma builds the smallest image and costs the most CPU to build and read."`
}

// compression returns the declared compression, or LZFSE.
func (d *DiskImage) compression() diskimage.Compression {
	if d == nil || d.Compression == "" {
		return diskimage.LZFSE
	}
	return d.Compression
}

// Preparation keeps the fields that shape the installer; publication settings
// and the icon asset change without invalidating prepared outputs.
func (s Spec) Preparation() Spec {
	s.Source, s.Destinations, s.Icon = nil, nil, ""
	s.Subjects, s.MinimumOS = nil, ""
	return s
}

func (s Spec) Validate() error {
	if s.PackagePath != "" && !relativePath(s.PackagePath) {
		return errors.New("package_path must be a confined archive path")
	}
	if image := s.DiskImage; image != nil {
		switch image.Compression {
		case "", diskimage.LZFSE, diskimage.Zlib, diskimage.LZMA:
		default:
			return errors.New("disk_image.compression must be lzfse, zlib or lzma")
		}
	}
	if s.MinimumOS != "" && !macOSVersion.MatchString(s.MinimumOS) {
		return errors.New("minimum_os must be a macOS version such as 14.0")
	}
	if app := s.Application; app != nil {
		if app.Path != "" && !relativePath(app.Path) {
			return errors.New("application.path must be a confined archive path")
		}
		if app.InstalledPath != "" && (!path.IsAbs(app.InstalledPath) || path.Clean(app.InstalledPath) != app.InstalledPath || strings.ContainsAny(app.InstalledPath, "\\\x00\r\n\t")) {
			return errors.New("application.installed_path must be a clean absolute POSIX path")
		}
		if app.VersionKey != "" && app.VersionKey != "CFBundleVersion" && app.VersionKey != "CFBundleShortVersionString" {
			return errors.New("application.version_key must be CFBundleShortVersionString or CFBundleVersion")
		}
	}
	for i, expected := range s.Signatures {
		if err := expected.Validate(signature.AppleDeveloperID); err != nil {
			return fmt.Errorf("signatures[%d]: %w", i, err)
		}
	}
	if s.Icon != "" && !icon.ValidName(s.Icon) {
		return fmt.Errorf("icon must name an asset under %s/ without directories or extension", icon.Directory)
	}
	return nil
}

var macOSVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)

func relativePath(name string) bool {
	return fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00\r\n\t")
}

// Notices recommends a signing assertion for externally supplied software.
// Resource outputs own their input policies; built packages need no publisher assertion.
func (s Spec) Notices(identity plugin.ResourceReference, derive string) []plugin.Notice {
	if s.Source == nil || s.Source.Resource != nil || len(s.Signatures) > 0 || derive == "signature" {
		return nil
	}
	return []plugin.Notice{{
		Level: "warning", Code: "signature-expectation-missing",
		Message: "Source has no signature expectation",
		Hint:    fmt.Sprintf("Run `stemma signature %s/%s` to derive one.", identity.Kind, identity.Name),
	}}
}
