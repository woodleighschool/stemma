package plugin

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// SubjectSelector matches observed evidence; an installation path is not a copy instruction.
type SubjectSelector struct {
	Kind          string `yaml:"kind,omitempty" json:"kind,omitempty" jsonschema_description:"Observed subject kind, such as app, container, file, package or msi. A package subject describes a receipt; the outer PKG is a container."`
	Path          string `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Exact path within the inspected artifact. A dot selects the artifact root."`
	InstalledPath string `yaml:"installed_path,omitempty" json:"installed_path,omitempty" jsonschema_description:"Observed absolute installation path, when the installer declares one."`
	BundleID      string `yaml:"bundle_id,omitempty" json:"bundle_id,omitempty" jsonschema_description:"Application bundle identifier, combined with any supplied path selectors."`
	PackageID     string `yaml:"package_id,omitempty" json:"package_id,omitempty" jsonschema_description:"Package identifier of a component receipt, as macOS records it."`
}

// SelectSubject requires one match. Every supplied criterion applies to the same subject.
func SelectSubject(facts Facts, selector SubjectSelector) (Subject, error) {
	var matches []Subject
	for _, subject := range facts.Subjects {
		if selector.Kind != "" && selector.Kind != subject.Kind ||
			selector.Path != "" && selector.Path != subject.Path ||
			selector.InstalledPath != "" && selector.InstalledPath != subject.InstalledPath ||
			selector.BundleID != "" && (subject.App == nil || selector.BundleID != subject.App.BundleID) ||
			selector.PackageID != "" && (subject.Package == nil || selector.PackageID != subject.Package.Identifier) {
			continue
		}
		matches = append(matches, subject)
	}
	switch len(matches) {
	case 0:
		return Subject{}, errors.New("selector matched no subject" + identifiers(facts, selector))
	case 1:
		return matches[0], nil
	}
	var candidates []string
	for _, subject := range matches {
		candidates = append(candidates, subject.ID+" ("+subject.Path+")")
	}
	return Subject{}, fmt.Errorf("selector matched %d subjects; require exactly one: %s", len(matches), strings.Join(candidates, ", "))
}

// identifiers lists the identifiers of the kind a selector asked for, so a
// selector that matched nothing shows what it could have named.
func identifiers(facts Facts, selector SubjectSelector) string {
	label := ""
	var found []string
	for _, subject := range facts.Subjects {
		switch {
		case selector.PackageID != "" && subject.Package != nil && subject.Package.Identifier != "":
			label, found = "package identifiers", append(found, subject.Package.Identifier)
		case selector.PackageID == "" && selector.BundleID != "" && subject.App != nil && subject.App.BundleID != "":
			label, found = "bundle identifiers", append(found, subject.App.BundleID)
		}
	}
	slices.Sort(found)
	found = slices.Compact(found)
	const shown = 8
	switch {
	case len(found) == 0:
		return ""
	case len(found) > shown:
		return fmt.Sprintf("; %s: %s and %d more", label, strings.Join(found[:shown], ", "), len(found)-shown)
	}
	return "; " + label + ": " + strings.Join(found, ", ")
}
