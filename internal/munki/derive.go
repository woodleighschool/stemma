package munki

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

// DestinationMetadata separates native pkginfo from provider-owned derivation and retention.
type DestinationMetadata struct {
	Unmanaged []string          `json:"unmanaged,omitempty" jsonschema:"description=Native pkginfo fields whose existing values remain unmanaged and are not derived."`
	Derive    Derivation        `json:"derive,omitzero"`
	Pkginfo   json.RawMessage   `json:"pkginfo,omitempty"`
	Retention *plugin.Retention `json:"retention,omitempty"`
}

// Derivation selects observed evidence used to fill omitted native fields.
type Derivation struct {
	App *AppDerivation `json:"app,omitempty"`
}

// AppDerivation selects a named application subject and its endpoint detection path.
type AppDerivation struct {
	Subject       string `json:"subject" jsonschema:"minLength=1"`
	InstalledPath string `json:"installed_path,omitempty"`
	VersionKey    string `json:"version_key,omitempty" jsonschema:"enum=CFBundleShortVersionString,enum=CFBundleVersion"`
}

// DecodeDestination validates authored fields without requiring prepared artifacts.
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
	var explicit map[string]any
	_ = json.Unmarshal(metadata.Pkginfo, &explicit)
	seen := map[string]bool{}
	schema := MetadataSchema()
	for _, field := range metadata.Unmanaged {
		key, ok := strings.CutPrefix(field, "pkginfo.")
		if !ok {
			return metadata, errors.New("unmanaged fields must use pkginfo.<field>")
		}
		if _, exists := schema.Properties.Get(key); !exists {
			return metadata, fmt.Errorf("unsupported unmanaged field %q", field)
		}
		if key == "name" || key == "version" || key == "installer_type" {
			return metadata, fmt.Errorf("publication identity field %s cannot be unmanaged", field)
		}
		if _, exists := explicit[key]; exists {
			return metadata, fmt.Errorf("%s cannot be both authored and unmanaged", field)
		}
		if seen[field] {
			return metadata, fmt.Errorf("duplicate unmanaged field %q", field)
		}
		seen[field] = true
	}
	if metadata.Retention != nil && metadata.Retention.Keep < 1 {
		return metadata, errors.New("retention.keep must be at least 1")
	}
	if app := metadata.Derive.App; app != nil {
		if app.Subject == "" {
			return metadata, errors.New("derive.app.subject is required")
		}
		if app.InstalledPath != "" && !path.IsAbs(app.InstalledPath) {
			return metadata, errors.New("derive.app.installed_path must be absolute")
		}
		if app.VersionKey != "" && app.VersionKey != "CFBundleVersion" && app.VersionKey != "CFBundleShortVersionString" {
			return metadata, errors.New("unsupported derive.app.version_key")
		}
	}
	return metadata, nil
}

// DestinationSchema describes catalog metadata shared by native Munki publishers.
func DestinationSchema() *jsonschema.Schema {
	r := jsonschema.Reflector{DoNotReference: true, RequiredFromJSONSchemaTags: true}
	schema := r.Reflect(&DestinationMetadata{})
	schema.ID = ""
	schema.Properties.Set("pkginfo", MetadataSchema())
	return schema
}

// Derive fills omitted pkginfo from selected artifact evidence. Authored native
// fields always win; archive paths only become endpoint paths through copy actions.
func Derive(request plugin.ReconcileRequest) (map[string]any, map[string]string, error) {
	metadata, err := DecodeDestination(request.Metadata)
	if err != nil {
		return nil, nil, err
	}
	var explicit map[string]any
	if err := json.Unmarshal(metadata.Pkginfo, &explicit); err != nil {
		return nil, nil, err
	}
	values := map[string]any{}
	origins := map[string]string{}
	put := func(key string, value any, origin string) {
		if _, exists := explicit[key]; !exists && !slices.Contains(metadata.Unmanaged, "pkginfo."+key) {
			values[key] = value
			origins["pkginfo."+key] = origin
		}
	}
	for key, value := range explicit {
		values[key] = value
		origins["pkginfo."+key] = "authored"
	}
	if !request.Prepared && request.Artifact.Path == "" {
		return values, origins, nil
	}
	icon := request.Inputs["icon"]
	_, authoredName := explicit["icon_name"]
	_, authoredHash := explicit["icon_hash"]
	if icon.Path != "" && !authoredName && !authoredHash && !slices.Contains(metadata.Unmanaged, "pkginfo.icon_name") && !slices.Contains(metadata.Unmanaged, "pkginfo.icon_hash") {
		digest, err := hex.DecodeString(icon.SHA256)
		if err != nil || len(digest) != 32 || icon.Format != "png" || icon.Tree || icon.Size <= 0 || icon.Size > 32<<20 {
			return nil, nil, errors.New("icon input requires a bounded PNG artifact with a SHA-256 digest")
		}
		put("icon_name", "stemma/"+strings.ToLower(icon.SHA256)+".png", "input.icon")
		put("icon_hash", strings.ToLower(icon.SHA256), "input.icon")
	}
	put("name", request.Identity.Software, "software.name")
	if request.Artifact.Version != "" {
		put("version", request.Artifact.Version, "installer.version")
	}
	kind, _ := values["installer_type"].(string)
	if kind == "nopkg" {
		if request.Artifact.Path != "" || request.Artifact.SHA256 != "" {
			return nil, nil, errors.New("nopkg must not include installer content")
		}
		return values, origins, nil
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
	facts := request.Facts
	if len(facts.Subjects) == 0 {
		facts = request.Artifact.Facts
	}
	var selected *plugin.Subject
	appOptions := metadata.Derive.App
	evidence, versionKey, architectures, err := macEvidence(request.Artifact)
	if err != nil {
		return nil, nil, err
	}
	if len(architectures) > 0 {
		put("supported_architectures", architectures, "macos.arch")
	}
	switch {
	case appOptions != nil:
		selector, exists := request.Subjects[appOptions.Subject]
		if !exists {
			return nil, nil, fmt.Errorf("derive.app references unknown subject %q", appOptions.Subject)
		}
		subject, err := plugin.SelectSubject(facts, selector)
		if err != nil {
			return nil, nil, err
		}
		if subject.App == nil {
			return nil, nil, errors.New("derive.app requires an application subject")
		}
		selected = &subject
	case evidence != nil:
		selected = evidence
		appOptions = &AppDerivation{VersionKey: versionKey}
	default:
		for _, subject := range facts.Subjects {
			if subject.App != nil {
				if selected != nil {
					selected = nil
					break
				}
				candidate := subject
				selected = &candidate
			}
		}
	}
	var receipts []Receipt
	versions := map[string]bool{}
	for _, subject := range facts.Subjects {
		if pkg := subject.Package; pkg != nil {
			if pkg.Version != "" {
				versions[pkg.Version] = true
			}
			if pkg.HasPayload && pkg.Identifier != "" {
				receipts = append(receipts, Receipt{PackageID: pkg.Identifier, Version: pkg.Version, InstalledSize: pkg.InstalledSize})
			}
		}
	}
	if len(receipts) > 0 && kind == "pkg" {
		put("receipts", receipts, "installer.receipts")
	}
	if selected != nil {
		app := selected.App
		version := app.Version
		versionKey := "CFBundleShortVersionString"
		if appOptions != nil && appOptions.VersionKey != "" {
			versionKey = appOptions.VersionKey
		}
		if versionKey == "CFBundleVersion" {
			version = app.Build
		}
		if version != "" {
			put("version", version, "app."+versionKey)
		}
		endpoint := selected.InstalledPath
		if appOptions != nil && appOptions.InstalledPath != "" {
			endpoint = appOptions.InstalledPath
		}
		if kind == "copy_from_dmg" {
			if _, exists := values["items_to_copy"]; !exists {
				if selected.Path == "" || path.IsAbs(selected.Path) || !safeLocation(selected.Path) {
					return nil, nil, errors.New("selected DMG application requires a safe relative archive path")
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
				return nil, nil, err
			}
			var copyEndpoint string
			for _, action := range copies {
				if path.Clean(action.SourceItem) == path.Clean(selected.Path) {
					name := action.DestinationItem
					if name == "" {
						name = path.Base(action.SourceItem)
					}
					if copyEndpoint != "" {
						return nil, nil, errors.New("selected app has multiple copy destinations; author installs explicitly")
					}
					copyEndpoint = path.Join(action.DestinationPath, name)
				}
			}
			if endpoint != "" && copyEndpoint != "" && endpoint != copyEndpoint {
				return nil, nil, errors.New("derive.app.installed_path disagrees with items_to_copy")
			}
			if copyEndpoint != "" {
				endpoint = copyEndpoint
			}
		}
		_, script := explicit["installcheck_script"]
		_, authoredReceipts := explicit["receipts"]
		_, authoredInstalls := explicit["installs"]
		authoredInstalls = authoredInstalls || slices.Contains(metadata.Unmanaged, "pkginfo.installs")
		if !script && !authoredReceipts && !authoredInstalls {
			if endpoint == "" {
				if appOptions != nil || kind == "copy_from_dmg" {
					return nil, nil, errors.New("selected application has no known installed path; author derive.app.installed_path or installs")
				}
			} else {
				if app.BundleID == "" || version == "" {
					return nil, nil, errors.New("selected application requires a bundle identifier and comparison version")
				}
				put("installs", []InstallItem{{Type: "application", Path: endpoint, BundleIdentifier: app.BundleID, BundleName: app.Name, BundleShortVersion: app.Version, BundleVersion: app.Build, VersionComparisonKey: versionKey, MinimumOSVersion: app.MinimumOS}}, "app.installed_path")
			}
		}
		if app.MinimumOS != "" {
			put("minimum_os_version", app.MinimumOS, "app.minimum_os")
		}
	} else if appOptions == nil {
		if len(versions) == 1 {
			for version := range versions {
				put("version", version, "installer.receipts")
			}
		}
	}
	return values, origins, nil
}

// ArtifactDefaults supplies conservative native defaults for explicit pkginfo
// rendering operations. Destination publication uses Derive and named subjects.
func ArtifactDefaults(facts plugin.Facts, explicit map[string]any) map[string]any {
	result := map[string]any{}
	versions := map[string]bool{}
	var receipts []Receipt
	var apps []plugin.Subject
	for _, subject := range facts.Subjects {
		if pkg := subject.Package; pkg != nil {
			if pkg.Version != "" {
				versions[pkg.Version] = true
			}
			if pkg.HasPayload && pkg.Identifier != "" {
				receipts = append(receipts, Receipt{PackageID: pkg.Identifier, Version: pkg.Version, InstalledSize: pkg.InstalledSize})
			}
		}
		if subject.App != nil {
			apps = append(apps, subject)
			if subject.App.Version != "" {
				versions[subject.App.Version] = true
			}
		}
	}
	if len(versions) == 1 {
		for version := range versions {
			result["version"] = version
		}
	}
	if len(receipts) > 0 {
		result["receipts"] = receipts
	}
	_, script := explicit["installcheck_script"]
	_, installs := explicit["installs"]
	_, authoredReceipts := explicit["receipts"]
	if len(apps) == 1 && apps[0].InstalledPath != "" && !script && !installs && !authoredReceipts {
		app := apps[0]
		result["installs"] = []InstallItem{{Type: "application", Path: app.InstalledPath, BundleIdentifier: app.App.BundleID, BundleShortVersion: app.App.Version, BundleVersion: app.App.Build}}
	}
	return result
}
