package intune

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// Derive compiles the declaration into Graph metadata and fills what the
// artifact supplies. Declared fields win over derived ones. A derivation
// manages every field it can supply: one the artifact gives no value is
// cleared, or must be set where Graph requires a value. Origins use the
// declaration's field names.
func Derive(req plugin.ReconcileRequest[Config]) (plugin.ReconcileRequest[Config], map[string]string, error) {
	m, err := compile(req)
	if err != nil {
		return req, nil, err
	}
	origins := map[string]string{}
	if m["@odata.type"] == win32Type {
		m, err = deriveInstaller(req, m, origins)
	} else {
		m, err = deriveMac(req, m, origins)
	}
	if err != nil {
		return req, nil, err
	}
	req.Metadata = raw(m)
	reported := make(map[string]string, len(origins))
	for property, origin := range origins {
		reported[reportName(property)] = origin
	}
	return req, reported, nil
}

// deriveMac supplies the minimum OS from the software's effective requirement
// and detection from its applications or PKG receipts.
func deriveMac(req plugin.ReconcileRequest[Config], m object, origins map[string]string) (object, error) {
	lob := m["@odata.type"] == lobType
	if lob {
		if err := validateLOB(req.Artifact, m["installAsManaged"] == true); err != nil {
			return nil, err
		}
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
	if declared, exists := m["includedApps"]; exists {
		first := declared.([]any)[0].(object)
		return mergeDerived(m, object{"primaryBundleId": first["bundleId"], "primaryBundleVersion": first["bundleVersion"]}, "included_apps", origins)
	}
	if declared, exists := m["childApps"]; exists {
		first := declared.([]any)[0].(object)
		return mergeDerived(m, object{"bundleId": first["bundleId"], "buildNumber": first["buildNumber"], "versionNumber": first["versionNumber"]}, "included_apps", origins)
	}
	if !req.Prepared && req.Artifact.Path == "" && len(req.Artifact.Facts.Subjects) == 0 {
		return m, nil
	}
	var selected *plugin.Subject
	if data := req.Artifact.Evidence["macos.application"]; len(data) > 0 {
		if err := json.Unmarshal(data, &selected); err != nil || selected == nil || selected.App == nil {
			return nil, errors.New("macos.application evidence requires an application subject")
		}
	}
	apps, origin := detectedApps(req.Artifact.Facts, selected, text(m["@odata.type"]))
	if len(apps) == 0 {
		return nil, errors.New("the artifact has no application or package receipt to detect it; set included_apps")
	}
	// The primary fields describe the first included app.
	first := apps[0]
	if first.version == "" {
		return nil, errors.New("the selected application has no CFBundleShortVersionString; set included_apps")
	}
	list := make([]any, len(apps))
	for i, app := range apps {
		if lob {
			list[i] = object{"bundleId": app.id, "buildNumber": app.version, "versionNumber": app.build}
		} else {
			list[i] = object{"bundleId": app.id, "bundleVersion": app.version}
		}
	}
	detection := object{"includedApps": list, "primaryBundleId": first.id, "primaryBundleVersion": first.version}
	if lob {
		detection = object{"childApps": list, "bundleId": first.id, "buildNumber": first.version, "versionNumber": first.build}
	}
	return mergeDerived(m, detection, origin, origins)
}

type detectedApp struct {
	id, version, build string
}

// detectedApps lists the applications that identify an installation, the
// selected one first, or the receipts of a PKG without applications.
// Only the selected application may lack a version, which fails its detection.
func detectedApps(facts plugin.Facts, selected *plugin.Subject, appType string) ([]detectedApp, string) {
	apps := map[string]bool{}
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			apps[subject.ID] = true
		}
	}
	eligible := func(app plugin.Subject) bool {
		return appType != lobType || strings.HasPrefix(app.InstalledPath, "/Applications/")
	}
	var candidates []plugin.Subject
	if selected != nil && eligible(*selected) {
		candidates = append(candidates, *selected)
	}
	for _, subject := range facts.Subjects {
		if subject.App != nil && !apps[subject.Parent] && eligible(subject) {
			candidates = append(candidates, subject)
		}
	}
	var detected []detectedApp
	seen := map[string]bool{}
	for _, app := range candidates {
		id := app.App.BundleID
		if id == "" || seen[id] || app.App.Version == "" && (selected == nil || app.ID != selected.ID) {
			continue
		}
		seen[id] = true
		build := cmp.Or(app.App.Build, app.App.Version)
		detected = append(detected, detectedApp{id: id, version: app.App.Version, build: build})
	}
	if len(detected) > 0 || appType != pkgType {
		return detected, "installer.apps"
	}
	for _, subject := range facts.Subjects {
		if receipt := subject.Package; receipt != nil && receipt.Identifier != "" && receipt.Version != "" && !seen[receipt.Identifier] {
			seen[receipt.Identifier] = true
			detected = append(detected, detectedApp{id: receipt.Identifier, version: receipt.Version, build: receipt.Version})
		}
	}
	return detected, "installer.receipts"
}

// lobLimit is Intune's size limit for a line-of-business PKG.
const lobLimit = 2 << 30

// validateLOB checks what Intune requires of a line-of-business upload that
// the prepared artifact can show: a flat PKG with a payload, a verified
// Developer ID Installer signature and a bounded size. Installing as managed
// needs one component that installs one application under /Applications.
// Validation without an artifact checks nothing here.
func validateLOB(artifact plugin.Artifact, managed bool) error {
	if artifact.Path == "" {
		return nil
	}
	if artifact.Tree || !strings.EqualFold(path.Ext(artifact.Filename), ".pkg") {
		return errors.New("a line-of-business app requires a flat PKG")
	}
	if artifact.Size > lobLimit {
		return errors.New("a line-of-business PKG must be at most 2 GiB")
	}
	if len(artifact.Evidence["signature"]) == 0 {
		return errors.New("a line-of-business app requires a verified Developer ID Installer signature; set signature.signer")
	}
	components, payload := 0, false
	apps := map[string]bool{}
	var installed []plugin.Subject
	for _, subject := range artifact.Facts.Subjects {
		if subject.Package != nil {
			components++
			payload = payload || subject.Package.HasPayload
		}
		if subject.App != nil {
			apps[subject.ID] = true
		}
	}
	if !payload {
		return errors.New("a line-of-business PKG requires a payload")
	}
	for _, subject := range artifact.Facts.Subjects {
		if subject.App != nil && !apps[subject.Parent] {
			installed = append(installed, subject)
		}
	}
	if managed && (components != 1 || len(installed) != 1 || !strings.HasPrefix(installed[0].InstalledPath, "/Applications/")) {
		return errors.New("install_as_managed requires a PKG with one component that installs one application under /Applications")
	}
	return nil
}

// deriveInstaller supplies the standard msiexec commands, MSI information and
// ProductCode detection of a selected setup MSI. Declared MSI properties
// extend the derived install command, and declared version comparisons
// without a value compare with the managed version.
func deriveInstaller(req plugin.ReconcileRequest[Config], metadata object, origins map[string]string) (object, error) {
	if metadata["@odata.type"] != win32Type {
		return metadata, nil
	}
	properties, _ := metadata["msi_properties"].(object)
	delete(metadata, "msi_properties")
	if err := deriveVersionRules(req, metadata, origins); err != nil {
		return nil, err
	}
	setup := req.Artifact.EntryPoint
	if setup == "" {
		setup = req.Artifact.Filename
	}
	data := req.Artifact.Evidence["windows.installer"]
	if !strings.EqualFold(path.Ext(setup), ".msi") || len(data) == 0 {
		if properties != nil && req.Artifact.Path != "" {
			return nil, errors.New("msi_properties requires an MSI setup file")
		}
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
	defaults["installCommandLine"] = `msiexec /i "` + strings.ReplaceAll(setup, "/", `\`) + `" /qn /norestart` + msiArguments(properties)
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

// windowsVersion matches the dotted version a file or registry rule compares.
var windowsVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}$`)

// deriveVersionRules sets the artifact's managed version as the value of each
// declared version comparison that omits one. Detection itself is declared, so
// each derived value reports its own origin.
func deriveVersionRules(req plugin.ReconcileRequest[Config], metadata object, origins map[string]string) error {
	rules, _ := metadata["rules"].([]any)
	for i, item := range rules {
		rule := item.(object)
		if _, compared := rule["comparisonValue"]; compared || rule["operationType"] != "version" {
			continue
		}
		version := req.Artifact.Version
		switch {
		case version == "" && !req.Prepared && req.Artifact.Path == "":
			continue
		case version == "":
			return errors.New("the artifact has no managed version to detect; set the version rule's value")
		case !windowsVersion.MatchString(version):
			return fmt.Errorf("managed version %q is not a Windows version; set the version rule's value", version)
		}
		rule["comparisonValue"] = version
		origins[fmt.Sprintf("rules.%d.value", i)] = "artifact.version"
	}
	return nil
}

// msiArguments renders public properties for msiexec in name order. Values
// are quoted, and a quote inside a value is doubled as Windows Installer reads
// it.
func msiArguments(properties object) string {
	var arguments strings.Builder
	for _, name := range slices.Sorted(maps.Keys(properties)) {
		value := strings.ReplaceAll(text(properties[name]), `"`, `""`)
		arguments.WriteString(" " + name + `="` + value + `"`)
	}
	return arguments.String()
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
