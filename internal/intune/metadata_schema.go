package intune

import (
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/plugin"
)

// MetadataSchema describes Intune app metadata. The software kind fixes the
// platform: Windows software is a Win32 app, and a Mac app follows its PKG or
// DMG installer unless it declares a line-of-business app. Creation
// requirements are checked after discovery, so an existing app can manage only
// selected fields. Omission preserves a field; lists replace their collection.
func MetadataSchema() *jsonschema.Schema {
	common := func() map[string]*jsonschema.Schema {
		return map[string]*jsonschema.Schema{
			"app_id":          {Type: "string", MinLength: new(uint64(1)), Description: "Pin this software to an existing Intune app, which must exist and must not carry another software's identity marker. Omit to find the app by the identity marker in its notes, or create one."},
			"display_name":    textSchema("Company Portal name. Required for creation."),
			"description":     textSchema("App description. Required for creation."),
			"publisher":       textSchema("Publisher name. Required for creation."),
			"developer":       textSchema("Developer name."),
			"owner":           textSchema("Owner label."),
			"notes":           textSchema("Administrator notes. A reserved marker line follows them: it identifies the app and records its published content, so it must stay intact."),
			"information_url": textSchema("Publisher information URL."),
			"privacy_url":     textSchema("Publisher privacy information URL."),
			"featured":        {Type: "boolean", Description: "Show the app as featured. Explicit false is managed."},
			"assignments":     assignmentSchema(),
			"retention":       objectSchema(map[string]*jsonschema.Schema{"keep": {Type: "integer", Minimum: "1", Description: "Keep the active content version and the N-1 newest other committed versions of this app, ordered by version number. Every other version is deleted, including uploads that never committed a file."}}, "keep"),
		}
	}
	win32 := common()
	win32["type"] = &jsonschema.Schema{Const: "win32", Description: "Windows software publishes a Win32 app, so this is the default."}
	win32["msi"] = msiSchema()
	win32["dependencies"] = referencesSchema("auto_install", 99)
	win32["supersedes"] = referencesSchema("uninstall_previous", 9)
	win32["msi_properties"] = &jsonschema.Schema{
		Type: "object", MinProperties: new(uint64(1)), PropertyNames: &jsonschema.Schema{Pattern: "^[A-Za-z_][A-Za-z0-9_.]*$"},
		AdditionalProperties: &jsonschema.Schema{Type: "string", Pattern: "^[^\\r\\n\\x00]*$"},
		Description:          "Windows Installer properties the derived install command passes to the selected setup MSI, such as PORTAL for GlobalProtect. Values are quoted in name order. Use instead of install_command.",
	}
	win32["install_command"] = textSchema("Silent Windows install command. Selected MSI content defaults to msiexec /i with /qn /norestart; EXE switches and architecture-specific executable paths are explicit. Payloads and hooks are never executed during preparation.")
	win32["uninstall_command"] = textSchema("Windows uninstall command. Selected MSI content defaults to msiexec /x ProductCode /qn /norestart; EXE uninstall behavior is explicit.")
	win32["minimum_windows_release"] = textSchema("Minimum Windows release, such as Windows11_23H2. Required for creation.")
	win32["architectures"] = &jsonschema.Schema{
		AnyOf:       []*jsonschema.Schema{{Type: "array", MinItems: new(uint64(1)), UniqueItems: true, Items: &jsonschema.Schema{Enum: []any{"x86", "x64", "arm64"}}}, {Type: "null"}},
		Description: "Processor architectures the app installs on, such as [x64, arm64] for an x64 app that also runs emulated on Arm. Null clears the restriction on an existing app; a list is required for creation.",
	}
	for key, description := range map[string]string{
		"minimum_disk_space_mb": "Minimum free disk space in MB.",
		"minimum_memory_mb":     "Minimum memory in MB.",
		"minimum_processors":    "Minimum number of processors.",
		"minimum_cpu_speed_mhz": "Minimum CPU speed in MHz.",
	} {
		win32[key] = &jsonschema.Schema{Type: "integer", Minimum: "0", Maximum: "2147483647", Description: description + " Omit to leave unchanged."}
	}
	win32["install_experience"] = objectSchema(map[string]*jsonschema.Schema{
		"run_as":  enumSchema("Install as system or user. Required for creation.", "system", "user"),
		"restart": enumSchema("Installer restart handling; omitted fields are preserved.", "based_on_return_code", "allow", "suppress", "force"),
	})
	win32["detection"] = detectionSchema()
	win32["return_codes"] = &jsonschema.Schema{Type: "array", Description: "Replace the return-code collection; an empty array clears it.", Items: objectSchema(map[string]*jsonschema.Schema{
		"code": {Type: "integer", Minimum: "-2147483648", Maximum: "2147483647"},
		"type": enumSchema("Result classification.", "success", "soft_reboot", "hard_reboot", "retry", "failed"),
	}, "code", "type")}
	mac := common()
	mac["type"] = &jsonschema.Schema{Enum: []any{"pkg", "dmg"}, Description: "A Mac app follows its installer: pkg for a PKG and dmg for a disk image."}
	mac["ignore_version_detection"] = &jsonschema.Schema{Type: "boolean", Description: "Ignore installed app versions during detection. Explicit false is managed."}
	mac["included_apps"] = &jsonschema.Schema{Type: "array", MinItems: new(uint64(1)), MaxItems: new(uint64(500)), Description: "Identifiers and versions that detect the installation; the first identifies the app. A PKG app accepts package receipts as well as applications. Derived from applications, selected first, or from receipts when a PKG installs no applications, as a payloadless package does. Line-of-business apps only include applications installed under /Applications. Explicit entries replace the derived list. Identifiers must be unique.", Items: objectSchema(map[string]*jsonschema.Schema{
		"id":      {Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(1000)), Description: "Application bundle identifier, or package receipt identifier for a PKG app."},
		"version": {Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(1000)), Description: "Application CFBundleShortVersionString or package receipt version."},
	}, "id", "version")}
	lob := common()
	lob["type"] = &jsonschema.Schema{Const: "lob", Description: "Publish the signed PKG as a line-of-business app. The PKG must install an application under /Applications."}
	lob["ignore_version_detection"], lob["included_apps"] = mac["ignore_version_detection"], mac["included_apps"]
	lob["install_as_managed"] = &jsonschema.Schema{Type: "boolean", Description: "Install the app as managed on macOS 11 or later. The PKG must have one component that installs one application under /Applications."}
	windows, macOS, lineOfBusiness := objectSchema(win32), objectSchema(mac), objectSchema(lob, "type")
	windows.Not = &jsonschema.Schema{Required: []string{"install_command", "msi_properties"}}
	windows.Title, macOS.Title, lineOfBusiness.Title = "Win32 app", "Mac app", "Mac line-of-business app"
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{windows, macOS, lineOfBusiness}, Description: "Intune app metadata. Windows software publishes a Win32 envelope; Mac software publishes its PKG or DMG, or a signed PKG as a line-of-business app. The minimum macOS derives from the software's minimum_os and its installer."}
}

func textSchema(description string) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: description}
}

func referencesSchema(flag string, limit uint64) *jsonschema.Schema {
	r := jsonschema.Reflector{DoNotReference: true}
	reference := r.Reflect(plugin.ResourceReference{})
	reference.ID = ""
	item := objectSchema(map[string]*jsonschema.Schema{
		"resource": reference,
		"app_id":   {Type: "string", MinLength: new(uint64(1))},
		flag:       {Type: "boolean"},
	}, flag)
	item.OneOf = []*jsonschema.Schema{{Required: []string{"resource"}}, {Required: []string{"app_id"}}}
	return &jsonschema.Schema{Type: "array", MaxItems: new(limit), Description: "Own this outgoing relationship category; [] clears it. Use resource for a publication on the same connection, or app_id for an external app.", Items: item}
}

func msiSchema() *jsonschema.Schema {
	p := map[string]*jsonschema.Schema{
		"requires_reboot": {Type: "boolean"},
		"package_type":    enumSchema("MSI installation scope.", "per_machine", "per_user", "dual_purpose"),
		"upgrade_code":    {AnyOf: []*jsonschema.Schema{textSchema(""), {Type: "null"}}},
	}
	for _, key := range []string{"product_code", "product_version", "product_name", "publisher"} {
		p[key] = textSchema("")
	}
	schema := objectSchema(p)
	schema.Description = "MSI information. The selected setup MSI supplies it; declared fields override it."
	return schema
}

func objectSchema(properties map[string]*jsonschema.Schema, required ...string) *jsonschema.Schema {
	ordered := orderedmap.New[string, *jsonschema.Schema]()
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		ordered.Set(key, properties[key])
	}
	return &jsonschema.Schema{Type: "object", Properties: ordered, Required: required, AdditionalProperties: jsonschema.FalseSchema}
}

//go:fix inline
func enumSchema(description string, values ...string) *jsonschema.Schema {
	valuesAny := make([]any, len(values))
	for i, v := range values {
		valuesAny[i] = v
	}
	return &jsonschema.Schema{Type: "string", Enum: valuesAny, Description: description}
}

func assignmentSchema() *jsonschema.Schema {
	item := objectSchema(map[string]*jsonschema.Schema{
		"intent":        enumSchema("Deployment intent.", "required", "available", "uninstall", "available_without_enrollment"),
		"group":         {Type: "string", MinLength: new(uint64(1)), Description: "Entra group object ID to target."},
		"exclude_group": {Type: "string", MinLength: new(uint64(1)), Description: "Entra group object ID to exclude."},
		"all_devices":   {Const: true, Description: "Target all devices."},
		"all_users":     {Const: true, Description: "Target all licensed users."},
		"filter": {AnyOf: []*jsonschema.Schema{objectSchema(map[string]*jsonschema.Schema{
			"id":   {Type: "string", MinLength: new(uint64(1)), Description: "Intune assignment filter ID."},
			"mode": enumSchema("Include or exclude the devices the filter matches.", "include", "exclude"),
		}, "id", "mode"), {Type: "null"}}, Description: "Assignment filter for an included target; null removes it. Omit to keep the assignment's filter."},
		"notifications": enumSchema("Win32 end-user notifications for an included target. Omit to keep the assignment's setting.", "show_all", "show_reboot", "hide_all"),
	}, "intent")
	item.OneOf = []*jsonschema.Schema{{Required: []string{"group"}}, {Required: []string{"exclude_group"}}, {Required: []string{"all_devices"}}, {Required: []string{"all_users"}}}
	item.If = &jsonschema.Schema{Required: []string{"exclude_group"}}
	item.Then = &jsonschema.Schema{Not: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Required: []string{"filter"}}, {Required: []string{"notifications"}}}}}
	return &jsonschema.Schema{Type: "array", MaxItems: new(uint64(1000)), Description: "Own the complete assignment collection. Omission preserves targeting; [] clears assignments. Each assignment has one target, and settings it omits keep their values on a matching target.", Items: item}
}

func detectionSchema() *jsonschema.Schema {
	operator := func(description string) *jsonschema.Schema {
		return enumSchema(description, "equal", "not_equal", "greater_than", "greater_than_or_equal", "less_than", "less_than_or_equal")
	}
	absent := &jsonschema.Schema{Not: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Required: []string{"operator"}}, {Required: []string{"value"}}}}}
	// A version comparison defaults to greater_than_or_equal the managed version.
	compared := &jsonschema.Schema{
		If:   &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"property": {Const: "version"}}).Properties},
		Else: &jsonschema.Schema{Required: []string{"operator", "value"}},
	}
	msi := objectSchema(map[string]*jsonschema.Schema{
		"type":            {Const: "msi"},
		"product_code":    {Type: "string", MinLength: new(uint64(1)), Description: "Exact MSI ProductCode GUID. Major MSI upgrades can change it; a version comparison only detects installations with this code. Declared detection overrides selected MSI defaults."},
		"product_version": textSchema("Version compared with the operator."),
		"operator":        operator("Comparison operator."),
	}, "type", "product_code")
	msi.If = &jsonschema.Schema{Required: []string{"operator"}}
	msi.Then = &jsonschema.Schema{Required: []string{"product_version"}}
	file := objectSchema(map[string]*jsonschema.Schema{
		"type":        {Const: "file"},
		"path":        {Type: "string", MinLength: new(uint64(1))},
		"name":        {Type: "string", MinLength: new(uint64(1)), Description: "File or folder name."},
		"check_32bit": {Type: "boolean", Description: "Check the 32-bit location on 64-bit Windows."},
		"property":    enumSchema("Detected file property.", "exists", "version", "size_mb", "modified", "created"),
		"operator":    operator("Comparison operator. A version comparison defaults to greater_than_or_equal."),
		"value":       {Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(10000)), Description: "Compared value. A version comparison defaults to the managed version. A stable file path with version greater_than_or_equal detects already-newer installations; equal intentionally does not."},
	}, "type", "path", "name", "property")
	file.If = &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"property": {Const: "exists"}}).Properties}
	file.Then = absent
	file.Else = compared
	registry := objectSchema(map[string]*jsonschema.Schema{
		"type":        {Const: "registry"},
		"key":         {Type: "string", MinLength: new(uint64(1)), Description: "Registry key path."},
		"value_name":  {Type: "string"},
		"check_32bit": {Type: "boolean", Description: "Check the 32-bit view on 64-bit Windows."},
		"property":    enumSchema("Registry comparison.", "exists", "does_not_exist", "string", "integer", "version"),
		"operator":    operator("Comparison operator. A version comparison defaults to greater_than_or_equal."),
		"value":       {Type: "string", Description: "Compared value. A version comparison defaults to the managed version. Version greater_than_or_equal can detect already-newer installations at a stable vendor registry value."},
	}, "type", "key", "property")
	registry.If = &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"property": {Enum: []any{"exists", "does_not_exist"}}}).Properties}
	registry.Then = absent
	registry.Else = compared
	script := objectSchema(map[string]*jsonschema.Schema{
		"type":                    {Const: "script"},
		"script":                  {Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(200000)), Description: "PowerShell detection script. Detection requires exit code 0, nonempty STDOUT and empty STDERR. Runs in the app install context. This is an alternative to other rules; preparation never executes it."},
		"enforce_signature_check": {Type: "boolean"},
		"run_as_32bit":            {Type: "boolean"},
	}, "type", "script")
	return &jsonschema.Schema{Type: "array", MaxItems: new(uint64(100)), Description: "Complete detection-rule collection. Selected MSI content defaults to its ProductCode with product_version greater_than_or_equal; a newer major version with another ProductCode needs file, registry or script detection. Rules are ANDed, with at most one msi rule. A script rule must be the only rule. Requirement rules are unsupported.", Items: &jsonschema.Schema{OneOf: []*jsonschema.Schema{msi, file, registry, script}}, Contains: msi, MinContains: new(uint64(0)), MaxContains: new(uint64(1)), If: &jsonschema.Schema{Contains: script}, Then: &jsonschema.Schema{MaxItems: new(uint64(1))}}
}

// JSONSchemaExtend advertises the two complete authentication shapes.
func (Config) JSONSchemaExtend(schema *jsonschema.Schema) {
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"token"}, Not: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Required: []string{"tenant_id"}}, {Required: []string{"client_id"}}, {Required: []string{"client_secret"}}}}},
		{Required: []string{"tenant_id", "client_id", "client_secret"}, Not: &jsonschema.Schema{Required: []string{"token"}}},
	}
	schema.Description = "Shared Graph connection. Publications are identified by native identity markers; app adoption belongs to destination metadata."
}
