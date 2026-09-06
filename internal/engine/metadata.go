package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/intune"
	"github.com/woodleighschool/stemma/plugin"
)

func needsInspection(native map[string]any, operation string) bool {
	if hasFactReference(native) {
		return true
	}
	if operation != "intune" || native["@odata.type"] == "#microsoft.graph.win32LobApp" {
		return false
	}
	_, included := native["includedApps"]
	_, primaryID := native["primaryBundleId"]
	_, primaryVersion := native["primaryBundleVersion"]
	_, minimum := native["minimumSupportedOperatingSystem"]
	return !included && (!primaryID || !primaryVersion) || !minimum
}

func validateReferences(recipe config.Recipe) error {
	validate := func(value any) error {
		_, err := mapFactReferences(value, func(reference string) (any, error) {
			_, _, err := parseFactReference(recipe, reference)
			return nil, err
		})
		return err
	}
	for destination, native := range recipe.Destinations {
		if err := validate(native); err != nil {
			return fmt.Errorf("destination %s: %w", destination, err)
		}
	}
	for _, step := range recipe.Steps {
		if err := validate(step.Config); err != nil {
			return fmt.Errorf("step %s: %w", step.Name, err)
		}
	}
	return nil
}

func resolveMetadata(recipe config.Recipe, native map[string]any, facts plugin.Facts, operation string) (map[string]any, map[string]string, error) {
	selected := map[string]plugin.Subject{}
	resolved, err := mapFactReferences(native, func(reference string) (any, error) {
		name, fields, err := parseFactReference(recipe, reference)
		if err != nil {
			return nil, err
		}
		if _, present := selected[name]; !present {
			selector := recipe.Subjects[name]
			var matches []plugin.Subject
			for _, subject := range facts.Subjects {
				if selector.Kind != "" && selector.Kind != subject.Kind || selector.Path != "" && selector.Path != subject.Path || selector.InstalledPath != "" && selector.InstalledPath != subject.InstalledPath || selector.BundleID != "" && (subject.App == nil || selector.BundleID != subject.App.BundleID) {
					continue
				}
				matches = append(matches, subject)
			}
			if len(matches) != 1 {
				return nil, fmt.Errorf("subject %s matched %d observed subjects; require exactly one", name, len(matches))
			}
			selected[name] = matches[0]
		}
		value, err := readFact(reflect.ValueOf(selected[name]), fields)
		if err != nil {
			return nil, fmt.Errorf("fact %s: %w", reference, err)
		}
		return value, nil
	})
	if err != nil {
		return nil, nil, err
	}
	explicit := resolved.(map[string]any)
	defaults := map[string]any{}
	switch operation {
	case "munki":
		defaults = munkiDefaults(facts, explicit)
	case "intune":
		defaults, err = intuneDefaults(facts, explicit)
		if err != nil {
			return nil, nil, err
		}
	}
	origins := make(map[string]string, len(defaults)+len(explicit))
	for key := range defaults {
		origins[key] = "derived"
	}
	for key, value := range native {
		origins[key] = "explicit"
		if hasFactReference(value) {
			origins[key] = "fact"
		}
	}
	return config.Merge(defaults, explicit), origins, nil
}

func mapFactReferences(value any, resolve func(string) (any, error)) (any, error) {
	switch value := value.(type) {
	case map[string]any:
		if reference, exists := value["$fact"]; exists {
			name, ok := reference.(string)
			if !ok || len(value) != 1 {
				return nil, errors.New("fact reference must be a sole-key {$fact: subject.field} object")
			}
			return resolve(name)
		}
		result := make(map[string]any, len(value))
		for key, child := range value {
			mapped, err := mapFactReferences(child, resolve)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			result[key] = mapped
		}
		return result, nil
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			mapped, err := mapFactReferences(child, resolve)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			result[i] = mapped
		}
		return result, nil
	default:
		return value, nil
	}
}

func parseFactReference(recipe config.Recipe, reference string) (string, []string, error) {
	name, field, ok := strings.Cut(reference, ".")
	if !ok || field == "" {
		return "", nil, fmt.Errorf("invalid fact reference %q", reference)
	}
	if _, exists := recipe.Subjects[name]; !exists {
		return "", nil, fmt.Errorf("unknown fact subject %q", name)
	}
	fields := strings.Split(field, ".")
	typ := reflect.TypeFor[plugin.Subject]()
	for _, field := range fields {
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() == reflect.Map && field != "" {
			typ = typ.Elem()
			continue
		}
		index := factField(typ, field)
		if index < 0 {
			return "", nil, fmt.Errorf("unknown fact field in %q", reference)
		}
		typ = typ.Field(index).Type
	}
	return name, fields, nil
}

func factField(typ reflect.Type, name string) int {
	if typ.Kind() != reflect.Struct || name == "" {
		return -1
	}
	for i := range typ.NumField() {
		field, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if field == name {
			return i
		}
	}
	return -1
}

func readFact(value reflect.Value, fields []string) (any, error) {
	for _, field := range fields {
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				return nil, errors.New("required observed value is missing")
			}
			value = value.Elem()
		}
		if value.Kind() == reflect.Map {
			value = value.MapIndex(reflect.ValueOf(field))
			if !value.IsValid() {
				return nil, errors.New("required observed value is missing")
			}
		} else {
			value = value.Field(factField(value.Type(), field))
		}
	}
	if value.Kind() == reflect.String && value.String() == "" || (value.Kind() == reflect.Pointer || value.Kind() == reflect.Map || value.Kind() == reflect.Slice) && value.IsNil() {
		return nil, errors.New("required observed value is missing")
	}
	data, err := json.Marshal(value.Interface())
	if err != nil {
		return nil, err
	}
	var result any
	err = json.Unmarshal(data, &result)
	return result, err
}

func munkiDefaults(facts plugin.Facts, explicit map[string]any) map[string]any {
	result := map[string]any{}
	versions := map[string]bool{}
	var receipts []any
	var apps []plugin.Subject
	for _, subject := range facts.Subjects {
		if pkg := subject.Package; pkg != nil {
			if pkg.Version != "" {
				versions[pkg.Version] = true
			}
			if pkg.HasPayload && pkg.Identifier != "" {
				receipts = append(receipts, map[string]any{"packageid": pkg.Identifier, "version": pkg.Version, "installed_size": pkg.InstalledSize})
			}
		}
		if subject.App != nil {
			apps = append(apps, subject)
			if subject.App.Version != "" {
				versions[subject.App.Version] = true
			}
		}
	}
	if len(versions) == 1 {
		for version := range versions {
			result["version"] = version
		}
	}
	if len(receipts) != 0 {
		result["receipts"] = receipts
	}
	_, script := explicit["installcheck_script"]
	_, installs := explicit["installs"]
	_, authoredReceipts := explicit["receipts"]
	if len(apps) == 1 && apps[0].InstalledPath != "" && !script && !installs && !authoredReceipts {
		app := apps[0]
		result["installs"] = []any{map[string]any{"type": "application", "path": app.InstalledPath, "CFBundleIdentifier": app.App.BundleID, "CFBundleShortVersionString": app.App.Version, "CFBundleVersion": app.App.Build}}
	}
	return result
}

func intuneDefaults(facts plugin.Facts, explicit map[string]any) (map[string]any, error) {
	result := map[string]any{}
	appType, _ := explicit["@odata.type"].(string)
	if _, present := explicit["@odata.type"]; !present {
		for _, subject := range facts.Subjects {
			if subject.Package != nil {
				appType = "#microsoft.graph.macOSPkgApp"
				break
			}
			if subject.MSI != nil {
				appType = "#microsoft.graph.win32LobApp"
			}
		}
		if appType != "" {
			result["@odata.type"] = appType
		}
	}
	if appType != "#microsoft.graph.macOSPkgApp" && appType != "#microsoft.graph.macOSDmgApp" {
		return result, nil
	}
	var apps []plugin.Subject
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			apps = append(apps, subject)
		}
	}
	if _, present := explicit["includedApps"]; !present {
		id, authoredID := explicit["primaryBundleId"]
		version, authoredVersion := explicit["primaryBundleVersion"]
		if !authoredID || !authoredVersion {
			candidates := apps
			if authoredID {
				candidates = nil
				for _, app := range apps {
					if id == app.App.BundleID {
						candidates = append(candidates, app)
					}
				}
			}
			if len(candidates) != 1 {
				return nil, fmt.Errorf("intune application detection found %d matching observed apps; author includedApps or a primary bundle ID/version pair explicitly", len(candidates))
			}
			if !authoredID {
				id = candidates[0].App.BundleID
			}
			if !authoredVersion {
				version = candidates[0].App.Version
			}
		}
		if id == "" || version == "" {
			return nil, errors.New("intune application detection requires an observed bundle ID and short version; author includedApps explicitly")
		}
		result["includedApps"] = []any{map[string]any{"bundleId": id, "bundleVersion": version}}
	}
	included, exists := explicit["includedApps"]
	if !exists {
		included = result["includedApps"]
	}
	if list, ok := included.([]any); ok && len(list) != 0 {
		if first, ok := list[0].(map[string]any); ok {
			for native, field := range map[string]string{"primaryBundleId": "bundleId", "primaryBundleVersion": "bundleVersion"} {
				if value, exists := explicit[native]; exists {
					if !reflect.DeepEqual(value, first[field]) {
						return nil, fmt.Errorf("%s must agree with the first includedApps entry", native)
					}
				} else if value, present := first[field]; present {
					result[native] = value
				}
			}
		}
	}
	if _, present := explicit["minimumSupportedOperatingSystem"]; !present {
		minimum := ""
		if list, ok := included.([]any); ok {
			for _, item := range list {
				entry, _ := item.(map[string]any)
				for _, subject := range apps {
					if entry["bundleId"] == subject.App.BundleID && subject.App.MinimumOS != "" {
						candidate := subject.App.MinimumOS
						if _, err := osVersionParts(candidate); err != nil {
							return nil, err
						}
						if compareOS(candidate, minimum) > 0 {
							minimum = candidate
						}
					}
				}
			}
		}
		if minimum != "" {
			value, err := intuneMinimumOS(minimum)
			if err != nil {
				return nil, err
			}
			result["minimumSupportedOperatingSystem"] = value
		}
	}
	return result, nil
}

func intuneMinimumOS(version string) (map[string]any, error) {
	parts, err := osVersionParts(version)
	if err != nil {
		return nil, err
	}
	field := "v" + strings.Join(parts, "_")
	for _, variant := range intune.MetadataSchema().OneOf {
		minimum, ok := variant.Properties.Get("minimumSupportedOperatingSystem")
		if ok && minimum.Properties != nil {
			if _, supported := minimum.Properties.Get(field); supported {
				return map[string]any{field: true}, nil
			}
		}
	}
	return nil, fmt.Errorf("minimum macOS %q has no exact supported Intune setting; author minimumSupportedOperatingSystem explicitly", version)
}

func osVersionParts(version string) ([]string, error) {
	parts := strings.Split(version, ".")
	if len(parts) == 1 {
		parts = append(parts, "0")
	}
	for _, part := range parts {
		if number, err := strconv.Atoi(part); err != nil || number < 0 || strconv.Itoa(number) != part {
			return nil, fmt.Errorf("minimum macOS %q requires explicit minimumSupportedOperatingSystem", version)
		}
	}
	if len(parts) > 2 && !slices.ContainsFunc(parts[2:], func(part string) bool { return part != "0" }) {
		parts = parts[:2]
	}
	return parts, nil
}

func compareOS(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(left), len(right)); i++ {
		var x, y int
		if i < len(left) {
			x, _ = strconv.Atoi(left[i])
		}
		if i < len(right) {
			y, _ = strconv.Atoi(right[i])
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}
