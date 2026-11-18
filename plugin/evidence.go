package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
)

// Evidence namespaces the host writes into prepared artifacts. Resolvers
// cannot write them.
const (
	// EvidenceSource holds the version a source states for its input.
	EvidenceSource = "source"
	// EvidenceSigning holds what preparation verified about signing.
	EvidenceSigning = "signing"
	// EvidenceMacOS holds what preparation selected in a macOS installer.
	EvidenceMacOS = "macos"
	// EvidenceWindows holds the MSI a Windows artifact runs.
	EvidenceWindows = "windows"
)

// MacOSEvidence is the macos namespace: the application an installer installs
// and the Info.plist key holding its comparable version.
type MacOSEvidence struct {
	Application *Subject `json:"application,omitempty"`
	VersionKey  string   `json:"version_key,omitempty"`
}

// WindowsEvidence is the windows namespace: the MSI setup file an artifact
// runs, when its setup file is one.
type WindowsEvidence struct {
	Installer *Subject `json:"installer,omitempty"`
}

var evidenceName = regexp.MustCompile(`^[a-z][a-z0-9]*([_-][a-z0-9]+)*$`)

// ValidEvidenceName reports whether name can key evidence. A name is one
// namespace; whatever the namespace holds nests below it, so paths such as
// evidence.homebrew.cask.macos are unambiguous.
func ValidEvidenceName(name string) bool { return evidenceName.MatchString(name) }

// HostEvidence reports whether the host writes the namespace itself.
func HostEvidence(name string) bool {
	return slices.Contains([]string{EvidenceSource, EvidenceSigning, EvidenceMacOS, EvidenceWindows}, name)
}

// ReadMacOS returns the macos namespace of an artifact's evidence. Absent
// evidence is empty; present evidence names an application subject and, if
// any, a version key Munki supports.
func ReadMacOS(evidence map[string]json.RawMessage) (MacOSEvidence, error) {
	var macos MacOSEvidence
	data, ok := evidence[EvidenceMacOS]
	if !ok {
		return macos, nil
	}
	if err := json.Unmarshal(data, &macos); err != nil || macos.Application == nil || macos.Application.App == nil {
		return MacOSEvidence{}, errors.New("macos evidence requires an application subject")
	}
	if key := macos.VersionKey; key != "" && key != "CFBundleVersion" && key != "CFBundleShortVersionString" {
		return MacOSEvidence{}, fmt.Errorf("unsupported macos version_key %q", key)
	}
	return macos, nil
}

// ReadWindows returns the windows namespace of an artifact's evidence. Absent
// evidence is empty; present evidence names an MSI subject.
func ReadWindows(evidence map[string]json.RawMessage) (WindowsEvidence, error) {
	var windows WindowsEvidence
	data, ok := evidence[EvidenceWindows]
	if !ok {
		return windows, nil
	}
	if err := json.Unmarshal(data, &windows); err != nil || windows.Installer == nil || windows.Installer.MSI == nil {
		return WindowsEvidence{}, errors.New("windows evidence requires an MSI installer subject")
	}
	return windows, nil
}
