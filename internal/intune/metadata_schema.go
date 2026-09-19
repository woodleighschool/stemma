package intune

import (
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
	"github.com/woodleighschool/stemma/plugin"
)

// MetadataSchema describes the supported native Graph metadata and software adoption.
// Creation requirements are checked after discovery, so existing apps can manage
// only selected fields. Omission preserves a field; collections replace membership.
func MetadataSchema() *jsonschema.Schema {
	variants := make([]*jsonschema.Schema, 0, 3)
	for _, appType := range []string{win32Type, dmgType, pkgType} {
		alias := map[string]string{win32Type: "win32", dmgType: "dmg", pkgType: "pkg"}[appType]
		p := map[string]*jsonschema.Schema{
			"type":                  {Const: alias, Description: "Short name for the native app subtype. Must agree with @odata.type if both are supplied."},
			"@odata.type":           {Const: appType, Description: "Native Graph app subtype. macOS apps use beta for current OS requirements; Win32 uses v1.0."},
			"app_id":                {Type: "string", MinLength: new(uint64(1)), Description: "Pin this software to an existing Intune app, which must exist and must not carry another software's identity marker. Omit to find the app by the identity marker in its notes, or create one."},
			"displayName":           {Type: "string", MaxLength: new(uint64(10000)), Description: "Company Portal display name. Required for creation."},
			"description":           {Type: "string", MaxLength: new(uint64(10000)), Description: "Native app description. Required for creation."},
			"publisher":             {Type: "string", MaxLength: new(uint64(10000)), Description: "Native publisher name. Required for creation."},
			"privacyInformationUrl": {Type: "string", MaxLength: new(uint64(10000)), Description: "Publisher privacy information URL."},
			"informationUrl":        {Type: "string", MaxLength: new(uint64(10000)), Description: "Publisher information URL."},
			"owner":                 {Type: "string", MaxLength: new(uint64(10000)), Description: "App owner label."},
			"developer":             {Type: "string", MaxLength: new(uint64(10000)), Description: "App developer label."},
			"notes":                 {Type: "string", MaxLength: new(uint64(10000)), Description: "Administrator notes. A reserved marker line follows them: it identifies the app and records its published content, so it must stay intact."},
			"isFeatured":            {Type: "boolean", Description: "Show the app as featured. Explicit false is managed."},
			"assignments":           assignmentSchema(),
			"retention":             objectSchema(map[string]*jsonschema.Schema{"keep": {Type: "integer", Minimum: "1", Description: "Keep the active content version and the N-1 newest other committed versions of this app, ordered by version number. Every other version is deleted, including uploads that never committed a file."}}, "keep"),
		}
		if appType == win32Type {
			p["content"] = objectSchema(map[string]*jsonschema.Schema{"setup_file": {Type: "string", MinLength: new(uint64(1)), Description: "Relative entrypoint inside the immutable setup tree. Defaults to the artifact entrypoint, or the filename for a single file; conflicting entries are rejected. All tree members are included in the provider-prepared Intune envelope."}}, "setup_file")
			p["derive"] = objectSchema(map[string]*jsonschema.Schema{"msi": {Type: "string", MinLength: new(uint64(1)), Description: "Named subject for explicit MSI descriptive and identity selection. Selected setup MSI content supplies standard commands and detection defaults; declared native fields override them."}}, "msi")
			p["msiInformation"] = msiSchema()
			p["dependencies"] = referencesSchema("auto_install", 99)
			p["supersedes"] = referencesSchema("uninstall_previous", 9)
			p["installCommandLine"] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: "Silent Windows install command line. Selected MSI content defaults to msiexec /i with /qn /norestart; EXE switches and architecture-specific executable paths are explicit. Payloads and hooks are never executed during preparation."}
			p["uninstallCommandLine"] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: "Windows uninstall command line. Selected MSI content defaults to msiexec /x ProductCode /qn /norestart; EXE uninstall behavior is explicit."}
			p["minimumSupportedWindowsRelease"] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: "Native minimum Windows release, such as Windows11_23H2. Required for creation."}
			p["allowedArchitectures"] = &jsonschema.Schema{Enum: []any{"x86", "x64", "arm64", nil}, Description: "Supported processor architecture; null clears it on an existing app. A non-null value is required for creation."}
			for _, key := range []string{"minimumFreeDiskSpaceInMB", "minimumMemoryInMB", "minimumNumberOfProcessors", "minimumCpuSpeedInMHz"} {
				p[key] = &jsonschema.Schema{Type: "integer", Minimum: "0", Maximum: "2147483647", Description: "Native minimum hardware requirement. Omit to leave unchanged."}
			}
			p["installExperience"] = objectSchema(map[string]*jsonschema.Schema{
				"@odata.type":           {Const: "#microsoft.graph.win32LobAppInstallExperience"},
				"runAsAccount":          enumSchema("Install as system or user. Required for creation.", "system", "user"),
				"deviceRestartBehavior": enumSchema("Installer restart handling; omitted fields are preserved.", "basedOnReturnCode", "allow", "suppress", "force"),
			})
			p["rules"] = rulesSchema()
			p["returnCodes"] = &jsonschema.Schema{Type: "array", Description: "Replace the return-code collection; an empty array clears it.", Items: objectSchema(map[string]*jsonschema.Schema{
				"@odata.type": {Const: "#microsoft.graph.win32LobAppReturnCode"},
				"returnCode":  {Type: "integer", Minimum: "-2147483648", Maximum: "2147483647"},
				"type":        enumSchema("Native result classification.", "success", "softReboot", "hardReboot", "retry", "failed"),
			}, "returnCode", "type")}
		} else {
			p["derive"] = objectSchema(map[string]*jsonschema.Schema{"app": {Type: "string", MinLength: new(uint64(1)), Description: "Named application subject supplying bundle identity, short version, display name and exact minimum OS."}}, "app")
			p["primaryBundleId"] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: "Primary application CFBundleIdentifier. Required for creation; read from the vendor artifact, not the filename."}
			p["primaryBundleVersion"] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: "Primary application CFBundleShortVersionString. Required for creation."}
			p["ignoreVersionDetection"] = &jsonschema.Schema{Type: "boolean", Description: "Ignore installed app versions during detection. Explicit false is managed."}
			p["includedApps"] = &jsonschema.Schema{Type: "array", MinItems: new(uint64(1)), MaxItems: new(uint64(500)), Description: "Complete collection of included bundle IDs and versions. Required for creation; bundle IDs must be unique.", Items: objectSchema(map[string]*jsonschema.Schema{
				"@odata.type":   {Const: "#microsoft.graph.macOSIncludedApp"},
				"bundleId":      {Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(1000)), Description: "Application CFBundleIdentifier."},
				"bundleVersion": {Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(1000)), Description: "Application CFBundleShortVersionString."},
			}, "bundleId", "bundleVersion")}
			operatingSystem := map[string]*jsonschema.Schema{"@odata.type": {Const: "#microsoft.graph.macOSMinimumOperatingSystem"}}
			for _, key := range minimumOSFields() {
				operatingSystem[key] = &jsonschema.Schema{Type: "boolean", Description: "Select exactly one version with true. Selecting a version clears the previous minimum OS selection."}
			}
			p["minimumSupportedOperatingSystem"] = objectSchema(operatingSystem)
			p["minimumSupportedOperatingSystem"].Description = "One minimum OS selection encoded as native Graph booleans. Exactly one version must be true. Required for creation."
		}
		variant := objectSchema(p)
		variant.AnyOf = []*jsonschema.Schema{{Required: []string{"@odata.type"}}, {Required: []string{"type"}}}
		if appType == win32Type {
			variant.AnyOf = append(variant.AnyOf, &jsonschema.Schema{Required: []string{"derive"}})
		}
		variant.Title = appType
		variants = append(variants, variant)
	}
	return &jsonschema.Schema{OneOf: variants, Description: "Native Intune metadata. Supports Win32 envelopes, raw macOS DMG and PKG (beta). Sources are preserved; signing and installer building are separate software policies. Fields not described here, including macOS scripts and managed macOSLobApp, are unsupported."}
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
		"@odata.type":    {Const: "#microsoft.graph.win32LobAppMsiInformation"},
		"requiresReboot": {Type: "boolean"},
		"packageType":    enumSchema("MSI installation scope.", "perMachine", "perUser", "dualPurpose"),
	}
	for _, key := range []string{"productCode", "productVersion", "upgradeCode", "productName", "publisher"} {
		p[key] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000))}
	}
	return objectSchema(p)
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
	var targets []*jsonschema.Schema
	for _, kind := range []string{"groupAssignmentTarget", "exclusionGroupAssignmentTarget", "allDevicesAssignmentTarget", "allLicensedUsersAssignmentTarget"} {
		p := map[string]*jsonschema.Schema{"@odata.type": {Const: "#microsoft.graph." + kind}}
		required := []string{"@odata.type"}
		if kind == "groupAssignmentTarget" || kind == "exclusionGroupAssignmentTarget" {
			p["groupId"] = &jsonschema.Schema{Type: "string", MinLength: new(uint64(1))}
			required = append(required, "groupId")
		}
		targets = append(targets, objectSchema(p, required...))
	}
	return &jsonschema.Schema{Type: "array", MaxItems: new(uint64(1000)), Description: "Own the complete assignment collection. Omission preserves targeting; [] clears assignments. Existing settings on matching targets are preserved. Filters and custom assignment settings cannot be configured.", Items: objectSchema(map[string]*jsonschema.Schema{
		"@odata.type": {Const: "#microsoft.graph.mobileAppAssignment"},
		"intent":      enumSchema("Native deployment intent.", "available", "required", "uninstall", "availableWithoutEnrollment"),
		"target":      {OneOf: targets},
	}, "intent", "target")}
}
func rulesSchema() *jsonschema.Schema {
	operator := func() *jsonschema.Schema {
		return enumSchema("Native comparison operator.", "notConfigured", "equal", "notEqual", "greaterThan", "greaterThanOrEqual", "lessThan", "lessThanOrEqual")
	}
	product := objectSchema(map[string]*jsonschema.Schema{
		"@odata.type": {Const: "#microsoft.graph.win32LobAppProductCodeRule"}, "ruleType": {Const: "detection"},
		"productCode":            {Type: "string", MinLength: new(uint64(1)), Description: "Exact MSI ProductCode GUID. Major MSI upgrades can change it; a version comparison only detects installations with this code. Declared detection overrides selected MSI defaults."},
		"productVersionOperator": operator(), "productVersion": {Type: "string", MaxLength: new(uint64(10000)), Description: "Version used when comparison is configured."},
	}, "@odata.type", "ruleType", "productCode", "productVersionOperator")
	product.If = &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"productVersionOperator": {Not: &jsonschema.Schema{Const: "notConfigured"}}}).Properties, Required: []string{"productVersionOperator"}}
	product.Then = &jsonschema.Schema{Required: []string{"productVersion"}}
	file := objectSchema(map[string]*jsonschema.Schema{
		"@odata.type": {Const: "#microsoft.graph.win32LobAppFileSystemRule"}, "ruleType": {Const: "detection"},
		"path": {Type: "string", MinLength: new(uint64(1))}, "fileOrFolderName": {Type: "string", MinLength: new(uint64(1))},
		"check32BitOn64System": {Type: "boolean"}, "operationType": enumSchema("Detected file property.", "exists", "version", "sizeInMB", "modifiedDate", "createdDate"),
		"operator": operator(), "comparisonValue": {Type: "string", MaxLength: new(uint64(10000)), Description: "Value used when comparison is configured. A stable file path with version greaterThanOrEqual detects already-newer installations; equal intentionally does not."},
	}, "@odata.type", "ruleType", "path", "fileOrFolderName", "operationType", "operator")
	file.If = &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"operationType": {Const: "exists"}}).Properties}
	file.Then = &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"operator": {Const: "notConfigured"}}).Properties}
	file.Else = &jsonschema.Schema{Required: []string{"comparisonValue"}, Properties: objectSchema(map[string]*jsonschema.Schema{"operator": {Not: &jsonschema.Schema{Const: "notConfigured"}}, "comparisonValue": {MinLength: new(uint64(1))}}).Properties}
	registry := objectSchema(map[string]*jsonschema.Schema{
		"@odata.type": {Const: "#microsoft.graph.win32LobAppRegistryRule"}, "ruleType": {Const: "detection"},
		"keyPath": {Type: "string", MinLength: new(uint64(1))}, "valueName": {Type: "string"},
		"check32BitOn64System": {Type: "boolean"}, "operationType": enumSchema("Registry comparison.", "exists", "doesNotExist", "string", "integer", "version"),
		"operator": operator(), "comparisonValue": {Type: "string", Description: "Version greaterThanOrEqual can detect already-newer installations at a stable vendor registry value. The key, value and comparison are explicit."},
	}, "@odata.type", "ruleType", "keyPath", "operationType", "operator")
	registry.If = &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"operationType": {Enum: []any{"exists", "doesNotExist"}}}).Properties}
	registry.Then = file.Then
	registry.Else = &jsonschema.Schema{Required: []string{"comparisonValue"}, Properties: objectSchema(map[string]*jsonschema.Schema{"operator": {Not: &jsonschema.Schema{Const: "notConfigured"}}}).Properties,
		If: &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"operationType": {Not: &jsonschema.Schema{Const: "string"}}}).Properties}, Then: &jsonschema.Schema{Properties: objectSchema(map[string]*jsonschema.Schema{"comparisonValue": {MinLength: new(uint64(1))}}).Properties}}
	script := objectSchema(map[string]*jsonschema.Schema{
		"@odata.type": {Const: "#microsoft.graph.win32LobAppPowerShellScriptRule"}, "ruleType": {Const: "detection"},
		"enforceSignatureCheck": {Type: "boolean"}, "runAs32Bit": {Type: "boolean"},
		"scriptContent": {Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(266668)), ContentEncoding: "base64", Description: "Base64 PowerShell detection script, at most 200000 decoded bytes. Detection requires exit code 0, nonempty STDOUT and empty STDERR. Runs in the app install context. This is an alternative to manual rules; preparation never executes it."},
		"operationType": {Const: "notConfigured"}, "operator": {Const: "notConfigured"},
	}, "@odata.type", "ruleType", "scriptContent")
	return &jsonschema.Schema{Type: "array", MaxItems: new(uint64(100)), Description: "Complete detection-rule collection. Selected MSI content defaults to its ProductCode and productVersion greaterThanOrEqual; a newer major version with another ProductCode needs declared file, registry or script detection. Manual rules are ANDed, with at most one MSI rule. A PowerShell rule must be the only rule. Requirement rules are unsupported.", Items: &jsonschema.Schema{OneOf: []*jsonschema.Schema{product, file, registry, script}}, Contains: product, MinContains: new(uint64(0)), MaxContains: new(uint64(1)), If: &jsonschema.Schema{Contains: script}, Then: &jsonschema.Schema{MaxItems: new(uint64(1))}}
}

// ConnectionSchema describes a shared Intune connection. App adoption belongs
// to software metadata because one connection may publish many different apps.
func ConnectionSchema() *jsonschema.Schema {
	schema := objectSchema(map[string]*jsonschema.Schema{
		"graph_url":     {Type: "string", Default: "https://graph.microsoft.com/v1.0", Description: "Graph base URL ending in /v1.0. macOS apps select /beta automatically. HTTPS is required."},
		"token":         {Type: "string", MinLength: new(uint64(1)), Description: "Existing Graph bearer token. Choose this or all three client credentials. Use ${VAR} to supply it from the environment."},
		"tenant_id":     {Type: "string", MinLength: new(uint64(1)), Description: "Microsoft Entra tenant ID. Use ${VAR} to supply it from the environment."},
		"client_id":     {Type: "string", MinLength: new(uint64(1)), Description: "App registration client ID. Use ${VAR} to supply it from the environment."},
		"client_secret": {Type: "string", MinLength: new(uint64(1)), Description: "App registration secret. Use ${VAR} to supply it from the environment."},
	})
	for _, field := range []string{"token", "client_secret"} {
		property, _ := schema.Properties.Get(field)
		property.WriteOnly = true
	}
	schema.OneOf = []*jsonschema.Schema{
		{Required: []string{"token"}, Not: &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Required: []string{"tenant_id"}}, {Required: []string{"client_id"}}, {Required: []string{"client_secret"}}}}},
		{Required: []string{"tenant_id", "client_id", "client_secret"}, Not: &jsonschema.Schema{Required: []string{"token"}}},
	}
	schema.Description = "Shared Graph connection for Windows and macOS apps. Each app is found by the identity marker in its notes; set metadata.app_id on a Software document to adopt or pin an existing app. Requires Graph DeviceManagementApps.ReadWrite.All for apply."
	return schema
}
