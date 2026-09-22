package intune

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// Derive resolves artifact facts into native app metadata. Declared fields win
// over derived ones. A derivation manages every field it can supply: one the
// artifact gives no value is cleared, or must be set where Graph requires a value.
func Derive(req plugin.ReconcileRequest[Config]) (plugin.ReconcileRequest[Config], map[string]string, error) {
	m, err := decodeObject(req.Metadata)
	if err != nil {
		return req, nil, err
	}
	origins := map[string]string{}
	if value, exists := m["type"]; exists {
		types := map[string]string{"win32": win32Type, "pkg": pkgType, "dmg": dmgType}
		native, ok := types[text(value)]
		if !ok {
			return req, nil, errors.New("intune type must be win32, pkg or dmg")
		}
		if existing, ok := m["@odata.type"]; ok && existing != native {
			return req, nil, errors.New("type conflicts with @odata.type")
		}
		m["@odata.type"] = native
		delete(m, "type")
	}
	if enum(m["@odata.type"], pkgType, dmgType) {
		m, err = deriveMac(req, m, origins)
	} else {
		m, err = deriveInstaller(req, m, origins)
	}
	req.Metadata = raw(m)
	return req, origins, err
}

// deriveMac supplies the minimum OS from the software's effective requirement
// and detection from the artifact's inventory. Included apps are the
// applications a DMG holds, or those a PKG installs under /Applications,
// otherwise the receipts of its components with a payload. The selected
// application comes first.
func deriveMac(req plugin.ReconcileRequest[Config], m object, origins map[string]string) (object, error) {
	if _, declared := m["minimumSupportedOperatingSystem"]; declared {
		return nil, errors.New("minimumSupportedOperatingSystem derives from the software; set minimum_os")
	}
	if minimum := req.MinimumOS; minimum != nil {
		field, lossy, err := minimumOS(minimum.Version)
		if err != nil {
			return nil, err
		}
		origin := minimum.Origin
		if lossy {
			origin = fmt.Sprintf("%s %s -> %s", origin, minimum.Version, field)
		}
		m["minimumSupportedOperatingSystem"], origins["minimumSupportedOperatingSystem"] = object{field: true}, origin
	} else if req.Prepared || req.Artifact.Path != "" {
		return nil, errors.New("no minimum macOS is known for this software; set minimum_os")
	}
	facts := req.Artifact.Facts
	if len(facts.Subjects) == 0 {
		return m, nil
	}
	var selected *plugin.Subject
	if data := req.Artifact.Evidence["macos.application"]; len(data) > 0 {
		if err := json.Unmarshal(data, &selected); err != nil || selected == nil || selected.App == nil {
			return nil, errors.New("macos.application evidence requires an application subject")
		}
	}
	included, origin := includedApps(facts, selected, m["@odata.type"] == pkgType)
	declared, declaredApps := m["includedApps"]
	if declaredApps {
		if err := validateIncludedApps(declared); err != nil {
			return nil, err
		}
		included, origin = nil, "includedApps"
		for _, item := range declared.([]any) {
			included = append(included, item.(object))
		}
	}
	detection := object{"includedApps": nil, "primaryBundleId": nil, "primaryBundleVersion": nil}
	if len(included) > 0 {
		// The primary fields describe the first included app; a declared one names it.
		first := maps.Clone(included[0])
		for primary, field := range map[string]string{"primaryBundleId": "bundleId", "primaryBundleVersion": "bundleVersion"} {
			value, exists := m[primary]
			switch {
			case !exists:
				detection[primary] = first[field]
			case declaredApps && value != first[field]:
				return nil, fmt.Errorf("%s must agree with the first includedApps entry", primary)
			default:
				first[field] = value
			}
		}
		if text(first["bundleVersion"]) == "" {
			return nil, errors.New("the selected application has no CFBundleShortVersionString; set primaryBundleVersion")
		}
		apps := []any{first}
		for _, app := range included[1:] {
			apps = append(apps, app)
		}
		detection["includedApps"] = apps
	}
	return mergeDerived(m, detection, origin, origins)
}

// includedApps lists the applications that identify an installation, the
// selected one first, or a PKG's receipts when it installs none. Only the
// selected application may leave its version to a declared primaryBundleVersion.
func includedApps(facts plugin.Facts, selected *plugin.Subject, pkg bool) ([]object, string) {
	apps := map[string]bool{}
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			apps[subject.ID] = true
		}
	}
	eligible := func(app plugin.Subject) bool { return !pkg || strings.HasPrefix(app.InstalledPath, "/Applications/") }
	var candidates []plugin.Subject
	if selected != nil && eligible(*selected) {
		candidates = append(candidates, *selected)
	}
	for _, subject := range facts.Subjects {
		if subject.App != nil && !apps[subject.Parent] && eligible(subject) {
			candidates = append(candidates, subject)
		}
	}
	var included []object
	seen := map[string]bool{}
	for _, app := range candidates {
		id := app.App.BundleID
		if id == "" || seen[id] || app.App.Version == "" && (selected == nil || app.ID != selected.ID) {
			continue
		}
		seen[id] = true
		included = append(included, object{"bundleId": id, "bundleVersion": app.App.Version})
	}
	if len(included) > 0 || !pkg {
		return included, "installer.apps"
	}
	for _, subject := range facts.Subjects {
		if receipt := subject.Package; receipt != nil && receipt.HasPayload && receipt.Identifier != "" && receipt.Version != "" && !seen[receipt.Identifier] {
			seen[receipt.Identifier] = true
			included = append(included, object{"bundleId": receipt.Identifier, "bundleVersion": receipt.Version})
		}
	}
	return included, "installer.receipts"
}

func deriveInstaller(req plugin.ReconcileRequest[Config], metadata object, origins map[string]string) (object, error) {
	if metadata["@odata.type"] != win32Type {
		return metadata, nil
	}
	setup := req.Artifact.EntryPoint
	if setup == "" {
		setup = req.Artifact.Filename
	}
	if !strings.EqualFold(path.Ext(setup), ".msi") {
		return metadata, nil
	}
	data := req.Artifact.Evidence["windows.installer"]
	if len(data) == 0 {
		return metadata, nil
	}
	var selected *plugin.Subject
	if err := json.Unmarshal(data, &selected); err != nil || selected == nil || selected.MSI == nil {
		return nil, errors.New("selected Windows MSI evidence is invalid")
	}
	msi := selected.MSI
	defaults := msiDefaults(msi)
	if strings.ContainsAny(setup, "\"%\r\n") && metadata["installCommandLine"] == nil {
		return nil, errors.New("MSI setup path cannot be represented safely in a standard command")
	}
	defaults["installCommandLine"] = `msiexec /i "` + strings.ReplaceAll(setup, "/", `\`) + `" /qn /norestart`
	defaults["uninstallCommandLine"], defaults["rules"] = "", nil
	if msi.ProductCode != "" {
		if strings.ContainsAny(msi.ProductCode, "\"%\r\n") && metadata["uninstallCommandLine"] == nil {
			return nil, errors.New("MSI ProductCode cannot be represented safely in a standard command")
		}
		defaults["uninstallCommandLine"] = `msiexec /x "` + msi.ProductCode + `" /qn /norestart`
		if msi.ProductVersion != "" {
			defaults["rules"] = []any{object{"@odata.type": "#microsoft.graph.win32LobAppProductCodeRule", "ruleType": "detection", "productCode": msi.ProductCode, "productVersionOperator": "greaterThanOrEqual", "productVersion": msi.ProductVersion}}
		}
	}
	return mergeDerived(metadata, defaults, "windows.installer", origins)
}

// msiDefaults lists the MSI identity Graph records; an empty value is one this
// MSI does not declare. Only the UpgradeCode is optional to both.
func msiDefaults(msi *plugin.MSIFacts) object {
	var upgradeCode any = cleared{}
	if msi.UpgradeCode != "" {
		upgradeCode = msi.UpgradeCode
	}
	return object{
		"msiInformation": object{"productCode": msi.ProductCode, "productVersion": msi.ProductVersion, "upgradeCode": upgradeCode, "productName": msi.ProductName, "publisher": msi.Manufacturer},
	}
}

// minimumOS maps a macOS version to the Graph setting for its release: the
// major version, or the major and minor for 10.x. It reports whether the
// setting drops precision, as v14_0 does for 14.2.
func minimumOS(version string) (string, bool, error) {
	parts := strings.Split(version, ".")
	release := parts[:1]
	if parts[0] == "10" && len(parts) > 1 {
		release = parts[:2]
	}
	field := "v" + strings.Join(release, "_")
	if len(release) == 1 {
		field += "_0"
	}
	if !slices.Contains(minimumOSFields(), field) {
		return "", false, fmt.Errorf("macOS %s has no Intune minimum OS setting", version)
	}
	lossy := slices.ContainsFunc(parts[len(release):], func(part string) bool { return strings.Trim(part, "0") != "" })
	return field, lossy, nil
}
