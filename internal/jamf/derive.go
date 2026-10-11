package jamf

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// deriveMetadata supplies locally known managed values before remote names
// resolve. Creation-only defaults and preserved remote values are not managed.
func deriveMetadata(request plugin.ReconcileRequest[Config]) (metadata, plugin.ReconcileResponse, error) {
	var response plugin.ReconcileResponse
	declared, err := decodeObject(request.Metadata)
	if err != nil {
		return metadata{}, response, fmt.Errorf("jamf metadata: %w", err)
	}
	m, err := decodeMetadata(declared)
	if err != nil {
		return m, response, err
	}
	response.Origins = map[string]string{}
	if request.Artifact.Filename != "" {
		m.fields["fileName"] = raw(request.Artifact.Filename)
		declared["filename"] = m.fields["fileName"]
		response.Origins["filename"] = "artifact.filename"
	}
	for i, policy := range m.policies {
		prefix := fmt.Sprintf("policies.%d.", i)
		if !policy.Category.set && m.category.set {
			policy.Category = m.category
			response.Origins[prefix+"category"] = "category"
		}
		if s := policy.SelfService; s != nil && s.enabled {
			if s.DisplayName == nil {
				name, origin := applicationName(request.Artifact)
				if name == "" {
					name, origin = request.Identity.Resource.Name, "software.name"
				}
				if name != "" {
					s.DisplayName = &name
					response.Origins[prefix+"self_service.display_name"] = origin
				}
			}
			if !s.Category.set && m.category.set {
				s.Category = m.category
				response.Origins[prefix+"self_service.category"] = "category"
			}
		}
	}
	if _, exists := declared["policies"]; exists {
		declared["policies"] = raw(m.policies)
	}
	if patch := m.patch; patch != nil {
		if patch.Version == nil && request.Artifact.Version != "" {
			patch.Version = new(request.Artifact.Version)
			response.Origins["patch.version"] = "artifact.version"
		}
		if (patch.Version == nil || strings.TrimSpace(*patch.Version) == "") && (request.Prepared || request.Artifact.SHA256 != "" || request.Method != "validate") {
			return m, response, errors.New("jamf patch has no managed version; set patch.version")
		}
		if p := patch.Policy; p != nil {
			if p.Name == nil && request.Identity.Resource.Name != "" {
				p.Name = new(request.Identity.Resource.Name)
				response.Origins["patch.policy.name"] = "software.name"
			}
			if p.Distribution != nil && *p.Distribution == "self_service" && !p.ReminderDays.set {
				p.ReminderDays = days{set: true, count: new(1)}
				response.Origins["patch.policy.reminder_days"] = "default"
			}
		}
		declared["patch"] = raw(patch)
	}
	response.Metadata = raw(declared)
	return m, response, nil
}

// applicationName is the selected application's name without its extension.
func applicationName(artifact plugin.Artifact) (string, string) {
	var selected plugin.Subject
	if json.Unmarshal(artifact.Evidence["macos.application"], &selected) != nil || selected.App == nil {
		return "", ""
	}
	location, origin := selected.InstalledPath, "app.installed_path"
	if location == "" {
		location, origin = selected.Path, "app.path"
	}
	return strings.TrimSuffix(path.Base(location), ".app"), origin
}
