package intune

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

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
	"categories":      {"categories", categoryNames},
	"assignments":     {"assignments", assignments},
}

var macFields = map[string]field{
	"included_apps":            {"includedApps", includedApps},
	"ignore_version_detection": {"ignoreVersionDetection", boolean},
}

// pkgFields apply only to a Mac PKG app. Scripts can embed environment
// values, so reports name these fields without their values.
var pkgFields = map[string]field{
	"pre_install_script":  {"preInstallScript", installScript},
	"post_install_script": {"postInstallScript", installScript},
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

// declaration validates metadata and resolves its app type without changing
// its vocabulary or field presence.
func declaration(req plugin.ReconcileRequest[Config]) (object, error) {
	declared, err := decodeObject(req.Metadata)
	if err != nil {
		return nil, err
	}
	appType, err := resolveType(req, declared)
	if err != nil {
		return nil, err
	}
	for key, value := range declared {
		switch key {
		case "type":
			continue
		case "app_id":
			if text(value) == "" {
				return nil, errors.New("app_id must be a nonempty adopted app ID")
			}
			continue
		case "dependencies", "supersedes":
			if appType != "win32" {
				return nil, fmt.Errorf("%s requires a Win32 app", key)
			}
			continue
		case "msi_properties":
			if appType != "win32" {
				return nil, errors.New("msi_properties requires a Win32 app")
			}
			if _, commanded := declared["install_command"]; commanded {
				return nil, errors.New("set msi_properties or install_command, not both")
			}
			if err := validateMSIProperties(value); err != nil {
				return nil, fmt.Errorf("msi_properties: %w", err)
			}
			continue
		}
		spec, ok := metadataField(appType, key)
		if _, scripted := pkgFields[key]; scripted && appType != "pkg" {
			if appType == "dmg" && req.Identity.Resource.Kind == "MacSoftware" {
				return nil, fmt.Errorf("%s requires a PKG app; set package to publish the application in a PKG", key)
			}
			return nil, fmt.Errorf("%s requires a PKG app", key)
		}
		if !ok {
			return nil, fmt.Errorf("unsupported Intune %s field %q", appType, key)
		}
		if key == "included_apps" {
			declared[key], err = resolveIncludedApps(value, appType)
			if err != nil {
				return nil, fmt.Errorf("included_apps: %w", err)
			}
			continue
		}
		if _, err := spec.convert(value); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	if list, assigning := declared["assignments"].([]any); assigning && appType != "win32" {
		for _, item := range list {
			if _, set := item.(object)["notifications"]; set {
				return nil, errors.New("assignment notifications require a Win32 app")
			}
		}
	}
	declared["type"] = appType
	return declared, nil
}

func metadataField(appType, key string) (field, bool) {
	if spec, ok := commonFields[key]; ok {
		return spec, true
	}
	if appType == "win32" {
		spec, ok := win32Fields[key]
		return spec, ok
	}
	if spec, ok := macFields[key]; ok {
		return spec, true
	}
	if appType == "lob" {
		spec, ok := lobFields[key]
		return spec, ok
	}
	if appType == "pkg" {
		spec, ok := pkgFields[key]
		return spec, ok
	}
	return field{}, false
}

// compile translates desired metadata at the Graph boundary. Derived-only
// values are never accepted as declaration fields.
func compile(desired object) (object, error) {
	appType := text(desired["type"])
	m := object{"@odata.type": appTypes[appType]}
	for key, value := range desired {
		switch key {
		case "type", "msi_properties":
			continue
		case "app_id", "dependencies", "supersedes":
			m[key] = value
			continue
		case "setup_file":
			m["setupFilePath"] = value
			continue
		case "minimum_os":
			m["minimumSupportedOperatingSystem"] = object{"v" + strings.ReplaceAll(text(value), ".", "_"): true}
			continue
		case "primary_bundle_id":
			property := "primaryBundleId"
			if appType == "lob" {
				property = "bundleId"
			}
			m[property] = value
			continue
		case "primary_bundle_version":
			property := "primaryBundleVersion"
			if appType == "lob" {
				property = "buildNumber"
			}
			m[property] = value
			continue
		case "primary_bundle_build":
			m["versionNumber"] = value
			continue
		case "included_apps":
			if appType == "lob" {
				apps := value.([]any)
				children := make([]any, len(apps))
				for i, item := range apps {
					app := item.(object)
					build := app["build"]
					if build == nil {
						build = app["version"]
					}
					children[i] = object{"bundleId": app["id"], "buildNumber": app["version"], "versionNumber": build}
				}
				m["childApps"] = children
				continue
			}
		}
		spec, ok := metadataField(appType, key)
		if !ok {
			return nil, fmt.Errorf("unsupported Intune %s field %q", appType, key)
		}
		converted, err := spec.convert(value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
		m[spec.graph] = converted
	}
	return m, nil
}

// resolveType selects the app type. The software kind fixes the platform,
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
		return name, nil
	}
	switch req.Identity.Resource.Kind {
	case "WindowsSoftware":
		return "win32", nil
	case "MacSoftware":
		format := req.Artifact.Format
		if format == "" {
			format = strings.TrimPrefix(strings.ToLower(path.Ext(req.Artifact.Filename)), ".")
		}
		switch {
		case format == "pkg" || format == "dmg":
			return format, nil
		case req.Artifact.SHA256 == "":
			// Without an artifact the format is unknown; a PKG app takes every
			// field a DMG app does.
			return "pkg", nil
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

// maxInstallScript is Intune's limit on a PKG app script, in characters.
const maxInstallScript = 15359

// installScript translates script text into the encoded script Graph stores;
// null removes the script.
func installScript(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	script, ok := value.(string)
	if !ok || script == "" || utf8.RuneCountInString(script) > maxInstallScript {
		return nil, fmt.Errorf("must be script text of at most %d characters, or null", maxInstallScript)
	}
	return object{"scriptContent": base64.StdEncoding.EncodeToString([]byte(script))}, nil
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

func categoryNames(value any) (any, error) {
	list, ok := value.([]any)
	if !ok {
		return nil, errors.New("must be an array of category names; [] clears categories")
	}
	seen := map[string]bool{}
	for _, item := range list {
		name, ok := item.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, errors.New("category must be a nonempty name")
		}
		if seen[name] {
			return nil, fmt.Errorf("duplicate category %q", name)
		}
		seen[name] = true
	}
	return list, nil
}

// msiProperty matches a Windows Installer property name.
var msiProperty = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.]*$`)

// validateMSIProperties checks the public properties a derived msiexec
// command passes, as property names and single-line text values.
func validateMSIProperties(value any) error {
	properties, ok := value.(object)
	if !ok || len(properties) == 0 {
		return errors.New("must map at least one property name to a text value")
	}
	for name, value := range properties {
		if !msiProperty.MatchString(name) {
			return fmt.Errorf("%q is not a Windows Installer property name", name)
		}
		text, ok := value.(string)
		if !ok || strings.ContainsAny(text, "\r\n\x00") {
			return fmt.Errorf("%s must be a single-line text value", name)
		}
	}
	return nil
}

// resolveIncludedApps normalizes native pairs and inspected subjects to the
// identities that detect a Mac installation.
func resolveIncludedApps(value any, appType string) ([]any, error) {
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
		if _, subject := app["kind"]; subject {
			var err error
			app, err = includedSubject(app, appType)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", len(result)+1, err)
			}
		} else if err := fields(app, "id", "version"); err != nil {
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
		result = append(result, app)
	}
	return result, nil
}

func includedSubject(item object, appType string) (object, error) {
	var subject plugin.Subject
	decoder := json.NewDecoder(bytes.NewReader(raw(item)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&subject); err != nil {
		return nil, fmt.Errorf("invalid inspected subject: %w", err)
	}
	var selected *plugin.Subject
	switch {
	case subject.Kind == "app" && subject.App != nil:
		selected = &subject
	case subject.Kind == "package" && subject.Package != nil:
		if appType != "pkg" {
			return nil, errors.New("a package subject requires a PKG app")
		}
	default:
		return nil, errors.New("expected an identifier and version pair or an inspected application or package, such as {{ facts.name }}")
	}
	apps, _, err := detectedApps(plugin.Facts{Subjects: []plugin.Subject{subject}}, selected, appType)
	if err != nil {
		return nil, err
	}
	app := apps[0]
	result := object{"id": app.id, "version": app.version}
	if appType == "lob" {
		result["build"] = app.build
	}
	return result, nil
}

func includedApps(value any) (any, error) {
	apps := value.([]any)
	result := make([]any, len(apps))
	for i, item := range apps {
		app := item.(object)
		result[i] = object{"bundleId": app["id"], "bundleVersion": app["version"]}
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

// reportName names a Graph property path the way declarations do, so plans
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
