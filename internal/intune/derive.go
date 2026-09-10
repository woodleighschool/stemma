package intune

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// Derive resolves explicitly selected artifact facts into native app metadata.
// Authored fields win; installer execution and Windows detection remain explicit.
func Derive(req plugin.ReconcileRequest) (plugin.ReconcileRequest, map[string]string, error) {
	m, err := decodeObject(req.Metadata)
	if err != nil {
		return req, nil, err
	}
	unmanaged, err := unmanagedFields(m)
	if err != nil {
		return req, nil, err
	}
	delete(m, "unmanaged")
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
	derive, exists := m["derive"]
	if !exists {
		req.Metadata = raw(m)
		return req, origins, nil
	}
	d, ok := derive.(object)
	if !ok || len(d) != 1 {
		return req, nil, errors.New("derive must select exactly one msi or app subject")
	}
	if err := fields(d, "msi", "app"); err != nil {
		return req, nil, err
	}
	for kind, value := range d {
		name := text(value)
		selector, exists := req.Subjects[name]
		if name == "" || !exists {
			return req, nil, fmt.Errorf("derive.%s requires a named software subject", kind)
		}
		if kind == "msi" {
			if t, present := m["@odata.type"]; present && t != win32Type {
				return req, nil, errors.New("derive.msi requires a Win32 app")
			}
			if _, present := m["@odata.type"]; !present {
				origins["@odata.type"] = "derive.msi:" + name
			}
			m["@odata.type"] = win32Type
		}
		if req.Method == "validate" && !req.Prepared && req.Artifact.Path == "" {
			continue
		}
		subject, err := plugin.SelectSubject(req.Facts, selector)
		if err != nil {
			return req, nil, fmt.Errorf("derive.%s: %w", kind, err)
		}
		defaults := object{}
		if kind == "msi" {
			if subject.MSI == nil {
				return req, nil, errors.New("derive.msi selected a subject without MSI facts")
			}
			msi := subject.MSI
			info := object{}
			for key, value := range map[string]string{"productCode": msi.ProductCode, "productVersion": msi.ProductVersion, "upgradeCode": msi.UpgradeCode, "productName": msi.ProductName, "publisher": msi.Manufacturer} {
				if value != "" {
					info[key] = value
				}
			}
			defaults["msiInformation"] = info
			if msi.ProductName != "" {
				defaults["displayName"] = msi.ProductName
			}
			if msi.Manufacturer != "" {
				defaults["publisher"] = msi.Manufacturer
			}
		} else {
			if subject.App == nil || !enum(m["@odata.type"], pkgType, dmgType) {
				return req, nil, errors.New("derive.app requires application facts and a macOS PKG or DMG app")
			}
			app := subject.App
			if app.Name != "" {
				defaults["displayName"] = app.Name
			}
			id, version := app.BundleID, app.Version
			if value, exists := m["primaryBundleId"]; exists {
				id = text(value)
			}
			if value, exists := m["primaryBundleVersion"]; exists {
				version = text(value)
			}
			if included, exists := m["includedApps"]; exists {
				if err := validateIncludedApps(included); err != nil {
					return req, nil, err
				}
				first := included.([]any)[0].(object)
				for primary, field := range map[string]string{"primaryBundleId": "bundleId", "primaryBundleVersion": "bundleVersion"} {
					if value, exists := m[primary]; exists && value != first[field] {
						return req, nil, fmt.Errorf("%s must agree with the first includedApps entry", primary)
					}
				}
				id, version = text(first["bundleId"]), text(first["bundleVersion"])
			}
			if id != "" {
				defaults["primaryBundleId"] = id
			}
			if version != "" {
				defaults["primaryBundleVersion"] = version
			}
			if id != "" && version != "" {
				defaults["includedApps"] = []any{object{"bundleId": id, "bundleVersion": version}}
			}
			if _, authored := m["minimumSupportedOperatingSystem"]; !authored && app.MinimumOS != "" && !suppressedField("minimumSupportedOperatingSystem", unmanaged) {
				minimum, err := minimumOS(app.MinimumOS)
				if err != nil {
					return req, nil, err
				}
				defaults["minimumSupportedOperatingSystem"] = minimum
			}
		}
		m = mergeDerived(m, defaults, "", "derive."+kind+":"+name, unmanaged, origins)
	}
	delete(m, "derive")
	req.Metadata = raw(m)
	return req, origins, nil
}

func minimumOS(version string) (object, error) {
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
	field := "v" + strings.Join(parts, "_")
	if slices.Contains(minimumOSFields(), field) {
		return object{field: true}, nil
	}
	return nil, fmt.Errorf("minimum macOS %q has no exact supported Intune setting; author minimumSupportedOperatingSystem explicitly", version)
}
