package intune

import (
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"math"
	"path"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// A field translates one declared metadata field into its Graph property.
type field struct {
	graph   string
	convert func(any) (any, error)
}

var commonFields = map[string]field{
	"display_name":    {"displayName", text10k},
	"description":     {"description", text10k},
	"publisher":       {"publisher", text10k},
	"developer":       {"developer", text10k},
	"owner":           {"owner", text10k},
	"notes":           {"notes", notesText},
	"information_url": {"informationUrl", text10k},
	"privacy_url":     {"privacyInformationUrl", text10k},
	"featured":        {"isFeatured", boolean},
	"assignments":     {"assignments", assignments},
}

var macFields = map[string]field{
	"included_apps":            {"includedApps", includedApps},
	"ignore_version_detection": {"ignoreVersionDetection", boolean},
}

// lobFields apply only to a Mac line-of-business app.
var lobFields = map[string]field{
	"install_as_managed": {"installAsManaged", boolean},
}

var win32Fields = map[string]field{
	"install_command":         {"installCommandLine", text10k},
	"uninstall_command":       {"uninstallCommandLine", text10k},
	"install_experience":      {"installExperience", installExperience},
	"minimum_windows_release": {"minimumSupportedWindowsRelease", text10k},
	"architectures":           {"allowedArchitectures", architectures},
	"minimum_disk_space_mb":   {"minimumFreeDiskSpaceInMB", int32Value},
	"minimum_memory_mb":       {"minimumMemoryInMB", int32Value},
	"minimum_processors":      {"minimumNumberOfProcessors", int32Value},
	"minimum_cpu_speed_mhz":   {"minimumCpuSpeedInMHz", int32Value},
	"detection":               {"rules", detection},
	"return_codes":            {"returnCodes", returnCodes},
	"msi":                     {"msiInformation", msiInformation},
}

// Graph properties that only derivation supplies still need names in reports.
var derivedNames = map[string]string{
	"primaryBundleId":                 "primary_bundle_id",
	"primaryBundleVersion":            "primary_bundle_version",
	"bundleId":                        "primary_bundle_id",
	"buildNumber":                     "primary_bundle_version",
	"versionNumber":                   "primary_bundle_build",
	"childApps":                       "included_apps",
	"minimumSupportedOperatingSystem": "minimum_os",
	"setupFilePath":                   "setup_file",
}

var msiFields = map[string]string{
	"product_code": "productCode", "product_version": "productVersion", "upgrade_code": "upgradeCode",
	"product_name": "productName", "publisher": "publisher", "requires_reboot": "requiresReboot", "package_type": "packageType",
}

var experienceFields = map[string]string{"run_as": "runAsAccount", "restart": "deviceRestartBehavior"}

var appTypes = map[string]string{"win32": win32Type, "pkg": pkgType, "dmg": dmgType, "lob": lobType}

// compile translates declared metadata into the Graph app object. Declarations
// name concepts in snake_case, while Graph property names, OData types and enum
// casing stay inside the destination. Omitted fields stay omitted and supported
// nulls clear, so the object keeps the declaration's presence. app_id,
// retention, dependencies and supersedes belong to the destination and pass
// through unchanged.
func compile(req plugin.ReconcileRequest[Config]) (object, error) {
	declared, err := decodeObject(req.Metadata)
	if err != nil {
		return nil, err
	}
	appType, err := resolveType(req, declared)
	if err != nil {
		return nil, err
	}
	platform := macFields
	if appType == win32Type {
		platform = win32Fields
	}
	m := object{"@odata.type": appType}
	for key, value := range declared {
		switch key {
		case "type":
			continue
		case "retention":
			m[key] = value
			continue
		case "app_id":
			if text(value) == "" {
				return nil, errors.New("app_id must be a nonempty adopted app ID")
			}
			m[key] = value
			continue
		case "dependencies", "supersedes":
			if appType != win32Type {
				return nil, fmt.Errorf("%s requires a Win32 app", key)
			}
			m[key] = value
			continue
		}
		spec, ok := commonFields[key]
		if !ok {
			spec, ok = platform[key]
		}
		if !ok && appType == lobType {
			spec, ok = lobFields[key]
		}
		if !ok {
			return nil, fmt.Errorf("unsupported Intune %s field %q", strings.TrimPrefix(appType, "#microsoft.graph."), key)
		}
		converted, err := spec.convert(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		m[spec.graph] = converted
	}
	if list, declared := m["assignments"].([]any); declared && appType != win32Type {
		for _, item := range list {
			if _, set := item.(object)["settings"]; set {
				return nil, errors.New("assignment notifications require a Win32 app")
			}
		}
	}
	// A line-of-business app lists its included apps as child apps, which
	// record the version twice.
	if apps, declared := m["includedApps"].([]any); declared && appType == lobType {
		children := make([]any, len(apps))
		for i, item := range apps {
			app := item.(object)
			children[i] = object{"bundleId": app["bundleId"], "buildNumber": app["bundleVersion"], "versionNumber": app["bundleVersion"]}
		}
		delete(m, "includedApps")
		m["childApps"] = children
	}
	return m, nil
}

// resolveType selects the Graph app type. The software kind fixes the platform,
// so only a Mac line-of-business app needs a declared type; otherwise a Mac PKG
// or DMG follows the artifact's format and Windows software is a Win32 app.
func resolveType(req plugin.ReconcileRequest[Config], m object) (string, error) {
	value, present := m["type"]
	name, declared := value.(string)
	if present && !declared {
		return "", errors.New("type must be a string")
	}
	allowed := map[string][]string{"WindowsSoftware": {"win32"}, "MacSoftware": {"pkg", "dmg", "lob"}}[req.Identity.Resource.Kind]
	if allowed == nil {
		allowed = []string{"win32", "pkg", "dmg", "lob"}
	}
	if declared {
		if !slices.Contains(allowed, name) {
			return "", fmt.Errorf("intune type must be %s", strings.Join(allowed, " or "))
		}
		return appTypes[name], nil
	}
	switch req.Identity.Resource.Kind {
	case "WindowsSoftware":
		return win32Type, nil
	case "MacSoftware":
		format := req.Artifact.Format
		if format == "" {
			format = strings.TrimPrefix(strings.ToLower(path.Ext(req.Artifact.Filename)), ".")
		}
		switch {
		case format == "pkg" || format == "dmg":
			return appTypes[format], nil
		case req.Artifact.Path == "":
			// Without an artifact the fields of a PKG and a DMG app are the same.
			return pkgType, nil
		}
		return "", fmt.Errorf("intune has no app type for a %q installer", format)
	}
	return "", errors.New("intune type is required")
}

func text10k(value any) (any, error) {
	s, ok := value.(string)
	if !ok || len(s) > 10000 {
		return nil, errors.New("must be a string of at most 10000 bytes")
	}
	return s, nil
}

func notesText(value any) (any, error) {
	if _, err := text10k(value); err != nil {
		return nil, err
	}
	if strings.Contains(value.(string), "[stemma:v1 ") {
		return nil, errors.New("contains a reserved Stemma marker")
	}
	return value, nil
}

func boolean(value any) (any, error) {
	if _, ok := value.(bool); !ok {
		return nil, errors.New("must be boolean")
	}
	return value, nil
}

func int32Value(value any) (any, error) {
	n, ok := value.(float64)
	if !ok || n < 0 || n > math.MaxInt32 || n != math.Trunc(n) {
		return nil, errors.New("must be a nonnegative Int32")
	}
	return value, nil
}

// windowsArchitectures lists Graph's architecture flags in the order Graph
// writes them, so a declared set compares equal to its readback.
var windowsArchitectures = []string{"x86", "x64", "arm64"}

// architectures translates a set of processor architectures into Graph's
// comma-separated flags; null clears the restriction.
func architectures(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok || len(list) == 0 {
		return nil, errors.New("must be a nonempty list of x86, x64 and arm64, or null")
	}
	declared := map[string]bool{}
	for _, item := range list {
		name := text(item)
		if !slices.Contains(windowsArchitectures, name) || declared[name] {
			return nil, errors.New("must list each of x86, x64 and arm64 at most once")
		}
		declared[name] = true
	}
	var flags []string
	for _, name := range windowsArchitectures {
		if declared[name] {
			flags = append(flags, name)
		}
	}
	return strings.Join(flags, ","), nil
}

// choice translates a declared enum value into its Graph spelling.
func choice(value any, values map[string]string) (string, error) {
	native, ok := values[text(value)]
	if !ok {
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		slices.Sort(names)
		return "", fmt.Errorf("must be one of %s", strings.Join(names, ", "))
	}
	return native, nil
}

var (
	intents       = map[string]string{"required": "required", "available": "available", "uninstall": "uninstall", "available_without_enrollment": "availableWithoutEnrollment"}
	filterModes   = map[string]string{"include": "include", "exclude": "exclude"}
	notifications = map[string]string{"show_all": "showAll", "show_reboot": "showReboot", "hide_all": "hideAll"}
)

// assignments translates each deployment intent and its one target: an Entra
// group, an excluded group, all devices or all users. An included target may
// carry an assignment filter and a notification setting, properties of that
// assignment as in Graph; omitting them keeps what the assignment has.
func assignments(value any) (any, error) {
	list, ok := value.([]any)
	if !ok || len(list) > 1000 {
		return nil, errors.New("must be an array of at most 1000 items; [] clears assignments")
	}
	result := make([]any, 0, len(list))
	seen := map[string]bool{}
	for _, item := range list {
		assignment, ok := item.(object)
		if !ok {
			return nil, errors.New("assignment must be an object")
		}
		if err := fields(assignment, "intent", "group", "exclude_group", "all_devices", "all_users", "filter", "notifications"); err != nil {
			return nil, err
		}
		intent, err := choice(assignment["intent"], intents)
		if err != nil {
			return nil, fmt.Errorf("intent %w", err)
		}
		var targets []object
		for key, kind := range map[string]string{"group": "groupAssignmentTarget", "exclude_group": "exclusionGroupAssignmentTarget"} {
			if value, exists := assignment[key]; exists {
				if text(value) == "" {
					return nil, fmt.Errorf("%s must be an Entra group ID", key)
				}
				targets = append(targets, object{"@odata.type": "#microsoft.graph." + kind, "groupId": value})
			}
		}
		for key, kind := range map[string]string{"all_devices": "allDevicesAssignmentTarget", "all_users": "allLicensedUsersAssignmentTarget"} {
			if value, exists := assignment[key]; exists {
				if value != true {
					return nil, fmt.Errorf("%s must be true", key)
				}
				targets = append(targets, object{"@odata.type": "#microsoft.graph." + kind})
			}
		}
		if len(targets) != 1 {
			return nil, errors.New("assignment requires exactly one of group, exclude_group, all_devices or all_users")
		}
		native := object{"intent": intent, "target": targets[0]}
		_, excluded := assignment["exclude_group"]
		if value, exists := assignment["filter"]; exists {
			if excluded {
				return nil, errors.New("an excluded group takes no filter")
			}
			filter, err := assignmentFilter(value)
			if err != nil {
				return nil, fmt.Errorf("filter %w", err)
			}
			maps.Copy(targets[0], filter)
		}
		if value, exists := assignment["notifications"]; exists {
			if excluded {
				return nil, errors.New("an excluded group takes no notifications")
			}
			setting, err := choice(value, notifications)
			if err != nil {
				return nil, fmt.Errorf("notifications %w", err)
			}
			native["settings"] = object{"@odata.type": "#microsoft.graph.win32LobAppAssignmentSettings", "notifications": setting}
		}
		key := assignmentKey(native)
		if seen[key] {
			return nil, errors.New("duplicate assignment target and intent")
		}
		seen[key] = true
		result = append(result, native)
	}
	return result, nil
}

// assignmentFilter translates a filter reference into its target properties;
// null removes the filter.
func assignmentFilter(value any) (object, error) {
	if value == nil {
		return object{"deviceAndAppManagementAssignmentFilterId": nil, "deviceAndAppManagementAssignmentFilterType": "none"}, nil
	}
	filter, ok := value.(object)
	if !ok {
		return nil, errors.New("must be an object with id and mode, or null")
	}
	if err := fields(filter, "id", "mode"); err != nil {
		return nil, err
	}
	if text(filter["id"]) == "" {
		return nil, errors.New("id must be an Intune assignment filter ID")
	}
	mode, err := choice(filter["mode"], filterModes)
	if err != nil {
		return nil, fmt.Errorf("mode %w", err)
	}
	return object{"deviceAndAppManagementAssignmentFilterId": filter["id"], "deviceAndAppManagementAssignmentFilterType": mode}, nil
}

// includedApps translates the application or package identities that detect a Mac installation.
func includedApps(value any) (any, error) {
	apps, ok := value.([]any)
	if !ok || len(apps) == 0 || len(apps) > 500 {
		return nil, errors.New("must contain between 1 and 500 identifier and version pairs")
	}
	result := make([]any, 0, len(apps))
	seen := map[string]bool{}
	for _, item := range apps {
		app, ok := item.(object)
		if !ok {
			return nil, errors.New("included app must be an object")
		}
		if err := fields(app, "id", "version"); err != nil {
			return nil, err
		}
		id, version := text(app["id"]), text(app["version"])
		if id == "" || version == "" || len(id) > 1000 || len(version) > 1000 {
			return nil, errors.New("included app requires nonempty id and version strings")
		}
		if seen[id] {
			return nil, errors.New("contains duplicate identifiers")
		}
		seen[id] = true
		result = append(result, object{"bundleId": id, "bundleVersion": version})
	}
	return result, nil
}

var (
	runAs    = map[string]string{"system": "system", "user": "user"}
	restarts = map[string]string{"based_on_return_code": "basedOnReturnCode", "allow": "allow", "suppress": "suppress", "force": "force"}
)

func installExperience(value any) (any, error) {
	experience, ok := value.(object)
	if !ok {
		return nil, errors.New("must be an object")
	}
	if err := fields(experience, "run_as", "restart"); err != nil {
		return nil, err
	}
	result := object{}
	for key, values := range map[string]map[string]string{"run_as": runAs, "restart": restarts} {
		value, exists := experience[key]
		if !exists {
			continue
		}
		native, err := choice(value, values)
		if err != nil {
			return nil, fmt.Errorf("%s %w", key, err)
		}
		result[experienceFields[key]] = native
	}
	return result, nil
}

var packageTypes = map[string]string{"per_machine": "perMachine", "per_user": "perUser", "dual_purpose": "dualPurpose"}

func msiInformation(value any) (any, error) {
	info, ok := value.(object)
	if !ok {
		return nil, errors.New("must be an object")
	}
	result := object{}
	for key, value := range info {
		native, known := msiFields[key]
		if !known {
			return nil, fmt.Errorf("unsupported field %q", key)
		}
		switch key {
		case "requires_reboot":
			if _, ok := value.(bool); !ok {
				return nil, errors.New("requires_reboot must be boolean")
			}
		case "package_type":
			converted, err := choice(value, packageTypes)
			if err != nil {
				return nil, fmt.Errorf("package_type %w", err)
			}
			value = converted
		default:
			// A null UpgradeCode clears the one an earlier installer declared.
			if value == nil && key == "upgrade_code" {
				break
			}
			if s, ok := value.(string); !ok || len(s) > 10000 {
				return nil, fmt.Errorf("%s must be a string of at most 10000 bytes", key)
			}
		}
		result[native] = value
	}
	return result, nil
}

var returnTypes = map[string]string{"success": "success", "soft_reboot": "softReboot", "hard_reboot": "hardReboot", "retry": "retry", "failed": "failed"}

func returnCodes(value any) (any, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, errors.New("must be an array")
	}
	result := make([]any, 0, len(list))
	for _, item := range list {
		code, ok := item.(object)
		if !ok {
			return nil, errors.New("return code must be an object")
		}
		if err := fields(code, "code", "type"); err != nil {
			return nil, err
		}
		n, ok := code["code"].(float64)
		if !ok || n < math.MinInt32 || n > math.MaxInt32 || n != math.Trunc(n) {
			return nil, errors.New("code must be an Int32")
		}
		kind, err := choice(code["type"], returnTypes)
		if err != nil {
			return nil, fmt.Errorf("type %w", err)
		}
		result = append(result, object{"returnCode": n, "type": kind})
	}
	return result, nil
}

var (
	operators = map[string]string{
		"equal": "equal", "not_equal": "notEqual", "greater_than": "greaterThan", "greater_than_or_equal": "greaterThanOrEqual",
		"less_than": "lessThan", "less_than_or_equal": "lessThanOrEqual",
	}
	fileProperties     = map[string]string{"exists": "exists", "version": "version", "size_mb": "sizeInMB", "modified": "modifiedDate", "created": "createdDate"}
	registryProperties = map[string]string{"exists": "exists", "does_not_exist": "doesNotExist", "string": "string", "integer": "integer", "version": "version"}
)

// detection translates the complete detection-rule collection. Rules are
// ANDed, with at most one MSI rule, and a script rule must stand alone.
func detection(value any) (any, error) {
	list, ok := value.([]any)
	if !ok || len(list) > 100 {
		return nil, errors.New("must be an array of at most 100 rules")
	}
	result := make([]any, 0, len(list))
	msi := false
	for _, item := range list {
		rule, ok := item.(object)
		if !ok {
			return nil, errors.New("rule must be an object")
		}
		var native object
		var err error
		switch rule["type"] {
		case "msi":
			if msi {
				return nil, errors.New("only one msi rule is supported")
			}
			msi = true
			native, err = msiRule(rule)
		case "file":
			native, err = propertyRule(rule, fileProperties, map[string]string{"path": "path", "name": "fileOrFolderName"}, "exists")
			if native != nil {
				native["@odata.type"] = "#microsoft.graph.win32LobAppFileSystemRule"
			}
		case "registry":
			native, err = propertyRule(rule, registryProperties, map[string]string{"key": "keyPath", "value_name": "valueName"}, "exists", "does_not_exist")
			if native != nil {
				native["@odata.type"] = "#microsoft.graph.win32LobAppRegistryRule"
			}
		case "script":
			if len(list) != 1 {
				return nil, errors.New("a script rule must be the only detection rule")
			}
			native, err = scriptRule(rule)
		default:
			return nil, errors.New("rule type must be msi, file, registry or script")
		}
		if err != nil {
			return nil, err
		}
		native["ruleType"] = "detection"
		result = append(result, native)
	}
	return result, nil
}

func msiRule(rule object) (object, error) {
	if err := fields(rule, "type", "product_code", "product_version", "operator"); err != nil {
		return nil, err
	}
	if text(rule["product_code"]) == "" {
		return nil, errors.New("msi rule requires product_code")
	}
	native := object{"@odata.type": "#microsoft.graph.win32LobAppProductCodeRule", "productCode": rule["product_code"], "productVersionOperator": "notConfigured"}
	if value, exists := rule["operator"]; exists {
		operator, err := choice(value, operators)
		if err != nil {
			return nil, fmt.Errorf("operator %w", err)
		}
		if text(rule["product_version"]) == "" {
			return nil, errors.New("an msi rule with an operator requires product_version")
		}
		native["productVersionOperator"] = operator
	}
	if value, exists := rule["product_version"]; exists {
		if _, ok := value.(string); !ok {
			return nil, errors.New("product_version must be a string")
		}
		native["productVersion"] = value
	}
	return native, nil
}

// propertyRule translates a file or registry rule. An existence check takes
// no operator; every other property compares against a value.
func propertyRule(rule object, properties, locations map[string]string, existence ...string) (object, error) {
	allowed := []string{"type", "check_32bit", "property", "operator", "value"}
	for key := range locations {
		allowed = append(allowed, key)
	}
	if err := fields(rule, allowed...); err != nil {
		return nil, err
	}
	native := object{"operator": "notConfigured"}
	for key, graph := range locations {
		value, exists := rule[key]
		if !exists {
			continue
		}
		if _, ok := value.(string); !ok {
			return nil, fmt.Errorf("%s must be a string", key)
		}
		native[graph] = value
	}
	for _, key := range []string{"path", "name", "key"} {
		if _, located := locations[key]; located && text(rule[key]) == "" {
			return nil, fmt.Errorf("%s rule requires %s", rule["type"], key)
		}
	}
	if value, exists := rule["check_32bit"]; exists {
		if _, ok := value.(bool); !ok {
			return nil, errors.New("check_32bit must be boolean")
		}
		native["check32BitOn64System"] = value
	}
	property, err := choice(rule["property"], properties)
	if err != nil {
		return nil, fmt.Errorf("property %w", err)
	}
	native["operationType"] = property
	_, hasOperator := rule["operator"]
	value, hasValue := rule["value"]
	if slices.Contains(existence, text(rule["property"])) {
		if hasOperator || hasValue {
			return nil, fmt.Errorf("a %s check takes no operator or value", rule["property"])
		}
		return native, nil
	}
	operator, err := choice(rule["operator"], operators)
	if err != nil {
		return nil, fmt.Errorf("operator %w", err)
	}
	comparison, ok := value.(string)
	if !hasValue || !ok || (comparison == "" && rule["property"] != "string") {
		return nil, fmt.Errorf("a %s comparison requires a value", rule["property"])
	}
	native["operator"], native["comparisonValue"] = operator, comparison
	return native, nil
}

// scriptRule carries the PowerShell text; Graph's base64 encoding stays here.
func scriptRule(rule object) (object, error) {
	if err := fields(rule, "type", "script", "enforce_signature_check", "run_as_32bit"); err != nil {
		return nil, err
	}
	script := text(rule["script"])
	if script == "" || len(script) > 200000 {
		return nil, errors.New("script must be PowerShell text of at most 200000 bytes")
	}
	native := object{"@odata.type": "#microsoft.graph.win32LobAppPowerShellScriptRule", "scriptContent": base64.StdEncoding.EncodeToString([]byte(script))}
	for key, graph := range map[string]string{"enforce_signature_check": "enforceSignatureCheck", "run_as_32bit": "runAs32Bit"} {
		if value, exists := rule[key]; exists {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be boolean", key)
			}
			native[graph] = value
		}
	}
	return native, nil
}

// reportName names a Graph property path the way declarations do, so plans
// and origins read in the catalog's terms.
func reportName(property string) string {
	head, rest, nested := strings.Cut(property, ".")
	name := head
	if derived, ok := derivedNames[property]; ok {
		return derived
	}
	for _, table := range []map[string]field{commonFields, macFields, lobFields, win32Fields} {
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
