package intune

import (
	"encoding/base64"
	"reflect"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// outcome is a publication in parts, so a failed apply reports the parts it
// completed. Fields follow the order apply writes them.
type outcome struct {
	version, previous                                string
	installer, icon                                  *plugin.Upload
	metadata, assignments, categories, relationships []plugin.FieldChange
}

func (o outcome) response() plugin.ReconcileResponse {
	response := plugin.ReconcileResponse{Version: o.version, Previous: o.previous, Fields: slices.Concat(o.metadata, o.assignments, o.categories, o.relationships)}
	for _, upload := range []*plugin.Upload{o.installer, o.icon} {
		if upload != nil {
			response.Uploads = append(response.Uploads, *upload)
		}
	}
	return response
}

// versionProperty is the Graph property holding the version clients get: a
// Win32 app's display version or a macOS app's primary bundle version.
func versionProperty(appType string) string {
	switch appType {
	case win32Type:
		return "displayVersion"
	case lobType:
		return "buildNumber"
	default:
		return "primaryBundleVersion"
	}
}

// metadataFields reports the native patch in declaration terms. A nonempty
// version is shown in the headline; a clear remains an explicit field.
func metadataFields(current, desired object, published publication) []plugin.FieldChange {
	version := versionProperty(text(desired["@odata.type"]))
	var fields []plugin.FieldChange
	report := func(property string, before any, present bool, after any) {
		name := reportName(property)
		was, now := reportValue(property, before), reportValue(property, after)
		field := plugin.FieldChange{Field: name, After: raw(now)}
		if present {
			field.Before = raw(was)
		}
		fields = append(fields, field)
	}
	for key, after := range metadataPatch(current, desired, published) {
		if key == version && text(after) != "" {
			continue
		}
		before, present := current[key]
		switch key {
		case "notes":
			notes, owned := desired["notes"]
			if previous := noteText(current, nil); owned && (!present || previous != notes) {
				report(key, previous, present, notes)
			}
		case "installExperience", "msiInformation":
			previous, _ := before.(object)
			merged, _ := after.(object)
			for leaf, value := range merged {
				if old, had := previous[leaf]; had && !reflect.DeepEqual(old, value) || !had && value != nil {
					report(key+"."+leaf, old, had, value)
				}
			}
		default:
			report(key, before, present, after)
		}
	}
	slices.SortFunc(fields, func(a, b plugin.FieldChange) int { return strings.Compare(a.Field, b.Field) })
	return fields
}

// reportName names a Graph property path the way declarations do, so reports
// read in the catalog's terms.
func reportName(property string) string {
	head, rest, nested := strings.Cut(property, ".")
	name := head
	if derived, ok := derivedNames[property]; ok {
		return derived
	}
	for _, table := range []map[string]field{commonFields, macFields, pkgFields, lobFields, win32Fields} {
		for declared, spec := range table {
			if spec.graph == head {
				name = declared
			}
		}
	}
	if !nested {
		return name
	}
	for _, table := range []map[string]string{msiFields, experienceFields} {
		for declared, graph := range table {
			if graph == rest {
				return name + "." + declared
			}
		}
	}
	return name + "." + rest
}

// reportValue names a Graph property's value the way declarations do. A value
// outside the declared vocabulary passes through unchanged.
func reportValue(property string, value any) any {
	switch property {
	case "allowedArchitectures":
		if flags, ok := value.(string); ok {
			return strings.Split(flags, ",")
		}
	case "minimumSupportedOperatingSystem":
		return minimumOSChangeValue(value)
	case "preInstallScript", "postInstallScript":
		return scriptText(value)
	case "includedApps":
		return items(value, func(app object) any { return object{"id": app["bundleId"], "version": app["bundleVersion"]} })
	case "childApps":
		return items(value, func(app object) any {
			return object{"id": app["bundleId"], "version": app["buildNumber"], "build": app["versionNumber"]}
		})
	case "rules":
		return items(value, declaredRule)
	case "returnCodes":
		return items(value, func(code object) any {
			return object{"code": code["returnCode"], "type": declaredChoice(code["type"], returnTypes)}
		})
	case "installExperience.deviceRestartBehavior":
		return declaredChoice(value, restarts)
	case "msiInformation.packageType":
		return declaredChoice(value, packageTypes)
	}
	return value
}

// items names each object of a Graph collection.
func items(value any, declared func(object) any) any {
	list, ok := value.([]any)
	if !ok {
		return value
	}
	result := make([]any, len(list))
	for i, item := range list {
		result[i] = item
		if native, ok := item.(object); ok {
			result[i] = declared(native)
		}
	}
	return result
}

// declaredChoice names a Graph enum value the way declarations do.
func declaredChoice(value any, values map[string]string) any {
	for declared, native := range values {
		if native == value {
			return declared
		}
	}
	return value
}

func scriptText(value any) any {
	if value == nil {
		return nil
	}
	script, _ := value.(object)
	content, _ := base64.StdEncoding.DecodeString(text(script["scriptContent"]))
	return string(content)
}

func minimumOSChangeValue(value any) any {
	if selected := selectedOS(value); selected != "" {
		return strings.ReplaceAll(strings.TrimPrefix(selected, "v"), "_", ".")
	}
	return value
}
