// Package munki builds native pkginfo for an explicitly supported writable subset.
package munki

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"reflect"
	"strings"
)

// Input binds package metadata to the exact installer bytes being published.
// InstallerType is pkg, copy_from_dmg, or nopkg. Size is bytes, not Munki KiB.
type Input struct {
	Name              string
	Version           string
	InstallerType     string
	InstallerLocation string
	SHA256            string
	Size              int64
	Metadata          json.RawMessage
}

// Metadata contains supported Munki-native fields. Fields retains presence,
// including explicit null, alongside the typed values. Null is accepted only for
// nullable display strings; lists use [] and booleans use false to clear values.
type Metadata struct {
	DisplayName            *string                    `json:"display_name,omitempty" jsonschema:"description=Optional display title. Omission preserves the remote title; null clears it."`
	Description            *string                    `json:"description,omitempty" jsonschema:"description=Application description. Omission preserves the remote value; null clears it."`
	Category               *string                    `json:"category,omitempty" jsonschema:"description=Managed Software Center category. Null clears the category."`
	Developer              *string                    `json:"developer,omitempty" jsonschema:"description=Software publisher display name. Null clears the publisher."`
	Catalogs               *[]string                  `json:"catalogs,omitempty" jsonschema:"description=Complete catalog membership. An empty list removes all memberships."`
	Requires               *[]string                  `json:"requires,omitempty" jsonschema_description:"Complete list of Munki item dependencies. Entries are native item names, optionally with version requirements; [] clears the list."`
	UnattendedInstall      *bool                      `json:"unattended_install,omitempty" jsonschema:"description=Permit installation without interaction. Explicit false disables it."`
	UnattendedUninstall    *bool                      `json:"unattended_uninstall,omitempty" jsonschema:"description=Permit removal without interaction. Explicit false disables it."`
	Uninstallable          *bool                      `json:"uninstallable,omitempty" jsonschema:"description=Expose supported removal using uninstall_method."`
	UninstallMethod        *string                    `json:"uninstall_method,omitempty" jsonschema:"description=Supported native removal method; requires its corresponding receipts or copy entries or script."`
	RestartAction          *string                    `json:"RestartAction,omitempty" jsonschema:"description=Munki restart or logout requirement after installation."`
	MinimumOSVersion       *string                    `json:"minimum_os_version,omitempty" jsonschema:"description=Lowest macOS version eligible for this package."`
	MaximumOSVersion       *string                    `json:"maximum_os_version,omitempty" jsonschema:"description=Highest macOS version eligible for this package."`
	MinimumMunkiVersion    *string                    `json:"minimum_munki_version,omitempty" jsonschema:"description=Lowest Munki client version eligible for this package."`
	SupportedArchitectures *[]string                  `json:"supported_architectures,omitempty" jsonschema:"description=Complete architecture list using arm64 or x86_64."`
	BlockingApplications   *[]string                  `json:"blocking_applications,omitempty" jsonschema:"description=Applications that block installation. Empty list disables inferred blocking."`
	InstallableCondition   *string                    `json:"installable_condition,omitempty" jsonschema:"description=Munki predicate controlling installation eligibility."`
	InstalledSize          *int64                     `json:"installed_size,omitempty" jsonschema:"description=Installed size in KiB; independent of downloaded installer size."`
	OnDemand               *bool                      `json:"OnDemand,omitempty" jsonschema:"description=Allow repeated optional installation even when the item is already installed."`
	Precache               *bool                      `json:"precache,omitempty" jsonschema:"description=Cache the installer before it is requested for installation."`
	Autoremove             *bool                      `json:"autoremove,omitempty" jsonschema:"description=Remove installed software when no applicable manifest requests it."`
	Installs               *[]InstallItem             `json:"installs,omitempty" jsonschema:"description=Complete native detection list using application or bundle or plist or file entries."`
	Receipts               *[]Receipt                 `json:"receipts,omitempty" jsonschema:"description=Complete Apple package receipt list used for detection and removal."`
	ItemsToCopy            *[]CopyItem                `json:"items_to_copy,omitempty" jsonschema:"description=Payload selections and destination paths for copy_from_dmg."`
	PackagePath            *string                    `json:"package_path,omitempty" jsonschema:"description=Relative package path within the original installer image."`
	Notes                  *string                    `json:"notes,omitempty" jsonschema:"description=Administrator notes retained in pkginfo."`
	InstallcheckScript     *string                    `json:"installcheck_script,omitempty" jsonschema:"description=Deployment-time installation check script. Never executed by Stemma."`
	UninstallcheckScript   *string                    `json:"uninstallcheck_script,omitempty" jsonschema:"description=Deployment-time removal check script. Never executed by Stemma."`
	PreinstallScript       *string                    `json:"preinstall_script,omitempty" jsonschema:"description=Deployment-time script before installation. Never executed by Stemma."`
	PostinstallScript      *string                    `json:"postinstall_script,omitempty" jsonschema:"description=Deployment-time script after installation. Never executed by Stemma."`
	PreuninstallScript     *string                    `json:"preuninstall_script,omitempty" jsonschema:"description=Deployment-time script before removal. Never executed by Stemma."`
	PostuninstallScript    *string                    `json:"postuninstall_script,omitempty" jsonschema:"description=Deployment-time script after removal. Never executed by Stemma."`
	UninstallScript        *string                    `json:"uninstall_script,omitempty" jsonschema:"description=Deployment-time removal script. Never executed by Stemma."`
	VersionScript          *string                    `json:"version_script,omitempty" jsonschema:"description=Deployment-time installed-version script. Never executed by Stemma."`
	Fields                 map[string]json.RawMessage `json:"-"`
}

// InstallItem is a native Munki application, bundle, plist or file matcher.
type InstallItem struct {
	Type                  string `json:"type" plist:"type"`
	Path                  string `json:"path" plist:"path"`
	BundleIdentifier      string `json:"CFBundleIdentifier,omitempty" plist:"CFBundleIdentifier,omitempty"`
	BundleName            string `json:"CFBundleName,omitempty" plist:"CFBundleName,omitempty"`
	BundleShortVersion    string `json:"CFBundleShortVersionString,omitempty" plist:"CFBundleShortVersionString,omitempty"`
	BundleVersion         string `json:"CFBundleVersion,omitempty" plist:"CFBundleVersion,omitempty"`
	VersionComparisonKey  string `json:"version_comparison_key,omitempty" plist:"version_comparison_key,omitempty"`
	MinimumUpdateVersion  string `json:"minimum_update_version,omitempty" plist:"minimum_update_version,omitempty"`
	InstallerItemLocation string `json:"installer_item_location,omitempty" plist:"installer_item_location,omitempty"`
	MinimumOSVersion      string `json:"minimum_os_version,omitempty" plist:"minimum_os_version,omitempty"`
	MD5Checksum           string `json:"md5checksum,omitempty" plist:"md5checksum,omitempty"`
}

// Receipt identifies an installed Apple package receipt.
type Receipt struct {
	PackageID     string `json:"packageid" plist:"packageid"`
	Name          string `json:"name,omitempty" plist:"name,omitempty"`
	Version       string `json:"version,omitempty" plist:"version,omitempty"`
	InstalledSize int64  `json:"installed_size,omitempty" plist:"installed_size,omitempty"`
	Optional      bool   `json:"optional,omitempty" plist:"optional,omitempty"`
}

// CopyItem selects a payload inside a disk image and its target location.
type CopyItem struct {
	SourceItem      string `json:"source_item" plist:"source_item"`
	DestinationPath string `json:"destination_path" plist:"destination_path"`
	DestinationItem string `json:"destination_item,omitempty" plist:"destination_item,omitempty"`
	User            string `json:"user,omitempty" plist:"user,omitempty"`
	Group           string `json:"group,omitempty" plist:"group,omitempty"`
	Mode            string `json:"mode,omitempty" plist:"mode,omitempty"`
}

// DecodeMetadata rejects unsupported keys, invalid types and unsupported clears.
func DecodeMetadata(raw json.RawMessage) (Metadata, error) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{")) {
		return Metadata{}, errors.New("munki metadata must be an object")
	}
	var metadata Metadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&metadata); err != nil {
		return Metadata{}, fmt.Errorf("munki metadata: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Metadata{}, errors.New("munki metadata must contain one object")
	}
	if err := json.Unmarshal(raw, &metadata.Fields); err != nil {
		return Metadata{}, err
	}
	supported := map[string]bool{}
	shape := reflect.TypeOf(metadata)
	for field := range shape.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name != "-" {
			supported[name] = true
		}
	}
	for key, value := range metadata.Fields {
		if !supported[key] {
			return Metadata{}, fmt.Errorf("unsupported Munki field %q", key)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			switch key {
			case "display_name", "description", "category", "developer":
			default:
				return Metadata{}, fmt.Errorf("munki field %q does not support null", key)
			}
		}
	}
	if metadata.InstalledSize != nil && *metadata.InstalledSize < 0 {
		return Metadata{}, errors.New("installed_size must not be negative")
	}
	if metadata.RestartAction != nil {
		switch *metadata.RestartAction {
		case "", "RequireLogout", "RecommendRestart", "RequireRestart", "RequireShutdown":
		default:
			return Metadata{}, errors.New("unsupported RestartAction")
		}
	}
	if metadata.UninstallMethod != nil {
		switch *metadata.UninstallMethod {
		case "", "removepackages", "remove_copied_items", "uninstall_script":
		default:
			return Metadata{}, errors.New("unsupported uninstall_method")
		}
	}
	if metadata.SupportedArchitectures != nil {
		for _, architecture := range *metadata.SupportedArchitectures {
			if architecture != "arm64" && architecture != "x86_64" {
				return Metadata{}, errors.New("unsupported Munki architecture")
			}
		}
	}
	if metadata.Installs != nil {
		for _, item := range *metadata.Installs {
			if strings.TrimSpace(item.Path) == "" {
				return Metadata{}, errors.New("installs requires a path")
			}
			switch item.Type {
			case "application", "bundle", "plist", "file":
			default:
				return Metadata{}, errors.New("unsupported installs item type")
			}
		}
	}
	if metadata.Receipts != nil {
		for _, receipt := range *metadata.Receipts {
			if strings.TrimSpace(receipt.PackageID) == "" || receipt.InstalledSize < 0 {
				return Metadata{}, errors.New("invalid receipt")
			}
		}
	}
	if metadata.ItemsToCopy != nil {
		for _, item := range *metadata.ItemsToCopy {
			if item.SourceItem == "" || path.IsAbs(item.SourceItem) || strings.Contains(item.SourceItem, "\\") || path.Clean(item.SourceItem) == ".." || strings.HasPrefix(path.Clean(item.SourceItem), "../") || !path.IsAbs(item.DestinationPath) {
				return Metadata{}, errors.New("items_to_copy requires a relative source and absolute destination")
			}
		}
	}
	return metadata, nil
}

// Build emits XML pkginfo with server-independent native Munki keys and exact
// content identity. It never inspects or executes the referenced installer.
func Build(input Input) ([]byte, error) {
	if strings.TrimSpace(input.Name) == "" || strings.Contains(input.Name, "/") || strings.TrimSpace(input.Version) == "" {
		return nil, errors.New("munki name and version are required; name must not contain a slash")
	}
	metadata, err := DecodeMetadata(input.Metadata)
	if err != nil {
		return nil, err
	}
	values := metadata.Values()
	values["name"], values["version"] = input.Name, input.Version
	switch input.InstallerType {
	case "pkg", "copy_from_dmg":
		if input.Size < 0 || input.InstallerLocation == "" || !safeLocation(input.InstallerLocation) {
			return nil, errors.New("installer requires a nonnegative size and safe relative location")
		}
		digest, err := hex.DecodeString(input.SHA256)
		if err != nil || len(digest) != 32 {
			return nil, errors.New("installer SHA256 must contain 64 hexadecimal characters")
		}
		values["installer_item_location"] = input.InstallerLocation
		values["installer_item_hash"] = strings.ToLower(input.SHA256)
		values["installer_item_size"] = input.Size/1024 + boolInt(input.Size%1024 != 0)
		if input.InstallerType == "copy_from_dmg" {
			if metadata.ItemsToCopy == nil || len(*metadata.ItemsToCopy) == 0 {
				return nil, errors.New("copy_from_dmg requires items_to_copy")
			}
			values["installer_type"] = input.InstallerType
		}
	case "nopkg":
		if input.InstallerLocation != "" || input.SHA256 != "" || input.Size != 0 {
			return nil, errors.New("nopkg must not include installer content")
		}
		values["installer_type"] = "nopkg"
	default:
		return nil, fmt.Errorf("unsupported installer_type %q", input.InstallerType)
	}
	if metadata.Uninstallable != nil && *metadata.Uninstallable {
		if metadata.UninstallMethod == nil || *metadata.UninstallMethod == "" {
			return nil, errors.New("uninstallable requires uninstall_method")
		}
		switch *metadata.UninstallMethod {
		case "removepackages":
			if metadata.Receipts == nil || len(*metadata.Receipts) == 0 {
				return nil, errors.New("removepackages requires receipts")
			}
		case "remove_copied_items":
			if metadata.ItemsToCopy == nil || len(*metadata.ItemsToCopy) == 0 {
				return nil, errors.New("remove_copied_items requires items_to_copy")
			}
		case "uninstall_script":
			if metadata.UninstallScript == nil || strings.TrimSpace(*metadata.UninstallScript) == "" {
				return nil, errors.New("uninstall_script requires script content")
			}
		}
	}
	if metadata.UninstallMethod != nil && *metadata.UninstallMethod == "remove_copied_items" && metadata.ItemsToCopy != nil {
		remove := make([]map[string]string, 0, len(*metadata.ItemsToCopy))
		for _, item := range *metadata.ItemsToCopy {
			name := item.DestinationItem
			if name == "" {
				name = path.Base(item.SourceItem)
			}
			remove = append(remove, map[string]string{"path": path.Join(item.DestinationPath, name)})
		}
		values["items_to_remove"] = remove
	}
	encoded, err := Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode Munki pkginfo: %w", err)
	}
	return encoded, nil
}

// Values returns typed, concrete native fields. Null display metadata is omitted
// from pkginfo; Fields remains available to adapters that must clear remote data.
func (metadata Metadata) Values() map[string]any {
	values := make(map[string]any)
	typed := reflect.ValueOf(metadata)
	for i := range typed.NumField() {
		field := typed.Field(i)
		if field.Kind() != reflect.Pointer || field.IsNil() {
			continue
		}
		name, _, _ := strings.Cut(typed.Type().Field(i).Tag.Get("json"), ",")
		values[name] = field.Elem().Interface()
	}
	return values
}

func safeLocation(location string) bool {
	return !path.IsAbs(location) && !strings.Contains(location, "\\") && path.Clean(location) != ".." && !strings.HasPrefix(path.Clean(location), "../") && !strings.Contains(location, ":")
}

func boolInt(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
