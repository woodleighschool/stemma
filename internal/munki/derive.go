package munki

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strconv"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

// DestinationMetadata separates native pkginfo from provider-owned retention.
type DestinationMetadata struct {
	Pkginfo   json.RawMessage   `json:"pkginfo,omitempty" jsonschema_description:"Native Munki pkginfo fields. Omission preserves unowned fields; supported null values clear fields."`
	Retention *plugin.Retention `json:"retention,omitempty" jsonschema_description:"Prune older publications belonging to this resource while keeping versions still referenced by the destination."`
}

// DecodeDestination validates declared fields without requiring prepared artifacts.
func DecodeDestination(data json.RawMessage) (DestinationMetadata, error) {
	var metadata DestinationMetadata
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return metadata, fmt.Errorf("munki destination: %w", err)
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return metadata, errors.New("munki destination must be an object")
	}
	if len(metadata.Pkginfo) == 0 {
		metadata.Pkginfo = json.RawMessage(`{}`)
	}
	if _, _, err := Compose(Input{}, metadata.Pkginfo); err != nil {
		return metadata, err
	}
	if metadata.Retention != nil && metadata.Retention.Keep < 1 {
		return metadata, errors.New("retention.keep must be at least 1")
	}
	return metadata, nil
}

// DestinationSchema describes catalog metadata shared by native Munki publishers.
func DestinationSchema() *jsonschema.Schema {
	r := jsonschema.Reflector{DoNotReference: true, RequiredFromJSONSchemaTags: true}
	schema := r.Reflect(&DestinationMetadata{})
	schema.ID = ""
	pkginfo := MetadataSchema()
	// Destinations derive the minimum from the software's minimum_os, so a
	// catalog declaration can never lower the installer's requirement.
	pkginfo.Properties.Delete("minimum_os_version")
	link := r.Reflect(resourceRelationship{})
	link.ID = ""
	for _, field := range []string{"requires", "update_for"} {
		property, _ := pkginfo.Properties.Get(field)
		pkginfo.Properties.Set(field, &jsonschema.Schema{Description: property.Description, Type: "array", Items: &jsonschema.Schema{OneOf: []*jsonschema.Schema{{Type: "string"}, link}}})
	}
	schema.Properties.Set("pkginfo", pkginfo)
	retention, _ := schema.Properties.Get("retention")
	retention.Description = "Keep the current item and newest other versions. Exact version references are protected; bare references protect older items with different catalogs or installation constraints."
	return schema
}

// Derived is the pkginfo a declaration manages for one artifact. Cleared lists
// the fields derivation owns whose evidence yields no value, which a publisher
// removes so they never outlive the evidence that produced them.
type Derived struct {
	Values  map[string]any
	Origins map[string]string
	Cleared []string
}

// Derive maps prepared evidence to native fields. Declared values win; owned
// fields without evidence clear. Detection requires an installed path.
func Derive[C any](request plugin.ReconcileRequest[C]) (Derived, error) {
	metadata, err := DecodeDestination(request.Metadata)
	if err != nil {
		return Derived{}, err
	}
	var explicit map[string]any
	if err := json.Unmarshal(metadata.Pkginfo, &explicit); err != nil {
		return Derived{}, err
	}
	result := Derived{Values: map[string]any{}, Origins: map[string]string{}}
	values, origins := result.Values, result.Origins
	declared := func(key string) bool {
		_, exists := explicit[key]
		return exists
	}
	put := func(key string, value any, origin string) {
		if !declared(key) {
			values[key] = value
			origins["pkginfo."+key] = origin
		}
	}
	for key, value := range explicit {
		values[key] = value
		origins["pkginfo."+key] = "explicit"
	}
	if !request.Prepared && request.Artifact.Path == "" {
		return result, nil
	}
	icon := request.Inputs["icon"]
	if icon.Path != "" && !declared("icon_name") && !declared("icon_hash") {
		digest, err := hex.DecodeString(icon.SHA256)
		if err != nil || len(digest) != 32 || icon.Format != "png" || icon.Tree || icon.Size <= 0 || icon.Size > 32<<20 {
			return Derived{}, errors.New("icon input requires a bounded PNG artifact with a SHA-256 digest")
		}
		put("icon_name", "stemma/"+strings.ToLower(icon.SHA256)+".png", "input.icon")
		put("icon_hash", strings.ToLower(icon.SHA256), "input.icon")
	}
	put("name", request.Identity.Resource.Name, "software.name")
	if request.Artifact.Version != "" {
		put("version", request.Artifact.Version, "artifact.version")
	}
	if minimum := request.MinimumOS; minimum != nil {
		put("minimum_os_version", minimum.Version, minimum.Origin)
	} else if !declared("minimum_os_version") {
		result.Cleared = append(result.Cleared, "minimum_os_version")
	}
	kind, _ := values["installer_type"].(string)
	if kind == "nopkg" {
		if request.Artifact.Path != "" || request.Artifact.SHA256 != "" {
			return Derived{}, errors.New("nopkg must not include installer content")
		}
		return result, nil
	}
	if kind == "" {
		switch {
		case request.Artifact.Format == "pkg" || strings.HasSuffix(strings.ToLower(request.Artifact.Filename), ".pkg"):
			kind = "pkg"
		case request.Artifact.Format == "dmg" || strings.HasSuffix(strings.ToLower(request.Artifact.Filename), ".dmg"):
			kind = "copy_from_dmg"
		}
		if kind != "" {
			put("installer_type", kind, "installer.format")
		}
	}
	facts := request.Artifact.Facts
	selected, versionKey, err := macEvidence(request.Artifact)
	if err != nil {
		return Derived{}, err
	}
	var receipts []Receipt
	var installedSize int64
	var installer plugin.InstallerFacts
	for _, subject := range facts.Subjects {
		if subject.Installer != nil {
			installer = *subject.Installer
		}
		if pkg := subject.Package; pkg != nil {
			if pkg.HasPayload && pkg.Identifier != "" {
				receipts = append(receipts, Receipt{PackageID: pkg.Identifier, Version: pkg.Version, InstalledSize: pkg.InstalledSize})
				installedSize += pkg.InstalledSize
			}
		}
	}
	detects := !declared("installcheck_script") && !declared("receipts") && !declared("installs")
	if kind == "pkg" {
		if len(receipts) > 0 {
			put("receipts", receipts, "installer.receipts")
		}
		if installedSize > 0 {
			put("installed_size", installedSize, "installer.receipts")
		}
		if installer.RestartAction != "" {
			put("RestartAction", installer.RestartAction, "installer.restart_action")
		}
	}
	if selected != nil {
		app := selected.App
		version := app.Version
		if versionKey == "" {
			versionKey = app.VersionKey()
		}
		if versionKey == "CFBundleVersion" {
			version = app.Build
		}
		endpoint := selected.InstalledPath
		if kind == "copy_from_dmg" {
			if _, exists := values["items_to_copy"]; !exists {
				if selected.Path == "" || path.IsAbs(selected.Path) || !safeLocation(selected.Path) {
					return Derived{}, errors.New("selected DMG application requires a safe relative archive path")
				}
				destination := "/Applications"
				destinationItem := ""
				if endpoint != "" {
					destination = path.Dir(endpoint)
					if path.Base(endpoint) != path.Base(selected.Path) {
						destinationItem = path.Base(endpoint)
					}
				}
				put("items_to_copy", []CopyItem{{SourceItem: selected.Path, DestinationPath: destination, DestinationItem: destinationItem}}, "app.copy")
			}
			data, _ := json.Marshal(values["items_to_copy"])
			var copies []CopyItem
			if err := json.Unmarshal(data, &copies); err != nil {
				return Derived{}, err
			}
			var copyEndpoint string
			for _, action := range copies {
				if path.Clean(action.SourceItem) == path.Clean(selected.Path) {
					name := action.DestinationItem
					if name == "" {
						name = path.Base(action.SourceItem)
					}
					if copyEndpoint != "" {
						return Derived{}, errors.New("selected app has multiple copy destinations; set installs explicitly")
					}
					copyEndpoint = path.Join(action.DestinationPath, name)
				}
			}
			if endpoint != "" && copyEndpoint != "" && endpoint != copyEndpoint {
				return Derived{}, errors.New("application.installed_path disagrees with items_to_copy")
			}
			if copyEndpoint != "" {
				endpoint = copyEndpoint
			}
		}
		if detects {
			if endpoint == "" {
				return Derived{}, errors.New("selected application has no known installed path; set application.installed_path or installs")
			}
			if app.BundleID == "" || version == "" {
				return Derived{}, errors.New("selected application requires a bundle identifier and comparison version")
			}
			put("installs", []InstallItem{{Type: "application", Path: endpoint, BundleIdentifier: app.BundleID, BundleName: app.Name, BundleShortVersion: app.Version, BundleVersion: app.Build, VersionComparisonKey: versionKey, MinimumOSVersion: app.MinimumOS}}, "app.installed_path")
		}
	}
	if _, exists := values["version"]; !exists && (kind == "pkg" || kind == "copy_from_dmg") {
		return Derived{}, errors.New("the prepared installer has no managed version; select an application or set pkginfo.version")
	}
	// The method follows the installer, and an item with a method is
	// uninstallable unless the declaration says otherwise.
	method, origin := "", ""
	switch {
	case declared("uninstall_method"):
		method, _ = explicit["uninstall_method"].(string)
		origin = "pkginfo.uninstall_method"
	case kind == "pkg" && hasEntries(values["receipts"]):
		method, origin = "removepackages", "installer.receipts"
	case kind == "copy_from_dmg" && hasEntries(values["items_to_copy"]):
		method, origin = "remove_copied_items", "app.copy"
	}
	if removable, _ := explicit["uninstallable"].(bool); method != "" && (removable || !declared("uninstallable")) {
		put("uninstallable", true, origin)
		put("uninstall_method", method, origin)
	}
	if kind == "pkg" || kind == "copy_from_dmg" {
		// File installers own the same derived fields across format changes.
		for _, key := range []string{"RestartAction", "installed_size", "installs", "items_to_copy", "receipts", "uninstall_method", "uninstallable"} {
			if _, present := values[key]; !present {
				result.Cleared = append(result.Cleared, key)
			}
		}
	}
	return result, nil
}

func hasEntries(value any) bool {
	list := reflect.ValueOf(value)
	return list.Kind() == reflect.Slice && list.Len() > 0
}

// CompareVersions orders dotted versions as Munki does: numeric components
// compare as numbers, empty ones are ignored and missing ones compare as zero.
func CompareVersions(a, b string) int {
	dot := func(r rune) bool { return r == '.' }
	left, right := strings.FieldsFunc(a, dot), strings.FieldsFunc(b, dot)
	for i := range max(len(left), len(right)) {
		x, y := "0", "0"
		if i < len(left) {
			x = left[i]
		}
		if i < len(right) {
			y = right[i]
		}
		order := strings.Compare(x, y)
		m, errM := strconv.ParseUint(x, 10, 64)
		n, errN := strconv.ParseUint(y, 10, 64)
		if errM == nil && errN == nil {
			order = cmp.Compare(m, n)
		}
		if order != 0 {
			return order
		}
	}
	return 0
}
