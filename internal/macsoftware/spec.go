// Package macsoftware prepares macOS installers and their selected application evidence.
package macsoftware

import (
	"encoding/hex"
	"errors"
	"io/fs"
	"path"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

const Version = "stemma.macsoftware/1"

type Spec struct {
	Source       *plugin.Input             `json:"source,omitempty" yaml:"source,omitempty"`
	Application  *Application              `json:"application,omitempty" yaml:"application,omitempty"`
	PackagePath  string                    `json:"package_path,omitempty" yaml:"package_path,omitempty"`
	Verification Verification              `json:"verification,omitzero" yaml:"verification,omitempty"`
	Destinations map[string]map[string]any `json:"destinations,omitempty" yaml:"destinations,omitempty"`
}

type Application struct {
	Path          string `json:"path,omitempty" yaml:"path,omitempty"`
	InstalledPath string `json:"installed_path,omitempty" yaml:"installed_path,omitempty"`
	BundleID      string `json:"bundle_id,omitempty" yaml:"bundle_id,omitempty"`
	VersionKey    string `json:"version_key,omitempty" yaml:"version_key,omitempty" jsonschema:"enum=CFBundleShortVersionString,enum=CFBundleVersion"`
}

type Verification struct {
	Subject           string `json:"subject,omitempty" yaml:"subject,omitempty" jsonschema:"enum=source,enum=application,enum=installer"`
	Integrity         bool   `json:"integrity,omitempty" yaml:"integrity,omitempty"`
	Signature         bool   `json:"signature,omitempty" yaml:"signature,omitempty"`
	Resources         bool   `json:"resources,omitempty" yaml:"resources,omitempty"`
	Identity          bool   `json:"identity,omitempty" yaml:"identity,omitempty"`
	CertificateSHA256 string `json:"certificate_sha256,omitempty" yaml:"certificate_sha256,omitempty"`
	Platform          bool   `json:"platform,omitempty" yaml:"platform,omitempty"`
}

func (s Spec) Preparation() Spec {
	s.Source, s.Destinations = nil, nil
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
	v := s.Verification
	if v.Subject != "" && v.Subject != "source" && v.Subject != "application" && v.Subject != "installer" {
		return errors.New("verification.subject must be source, application or installer")
	}
	if v.CertificateSHA256 != "" {
		digest, err := hex.DecodeString(v.CertificateSHA256)
		if err != nil || len(digest) != 32 {
			return errors.New("verification.certificate_sha256 must be a SHA-256 digest")
		}
	}
	return nil
}

func relativePath(name string) bool {
	return fs.ValidPath(name) && !strings.ContainsAny(name, "\\\x00\r\n\t")
}
