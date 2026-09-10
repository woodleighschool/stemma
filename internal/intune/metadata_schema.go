package intune

import (
	"slices"

	"github.com/invopop/jsonschema"
	orderedmap "github.com/pb33f/ordered-map/v2"
)

// MetadataSchema describes the supported native Graph metadata and software adoption.
// Creation requirements are checked after discovery, so existing apps can manage
// only selected fields. Omission preserves a field; collections replace membership.
func MetadataSchema() *jsonschema.Schema {
	variants := make([]*jsonschema.Schema, 0, 3)
	for _, appType := range []string{win32Type, dmgType, pkgType} {
		alias := map[string]string{win32Type: "win32", dmgType: "dmg", pkgType: "pkg"}[appType]
		p := map[string]*jsonschema.Schema{
			"unmanaged":             {Type: "array", MaxItems: new(uint64(100)), Items: &jsonschema.Schema{Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(256))}, Description: "Native field paths excluded from derivation and relinquished from previous derived ownership. Must not overlap authored native values. Removing derive relinquishes all derived ownership."},
			"type":                  {Const: alias, Description: "Short name for the native app subtype. Must agree with @odata.type if both are supplied."},
			"@odata.type":           {Const: appType, Description: "Native Graph app subtype. macOS apps use beta for current OS requirements; Win32 uses v1.0."},
			"app_id":                {Type: "string", MinLength: new(uint64(1)), Description: "Adopt this existing Intune app for this software. Must match any saved binding. Omit to discover by Stemma's stable identity marker or create an app."},
			"displayName":           {Type: "string", MaxLength: new(uint64(10000)), Description: "Company Portal display name. Required for creation."},
			"description":           {Type: "string", MaxLength: new(uint64(10000)), Description: "Native app description. Required for creation."},
			"publisher":             {Type: "string", MaxLength: new(uint64(10000)), Description: "Native publisher name. Required for creation."},
			"privacyInformationUrl": {Type: "string", MaxLength: new(uint64(10000)), Description: "Publisher privacy information URL."},
			"informationUrl":        {Type: "string", MaxLength: new(uint64(10000)), Description: "Publisher information URL."},
			"owner":                 {Type: "string", MaxLength: new(uint64(10000)), Description: "App owner label."},
			"developer":             {Type: "string", MaxLength: new(uint64(10000)), Description: "App developer label."},
			"notes":                 {Type: "string", MaxLength: new(uint64(10000)), Description: "Administrator notes. Stemma preserves a reserved identity marker alongside these notes."},
			"isFeatured":            {Type: "boolean", Description: "Show the app as featured. Explicit false is managed."},
			"assignments":           assignmentSchema(),
			"retention":             objectSchema(map[string]*jsonschema.Schema{"keep": {Type: "integer", Minimum: "1", Description: "Keep the current and N-1 most recently published distinct payloads in this app. Unknown content ownership or publication order is always protected."}}, "keep"),
		}
		if appType == win32Type {
			p["derive"] = objectSchema(map[string]*jsonschema.Schema{"msi": {Type: "string", MinLength: new(uint64(1)), Description: "Named subject supplying MSI descriptive and identity facts. Commands, install context and detection stay explicit."}}, "msi")
			p["msiInformation"] = msiSchema()
			p["dependencies"] = referencesSchema("auto_install", 99)
			p["supersedes"] = referencesSchema("uninstall_previous", 9)
			p["installCommandLine"] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: "Windows install command line. Required for creation; Stemma does not guess installer switches."}
			p["uninstallCommandLine"] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: "Windows uninstall command line. Required for creation."}
			p["minimumSupportedWindowsRelease"] = &jsonschema.Schema{Type: "string", MaxLength: new(uint64(10000)), Description: "Native minimum Windows release, such as Windows11_23H2. Required for creation."}
			p["allowedArchitectures"] = &jsonschema.Schema{Enum: []any{"x86", "x64", "arm64", nil}, Description: "Supported processor architecture; null clears it on an existing app. A non-null value is required for creation."}
			for _, key := range []string{"minimumFreeDiskSpaceInMB", "minimumMemoryInMB", "minimumNumberOfProcessors", "minimumCpuSpeedInMHz"} {
				p[key] = &jsonschema.Schema{Type: "integer", Minimum: "0", Maximum: "2147483647", Description: "Native minimum hardware requirement. Omit to leave unmanaged."}
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
	return &jsonschema.Schema{OneOf: variants, Description: "Native Intune metadata. Supports Win32 envelopes, raw macOS DMG and PKG (beta). Sources are preserved; signing and installer authoring are separate software policies. Fields not described here, including macOS scripts and managed macOSLobApp, are unsupported."}
}

func referencesSchema(flag string, limit uint64) *jsonschema.Schema {
	return &jsonschema.Schema{Type: "array", MaxItems: new(limit), Description: "Own this outgoing relationship category. Omission preserves its relationships; [] clears it. Referenced software must use the same Intune connection and be published first.", Items: objectSchema(map[string]*jsonschema.Schema{
		"software": {Type: "string", MinLength: new(uint64(1))},
		flag:       {Type: "boolean"},
	}, "software", flag)}
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
		"productCode":            {Type: "string", MinLength: new(uint64(1)), Description: "MSI ProductCode GUID."},
		"productVersionOperator": operator(), "productVersion": {Type: "string", MaxLength: new(uint64(10000)), Description: "Version used when comparison is configured."},
	}, "@odata.type", "ruleType", "productCode", "productVersionOperator")
	file := objectSchema(map[string]*jsonschema.Schema{
		"@odata.type": {Const: "#microsoft.graph.win32LobAppFileSystemRule"}, "ruleType": {Const: "detection"},
		"path": {Type: "string", MinLength: new(uint64(1))}, "fileOrFolderName": {Type: "string", MinLength: new(uint64(1))},
		"check32BitOn64System": {Type: "boolean"}, "operationType": enumSchema("Detected file property.", "exists", "version", "sizeInMB", "modifiedDate", "createdDate"),
		"operator": operator(), "comparisonValue": {Type: "string", MaxLength: new(uint64(10000)), Description: "Value used when comparison is configured."},
	}, "@odata.type", "ruleType", "path", "fileOrFolderName", "operationType", "operator")
	registry := objectSchema(map[string]*jsonschema.Schema{
		"@odata.type": {Const: "#microsoft.graph.win32LobAppRegistryRule"}, "ruleType": {Const: "detection"},
		"keyPath": {Type: "string", MinLength: new(uint64(1))}, "valueName": {Type: "string"},
		"check32BitOn64System": {Type: "boolean"}, "operationType": enumSchema("Registry comparison.", "exists", "doesNotExist", "string", "integer", "version"),
		"operator": operator(), "comparisonValue": {Type: "string"},
	}, "@odata.type", "ruleType", "keyPath", "operationType", "operator")
	script := objectSchema(map[string]*jsonschema.Schema{
		"@odata.type": {Const: "#microsoft.graph.win32LobAppPowerShellScriptRule"}, "ruleType": {Const: "detection"},
		"enforceSignatureCheck": {Type: "boolean"}, "runAs32Bit": {Type: "boolean"},
		"scriptContent": {Type: "string", MinLength: new(uint64(1)), MaxLength: new(uint64(266668)), ContentEncoding: "base64", Description: "Base64 PowerShell detection script, at most 200000 decoded bytes. Runs in the app install context."},
		"operationType": {Const: "notConfigured"}, "operator": {Const: "notConfigured"},
	}, "@odata.type", "ruleType", "scriptContent")
	return &jsonschema.Schema{Type: "array", MaxItems: new(uint64(100)), Description: "Complete detection-rule collection. Creation requires at least one rule. Product-code, file-system, registry and PowerShell detection are supported; requirement rules are unsupported.", Items: &jsonschema.Schema{OneOf: []*jsonschema.Schema{product, file, registry, script}}}
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
	schema.Description = "Shared Graph connection for Windows and macOS apps. Set metadata.app_id on a Software document to adopt an existing app. Requires Graph DeviceManagementApps.ReadWrite.All for apply."
	return schema
}
