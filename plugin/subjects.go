package plugin

import (
	"fmt"
	"strings"
)

// SubjectSelector matches observed evidence; an installation path is not a copy instruction.
type SubjectSelector struct {
	Kind          string `yaml:"kind,omitempty" json:"kind,omitempty" jsonschema_description:"Observed subject kind: app, package or msi."`
	Path          string `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Exact path within the inspected artifact."`
	InstalledPath string `yaml:"installed_path,omitempty" json:"installed_path,omitempty" jsonschema_description:"Observed absolute installation path, when the installer declares one."`
	BundleID      string `yaml:"bundle_id,omitempty" json:"bundle_id,omitempty" jsonschema_description:"Application bundle identifier, combined with any supplied path selectors."`
}

// SelectSubject requires one match. Every supplied criterion applies to the same subject.
func SelectSubject(facts Facts, selector SubjectSelector) (Subject, error) {
	var matches []Subject
	for _, subject := range facts.Subjects {
		if selector.Kind != "" && selector.Kind != subject.Kind ||
			selector.Path != "" && selector.Path != subject.Path ||
			selector.InstalledPath != "" && selector.InstalledPath != subject.InstalledPath ||
			selector.BundleID != "" && (subject.App == nil || selector.BundleID != subject.App.BundleID) {
			continue
		}
		matches = append(matches, subject)
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	var candidates []string
	for _, subject := range matches {
		candidates = append(candidates, subject.ID+" ("+subject.Path+")")
	}
	return Subject{}, fmt.Errorf("selector matched %d subjects; require exactly one: %s", len(matches), strings.Join(candidates, ", "))
}
