package intune

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"reflect"
	"slices"
	"strings"
)

const (
	win32Type = "#microsoft.graph.win32LobApp"
	dmgType   = "#microsoft.graph.macOSDmgApp"
	pkgType   = "#microsoft.graph.macOSPkgApp"
)

type object = map[string]any

func decodeObject(data []byte) (object, error) {
	var result object
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("expected JSON object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected one JSON object")
	}
	return result, nil
}

func validateMetadata(data []byte) (object, error) {
	m, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	appType := text(m["@odata.type"])
	if !enum(appType, win32Type, dmgType, pkgType) {
		return nil, errors.New("supported Intune types are win32LobApp, macOSDmgApp and macOSPkgApp; macOSLobApp is unsupported")
	}
	stringsAllowed := []string{"displayName", "description", "publisher", "privacyInformationUrl", "informationUrl", "owner", "developer", "notes"}
	if appType == win32Type {
		stringsAllowed = append(stringsAllowed, "installCommandLine", "uninstallCommandLine", "minimumSupportedWindowsRelease")
	} else {
		stringsAllowed = append(stringsAllowed, "primaryBundleId", "primaryBundleVersion")
	}
	numbers := []string{"minimumFreeDiskSpaceInMB", "minimumMemoryInMB", "minimumNumberOfProcessors", "minimumCpuSpeedInMHz"}
	for key, value := range m {
		switch {
		case key == "@odata.type":
		case key == "app_id":
			if text(value) == "" {
				return nil, errors.New("app_id must be a nonempty adopted app ID")
			}
		case slices.Contains(stringsAllowed, key):
			text, ok := value.(string)
			if !ok || len(text) > 10000 {
				return nil, fmt.Errorf("%s must be a string of at most 10000 bytes", key)
			}
			if key == "notes" && strings.Contains(text, "[stemma:v1 ") {
				return nil, errors.New("notes contains a reserved Stemma marker")
			}
		case appType == win32Type && slices.Contains(numbers, key):
			n, ok := value.(float64)
			if !ok || n < 0 || n > math.MaxInt32 || n != math.Trunc(n) {
				return nil, fmt.Errorf("%s must be a nonnegative Int32", key)
			}
		case key == "isFeatured" || (appType != win32Type && key == "ignoreVersionDetection"):
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be boolean", key)
			}
		case appType == win32Type && key == "allowedArchitectures":
			if value != nil && !enum(value, "x86", "x64", "arm64") {
				return nil, errors.New("allowedArchitectures must be x86, x64, arm64 or null")
			}
		case appType == win32Type && key == "installExperience":
			v, ok := value.(object)
			if !ok {
				return nil, errors.New("installExperience must be an object")
			}
			if err := fields(v, "@odata.type", "runAsAccount", "deviceRestartBehavior"); err != nil {
				return nil, err
			}
			if v["@odata.type"] != nil && v["@odata.type"] != "#microsoft.graph.win32LobAppInstallExperience" {
				return nil, errors.New("unsupported installExperience type")
			}
			if v["runAsAccount"] != nil && !enum(v["runAsAccount"], "system", "user") {
				return nil, errors.New("unsupported runAsAccount")
			}
			if v["deviceRestartBehavior"] != nil && !enum(v["deviceRestartBehavior"], "basedOnReturnCode", "allow", "suppress", "force") {
				return nil, errors.New("unsupported deviceRestartBehavior")
			}
			for _, value := range v {
				if value == nil {
					return nil, errors.New("installExperience does not support null fields")
				}
			}
		case appType == win32Type && key == "rules":
			if err := validateRules(value); err != nil {
				return nil, err
			}
		case appType == win32Type && key == "returnCodes":
			list, ok := value.([]any)
			if !ok {
				return nil, errors.New("returnCodes must be an array")
			}
			for _, item := range list {
				v, ok := item.(object)
				if !ok {
					return nil, errors.New("return code must be an object")
				}
				if err := fields(v, "returnCode", "type", "@odata.type"); err != nil {
					return nil, err
				}
				if t, exists := v["@odata.type"]; exists && t != "#microsoft.graph.win32LobAppReturnCode" {
					return nil, errors.New("unsupported return code type")
				}
				n, ok := v["returnCode"].(float64)
				if !ok || n < math.MinInt32 || n > math.MaxInt32 || n != math.Trunc(n) || !enum(v["type"], "success", "softReboot", "hardReboot", "retry", "failed") {
					return nil, errors.New("invalid return code")
				}
			}
		case appType != win32Type && key == "includedApps":
			if err := validateIncludedApps(value); err != nil {
				return nil, err
			}
		case appType != win32Type && key == "minimumSupportedOperatingSystem":
			if err := validateMinimumOS(value, appType); err != nil {
				return nil, err
			}
		case key == "assignments":
			if err := validateAssignments(value); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported writable %s field %q", strings.TrimPrefix(appType, "#microsoft.graph."), key)
		}
	}
	return m, nil
}

func minimumOSFields(appType string) []string {
	fields := []string{"v10_7", "v10_8", "v10_9", "v10_10", "v10_11", "v10_12", "v10_13", "v10_14", "v10_15", "v11_0", "v12_0", "v13_0"}
	if appType == pkgType {
		fields = append(fields, "v14_0", "v15_0", "v26_0")
	}
	return fields
}

func validateMinimumOS(value any, appType string) error {
	operatingSystem, ok := value.(object)
	if !ok {
		return errors.New("minimumSupportedOperatingSystem must be an object")
	}
	allowed := append(minimumOSFields(appType), "@odata.type")
	if err := fields(operatingSystem, allowed...); err != nil {
		return err
	}
	if t, exists := operatingSystem["@odata.type"]; exists && t != "#microsoft.graph.macOSMinimumOperatingSystem" {
		return errors.New("unsupported minimum operating system type")
	}
	selected := 0
	for key, value := range operatingSystem {
		if key == "@odata.type" {
			continue
		}
		enabled, ok := value.(bool)
		if !ok {
			return fmt.Errorf("minimum OS %s must be boolean", key)
		}
		if enabled {
			selected++
		}
	}
	if selected != 1 {
		return errors.New("minimumSupportedOperatingSystem must select exactly one version with true")
	}
	return nil
}

func validateIncludedApps(value any) error {
	apps, ok := value.([]any)
	if !ok || len(apps) == 0 || len(apps) > 500 {
		return errors.New("includedApps must contain between 1 and 500 bundle ID/version pairs")
	}
	seen := map[string]bool{}
	for _, item := range apps {
		app, ok := item.(object)
		if !ok {
			return errors.New("included app must be an object")
		}
		if err := fields(app, "@odata.type", "bundleId", "bundleVersion"); err != nil {
			return err
		}
		if t, exists := app["@odata.type"]; exists && t != "#microsoft.graph.macOSIncludedApp" {
			return errors.New("unsupported included app type")
		}
		id, version := text(app["bundleId"]), text(app["bundleVersion"])
		if id == "" || version == "" || len(id) > 1000 || len(version) > 1000 {
			return errors.New("included app requires nonempty bundleId and bundleVersion strings")
		}
		if seen[id] {
			return errors.New("includedApps contains duplicate bundle IDs")
		}
		seen[id] = true
	}
	return nil
}

// Minimum OS is one selection even though Graph encodes it as boolean flags.
func selectedOS(value any) string {
	m, _ := value.(object)
	selected := ""
	for key, value := range m {
		if value == true {
			if selected != "" {
				return ""
			}
			selected = key
		}
	}
	return selected
}

func validateRules(value any) error {
	list, ok := value.([]any)
	if !ok || len(list) > 100 {
		return errors.New("rules must be an array of at most 100 rules")
	}
	for _, item := range list {
		rule, ok := item.(object)
		if !ok {
			return errors.New("rule must be an object")
		}
		if rule["ruleType"] != "detection" {
			return errors.New("only detection rules are supported")
		}
		switch rule["@odata.type"] {
		case "#microsoft.graph.win32LobAppProductCodeRule":
			if err := fields(rule, "@odata.type", "ruleType", "productCode", "productVersionOperator", "productVersion"); err != nil {
				return err
			}
			if text(rule["productCode"]) == "" {
				return errors.New("product-code detection requires productCode")
			}
			if !enum(rule["productVersionOperator"], "notConfigured", "equal", "notEqual", "greaterThan", "greaterThanOrEqual", "lessThan", "lessThanOrEqual") {
				return errors.New("invalid productVersionOperator")
			}
		case "#microsoft.graph.win32LobAppFileSystemRule":
			if err := fields(rule, "@odata.type", "ruleType", "path", "fileOrFolderName", "check32BitOn64System", "operationType", "operator", "comparisonValue"); err != nil {
				return err
			}
			if text(rule["path"]) == "" || text(rule["fileOrFolderName"]) == "" || !enum(rule["operationType"], "exists", "version", "sizeInMB", "modifiedDate", "createdDate") {
				return errors.New("invalid file-system detection")
			}
			if !enum(rule["operator"], "notConfigured", "equal", "notEqual", "greaterThan", "greaterThanOrEqual", "lessThan", "lessThanOrEqual") {
				return errors.New("invalid file-system operator")
			}
		default:
			return errors.New("only product-code and file-system detection rules are supported")
		}
		for key, value := range rule {
			if key == "check32BitOn64System" {
				if _, ok := value.(bool); !ok {
					return errors.New("check32BitOn64System must be boolean")
				}
			} else if _, ok := value.(string); !ok {
				return fmt.Errorf("rule %s must be a string", key)
			}
		}
	}
	return nil
}

func validateAssignments(value any) error {
	list, ok := value.([]any)
	if !ok || len(list) > 1000 {
		return errors.New("assignments must be an array of at most 1000 items; [] clears assignments")
	}
	seen := map[string]bool{}
	for _, item := range list {
		assignment, ok := item.(object)
		if !ok {
			return errors.New("assignment must be an object")
		}
		if err := fields(assignment, "@odata.type", "intent", "target"); err != nil {
			return err
		}
		if assignment["@odata.type"] != nil && assignment["@odata.type"] != "#microsoft.graph.mobileAppAssignment" {
			return errors.New("unsupported assignment type")
		}
		if !enum(assignment["intent"], "available", "required", "uninstall", "availableWithoutEnrollment") {
			return errors.New("invalid assignment intent")
		}
		target, ok := assignment["target"].(object)
		if !ok {
			return errors.New("assignment target must be an object")
		}
		if err := fields(target, "@odata.type", "groupId"); err != nil {
			return err
		}
		switch target["@odata.type"] {
		case "#microsoft.graph.groupAssignmentTarget", "#microsoft.graph.exclusionGroupAssignmentTarget":
			if text(target["groupId"]) == "" {
				return errors.New("group assignment requires groupId")
			}
		case "#microsoft.graph.allDevicesAssignmentTarget", "#microsoft.graph.allLicensedUsersAssignmentTarget":
			if _, exists := target["groupId"]; exists {
				return errors.New("all-users/devices target cannot have groupId")
			}
		default:
			return errors.New("unsupported assignment target type")
		}
		key := assignmentKey(assignment)
		if seen[key] {
			return errors.New("duplicate assignment target and intent")
		}
		seen[key] = true
	}
	return nil
}

func fields(value object, allowed ...string) error {
	for key := range value {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unsupported field %q", key)
		}
	}
	return nil
}

func enum(value any, allowed ...string) bool {
	text, ok := value.(string)
	return ok && slices.Contains(allowed, text)
}
func text(value any) string         { result, _ := value.(string); return result }
func raw(value any) json.RawMessage { data, _ := json.Marshal(value); return data }

func mergeOwned(current, desired object) object {
	result := object{}
	maps.Copy(result, current)
	for key, value := range desired {
		if child, ok := value.(object); ok {
			previous, _ := current[key].(object)
			result[key] = mergeOwned(previous, child)
		} else {
			result[key] = value
		}
	}
	return result
}

func ownedEqual(current any, desired any) bool {
	if object, ok := desired.(object); ok {
		previous, ok := current.(map[string]any)
		if !ok {
			return len(object) == 0
		}
		for key, value := range object {
			if !ownedEqual(previous[key], value) {
				return false
			}
		}
		return true
	}
	if list, ok := desired.([]any); ok {
		previous, ok := current.([]any)
		if !ok || len(previous) != len(list) {
			return false
		}
		used := make([]bool, len(previous))
		for _, wanted := range list {
			found := false
			for i, observed := range previous {
				if !used[i] && ownedEqual(observed, wanted) {
					used[i] = true
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(current, desired)
}

func assignmentKey(value object) string {
	target, _ := value["target"].(object)
	return string(raw([]any{value["intent"], target["@odata.type"], target["groupId"]}))
}

func mergeItems(field string, current, desired []any) []any {
	key := func(item object) string {
		if field == "returnCodes" {
			return string(raw(item["returnCode"]))
		}
		return string(raw([]any{item["@odata.type"], item["ruleType"], item["productCode"], item["path"], item["fileOrFolderName"]}))
	}
	previous := map[string]object{}
	for _, item := range current {
		if value, ok := item.(object); ok {
			previous[key(value)] = value
		}
	}
	result := make([]any, 0, len(desired))
	for _, item := range desired {
		value := item.(object)
		result = append(result, mergeOwned(previous[key(value)], value))
	}
	return result
}

func reconcileAssignments(current, desired []any) ([]any, bool) {
	byKey := map[string]object{}
	for _, item := range current {
		if v, ok := item.(object); ok {
			byKey[assignmentKey(v)] = v
		}
	}
	result := make([]any, 0, len(desired))
	changed := len(current) != len(desired)
	for _, item := range desired {
		v := item.(object)
		previous, ok := byKey[assignmentKey(v)]
		if !ok || !ownedEqual(previous, v) {
			changed = true
		}
		merged := mergeOwned(previous, v)
		delete(merged, "id")
		delete(merged, "@odata.context")
		result = append(result, merged)
	}
	return result, changed
}
