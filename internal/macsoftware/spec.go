// Package macsoftware prepares macOS installers and their selected application evidence.
package macsoftware

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

const Version = "stemma.macsoftware/5"

type Spec struct {
	Source      *plugin.Input `json:"source,omitempty" yaml:"source,omitempty"`
	Application *Application  `json:"application,omitempty" yaml:"application,omitempty"`
	// PackagePath selects one installer by archive-relative path or glob.
	PackagePath string `json:"package_path,omitempty" yaml:"package_path,omitempty"`
	// Signature requires the published PKG or selected application to carry a
	// complete Developer ID signature from the expected team.
	Signature *signature.Policy `json:"signature,omitempty" yaml:"signature,omitempty"`
	// Icon names the catalog asset icons/<name>.png that destinations publish.
	Icon         string                    `json:"icon,omitempty" yaml:"icon,omitempty" jsonschema:"pattern=^[A-Za-z0-9][A-Za-z0-9._-]*$,maxLength=128,description=Name of the icon asset icons/<name>.png that destinations publish. Render it with stemma icon or commit a square PNG."`
	Destinations map[string]map[string]any `json:"destinations,omitempty" yaml:"destinations,omitempty"`
}

type Application struct {
	Path          string `json:"path,omitempty" yaml:"path,omitempty"`
	InstalledPath string `json:"installed_path,omitempty" yaml:"installed_path,omitempty"`
	BundleID      string `json:"bundle_id,omitempty" yaml:"bundle_id,omitempty"`
	VersionKey    string `json:"version_key,omitempty" yaml:"version_key,omitempty" jsonschema:"enum=CFBundleShortVersionString,enum=CFBundleVersion"`
}

// Preparation keeps the fields that shape the installer; publication settings
// and the icon asset change without invalidating prepared outputs.
func (s Spec) Preparation() Spec {
	s.Source, s.Destinations, s.Icon = nil, nil, ""
	return s
}

func (s Spec) Validate() error {
	if s.PackagePath != "" && !relativePath(s.PackagePath) {
		return errors.New("package_path must be a confined archive path")
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
	if s.Signature != nil {
		signer, err := signature.Parse(s.Signature.Signer)
		if err != nil {
			return fmt.Errorf("signature: %w", err)
		}
		if signer.Scheme != signature.AppleDeveloperID {
			return errors.New("signature.signer must name an Apple Developer ID team")
		}
	}
	if s.Icon != "" && !icon.ValidName(s.Icon) {
		return fmt.Errorf("icon must name an asset under %s/ without directories or extension", icon.Directory)
	}
	return nil
}

func relativePath(name string) bool {
	return fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00\r\n\t")
}
