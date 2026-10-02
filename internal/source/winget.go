package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

type wingetConfig struct {
	Package       string `json:"package" jsonschema_description:"Exact WinGet community package identifier."`
	Version       string `json:"version,omitempty" jsonschema_description:"Exact published version or latest (the default). Selects the release before filtering installers."`
	Architecture  string `json:"architecture,omitempty" jsonschema:"enum=x64,enum=x86,enum=arm64,enum=arm,enum=neutral" jsonschema_description:"Exact target architecture. Defaults to x64 independently of the runner."`
	Scope         string `json:"scope,omitempty" jsonschema:"enum=user,enum=machine" jsonschema_description:"Optional declared installer scope filter. Omit only when selection is unambiguous."`
	InstallerType string `json:"installer_type,omitempty" jsonschema_description:"Optional declared installer type filter, such as msi, wix, inno or zip."`
	Locale        string `json:"locale,omitempty" jsonschema_description:"Optional exact installer locale filter, such as en-US. Package description locale is not an installer locale."`
}

var wingetIdentifier = regexp.MustCompile(`^[^.\s\\/:*?"<>|\x00-\x1f]{1,32}(\.[^.\s\\/:*?"<>|\x00-\x1f]{1,32}){1,7}$`)
var wingetLocale = regexp.MustCompile(`^([a-zA-Z]{2,3}|[iI]-[a-zA-Z]+|[xX]-[a-zA-Z]{1,8})(-[a-zA-Z]{1,8})*$`)
var wingetTypes = []string{"msix", "msi", "appx", "exe", "zip", "inno", "nullsoft", "wix", "burn", "pwa", "portable", "font", "msstore"}

func wingetInput(input plugin.Input) (wingetConfig, error) {
	var config wingetConfig
	data, err := json.Marshal(input.Config)
	if err == nil {
		err = decode(data, &config)
	}
	if err != nil {
		return config, fmt.Errorf("winget config: %w", err)
	}
	if !wingetIdentifier.MatchString(config.Package) || utf8.RuneCountInString(config.Package) > 128 || strings.IndexFunc(config.Package, unicode.IsSpace) >= 0 {
		return config, errors.New("winget requires a valid package identifier")
	}
	if config.Version == "" {
		config.Version = "latest"
	}
	if utf8.RuneCountInString(config.Version) > 128 || strings.ContainsAny(config.Version, `\/:*?"<>|`) || strings.IndexFunc(config.Version, unicode.IsControl) >= 0 || strings.TrimSpace(config.Version) != config.Version {
		return config, errors.New("winget version must be latest or an exact published version")
	}
	if config.Architecture == "" {
		config.Architecture = "x64"
	}
	if !slices.Contains([]string{"x64", "x86", "arm64", "arm", "neutral"}, config.Architecture) {
		return config, errors.New("invalid winget architecture")
	}
	if config.Scope != "" && config.Scope != "user" && config.Scope != "machine" {
		return config, errors.New("winget scope must be user or machine")
	}
	if config.InstallerType != "" && !slices.Contains(wingetTypes, config.InstallerType) {
		return config, errors.New("invalid winget installer_type")
	}
	if config.Locale != "" && (!wingetLocale.MatchString(config.Locale) || len(config.Locale) > 20) {
		return config, errors.New("invalid winget locale")
	}
	config.Package = strings.ToLower(config.Package)
	return config, nil
}

func (m *Manager) wingetResolver() Resolver {
	return Resolver{Version: "1", Discover: m.discoverWinget, Fingerprint: func(input plugin.Input) (string, error) {
		config, err := wingetInput(input)
		if err != nil {
			return "", err
		}
		return fingerprint(config)
	}}
}

type wingetVersions struct {
	Schema   string `yaml:"sV"`
	Versions []struct {
		Version string `yaml:"v"`
		Path    string `yaml:"rP"`
		SHA256  string `yaml:"s256H"`
	} `yaml:"vD"`
}

type wingetFields map[string]yaml.Node

type wingetManifest struct {
	Package    string         `yaml:"PackageIdentifier"`
	Version    string         `yaml:"PackageVersion"`
	Schema     string         `yaml:"ManifestVersion"`
	Type       string         `yaml:"ManifestType"`
	Installers []wingetFields `yaml:"Installers"`
	Fields     wingetFields   `yaml:",inline"`
}

func (m *Manager) discoverWinget(ctx context.Context, input plugin.Input) (Discovery, error) {
	config, err := wingetInput(input)
	if err != nil {
		return Discovery{}, err
	}
	p, err := m.wingetPackage(ctx, config.Package)
	if err != nil {
		return Discovery{}, err
	}
	config.Package = p.ID
	data, err := m.wingetMetadata(ctx, "packages/"+p.ID+"/"+p.SHA256[:8]+"/versionData.mszyml", p.SHA256)
	if err != nil {
		return Discovery{}, fmt.Errorf("winget versions: %w", err)
	}
	data, err = decompressWinget(data)
	if err != nil {
		return Discovery{}, err
	}
	var versions wingetVersions
	if err := yaml.Unmarshal(data, &versions); err != nil {
		return Discovery{}, fmt.Errorf("winget versions: %w", err)
	}
	if versions.Schema != "1.0" {
		return Discovery{}, fmt.Errorf("unsupported winget version data schema %q", versions.Schema)
	}
	version := config.Version
	if version == "latest" {
		version = p.Latest
	}
	var relative, digest string
	for _, candidate := range versions.Versions {
		if candidate.Version == version {
			if relative != "" {
				return Discovery{}, errors.New("winget version data repeats the selected version")
			}
			relative, digest = candidate.Path, strings.ToLower(candidate.SHA256)
		}
	}
	if relative == "" {
		return Discovery{}, fmt.Errorf("winget %s has no version %q", p.ID, version)
	}
	if !strings.HasPrefix(relative, "manifests/") {
		return Discovery{}, errors.New("winget version has an invalid manifest path")
	}
	data, err = m.wingetMetadata(ctx, relative, digest)
	if err != nil {
		return Discovery{}, fmt.Errorf("winget manifest: %w", err)
	}
	return selectWinget(data, config, version)
}

func selectWinget(data []byte, config wingetConfig, version string) (Discovery, error) {
	var manifest wingetManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return Discovery{}, fmt.Errorf("winget manifest: %w", err)
	}
	if manifest.Package != config.Package || manifest.Version != version {
		return Discovery{}, errors.New("winget manifest package or version mismatch")
	}
	if !strings.HasPrefix(manifest.Schema, "1.") || manifest.Type != "merged" {
		return Discovery{}, errors.New("unsupported winget manifest schema or type")
	}
	var selected wingetFields
	matches := 0
	for _, installer := range manifest.Installers {
		claims, err := effectiveWingetInstaller(manifest.Fields, installer)
		if err != nil {
			return Discovery{}, err
		}
		if wingetString(claims, "Architecture") != config.Architecture ||
			config.Scope != "" && wingetString(claims, "Scope") != config.Scope ||
			config.InstallerType != "" && wingetString(claims, "InstallerType") != config.InstallerType ||
			config.Locale != "" && !strings.EqualFold(wingetString(claims, "InstallerLocale"), config.Locale) {
			continue
		}
		selected, matches = claims, matches+1
	}
	if matches != 1 {
		return Discovery{}, fmt.Errorf("winget %s %s: %d installers match; select architecture, scope, installer_type and locale to identify exactly one", config.Package, version, matches)
	}
	installerType := wingetString(selected, "InstallerType")
	if !slices.Contains(wingetTypes, installerType) || installerType == "msstore" || installerType == "pwa" {
		return Discovery{}, fmt.Errorf("unsupported winget installer transport %q", installerType)
	}
	if auth, ok := selected["Authentication"]; ok {
		var fields wingetFields
		if err := auth.Decode(&fields); err != nil || wingetString(fields, "AuthenticationType") != "none" {
			return Discovery{}, errors.New("winget authenticated installer downloads are unsupported")
		}
	}
	if prohibited, ok := selected["DownloadCommandProhibited"]; ok {
		var value bool
		if err := prohibited.Decode(&value); err != nil || value {
			return Discovery{}, errors.New("winget manifest prohibits standalone download")
		}
	}
	address := wingetString(selected, "InstallerUrl")
	if err := validateHTTPURL(address); err != nil {
		return Discovery{}, fmt.Errorf("winget installer URL: %w", err)
	}
	digest := strings.ToLower(wingetString(selected, "InstallerSha256"))
	if !validDigest(digest) {
		return Discovery{}, errors.New("winget installer requires a SHA256 digest")
	}
	u, _ := url.Parse(address)
	filename := path.Base(u.Path)
	if !validFilename(filename) {
		return Discovery{}, errors.New("winget installer URL has no valid filename")
	}
	evidence, err := wingetEvidence(selected)
	if err != nil {
		return Discovery{}, err
	}
	evidence["package_identifier"], evidence["package_version"] = config.Package, version
	evidence["installer_sha256"] = digest
	for _, key := range []string{"PackageName", "Publisher"} {
		if value := wingetString(manifest.Fields, key); value != "" {
			evidence[wingetFieldName(key)] = value
		}
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return Discovery{}, err
	}
	observation, err := json.Marshal(struct {
		Package string `json:"package"`
		Version string `json:"version"`
	}{config.Package, version})
	if err != nil {
		return Discovery{}, err
	}
	return Discovery{Version: version, Content: &Content{SHA256: digest, Filename: filename, Mode: 0o644}, Download: &plugin.Download{URL: address}, Observation: observation, Immutable: true, Evidence: map[string]json.RawMessage{"winget.installer": encoded}}, nil
}

var wingetInheritedFields = strings.Fields(`InstallerType PackageFamilyName ProductCode InstallerLocale Platform MinimumOSVersion Scope InstallModes InstallerSwitches InstallerSuccessCodes UpgradeBehavior Commands Protocols FileExtensions Dependencies Capabilities RestrictedCapabilities InstallerAbortsTerminal InstallLocationRequired RequireExplicitUpgrade ReleaseDate UnsupportedOSArchitectures ElevationRequirement Markets AppsAndFeaturesEntries ExpectedReturnCodes UnsupportedArguments DisplayInstallWarnings NestedInstallerType NestedInstallerFiles InstallationMetadata DownloadCommandProhibited RepairBehavior ArchiveBinariesDependOnPath Authentication DesiredStateConfiguration`)
var wingetLeafFields = []string{"Architecture", "InstallerUrl", "InstallerSha256", "SignatureSha256", "MSStoreProductIdentifier"}

// Follow ManifestYamlPopulator's declared inheritance only. Known switches and
// return codes inferred by the WinGet client are intentionally not evidence.
func effectiveWingetInstaller(root, leaf wingetFields) (wingetFields, error) {
	result := make(wingetFields)
	for _, key := range wingetInheritedFields {
		if value, ok := root[key]; ok && value.Tag != "!!null" {
			result[key] = value
		}
		if value, ok := leaf[key]; ok && value.Tag != "!!null" {
			result[key] = value
		}
	}
	for _, key := range wingetLeafFields {
		if value, ok := leaf[key]; ok && value.Tag != "!!null" {
			result[key] = value
		}
	}
	for _, key := range []string{"InstallerSwitches", "InstallationMetadata"} {
		rootValue, haveRoot := root[key]
		leafValue, haveLeaf := leaf[key]
		if haveRoot && haveLeaf && rootValue.Tag != "!!null" && leafValue.Tag != "!!null" {
			var defaults, overrides wingetFields
			if err := rootValue.Decode(&defaults); err != nil {
				return nil, err
			}
			if err := leafValue.Decode(&overrides); err != nil {
				return nil, err
			}
			for field, value := range overrides {
				if value.Tag != "!!null" && (key != "InstallationMetadata" || field != "Files" || len(value.Content) != 0) {
					defaults[field] = value
				}
			}
			var merged yaml.Node
			if err := merged.Encode(defaults); err != nil {
				return nil, err
			}
			result[key] = merged
		}
	}
	kind := wingetString(result, "InstallerType")
	for _, key := range []string{"NestedInstallerType", "NestedInstallerFiles"} {
		inheritWinget(result, root, leaf, key, kind == "zip")
	}
	if kind == "zip" {
		kind = wingetString(result, "NestedInstallerType")
	}
	arp := slices.Contains([]string{"exe", "inno", "msi", "nullsoft", "wix", "burn", "portable"}, kind)
	inheritWinget(result, root, leaf, "AppsAndFeaturesEntries", arp)
	inheritWinget(result, root, leaf, "ProductCode", arp)
	family := kind == "msix" || kind == "appx" || kind == "msstore"
	if entries, ok := result["AppsAndFeaturesEntries"]; ok {
		var values []wingetFields
		if err := entries.Decode(&values); err != nil {
			return nil, err
		}
		for _, entry := range values {
			family = family || slices.Contains([]string{"msix", "appx", "msstore"}, wingetString(entry, "InstallerType"))
		}
	}
	inheritWinget(result, root, leaf, "PackageFamilyName", family)
	if dependencies, ok := leaf["Dependencies"]; ok {
		var fields wingetFields
		if err := dependencies.Decode(&fields); err != nil {
			return nil, err
		}
		if !slices.ContainsFunc(slices.Collect(maps.Values(fields)), func(value yaml.Node) bool { return len(value.Content) > 0 }) {
			if value, ok := root["Dependencies"]; ok {
				result["Dependencies"] = value
			}
		}
	}
	return result, nil
}

func inheritWinget(result, root, leaf wingetFields, key string, allowed bool) {
	value, exists := leaf[key]
	if exists && value.Tag != "!!null" && (value.Kind == yaml.ScalarNode && value.Value != "" || value.Kind != yaml.ScalarNode && len(value.Content) > 0) {
		result[key] = value
		return
	}
	delete(result, key)
	if value, exists := root[key]; allowed && exists && value.Tag != "!!null" {
		result[key] = value
	}
}

func wingetString(fields wingetFields, key string) string {
	value := fields[key]
	if value.Kind != yaml.ScalarNode || value.Tag == "!!null" {
		return ""
	}
	return value.Value
}

func wingetEvidence(fields wingetFields) (map[string]any, error) {
	result := make(map[string]any, len(fields))
	for key, node := range fields {
		value, err := wingetEvidenceValue(node)
		if err != nil {
			return nil, fmt.Errorf("winget evidence %s: %w", key, err)
		}
		result[wingetFieldName(key)] = value
	}
	return result, nil
}

func wingetEvidenceValue(node yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		var fields wingetFields
		if err := node.Decode(&fields); err != nil {
			return nil, err
		}
		return wingetEvidence(fields)
	case yaml.SequenceNode:
		values := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := wingetEvidenceValue(*child)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case yaml.ScalarNode:
		if node.Tag == "!!str" || node.Tag == "!!timestamp" {
			return node.Value, nil
		}
		var value any
		err := node.Decode(&value)
		return value, err
	case yaml.DocumentNode, yaml.AliasNode, yaml.StreamNode:
		return nil, errors.New("unsupported YAML evidence node")
	}
	return nil, errors.New("invalid YAML evidence node")
}

func wingetFieldName(name string) string {
	var out strings.Builder
	for i, letter := range name {
		if unicode.IsUpper(letter) && i > 0 && (unicode.IsLower(rune(name[i-1])) || i+1 < len(name) && unicode.IsLower(rune(name[i+1]))) {
			out.WriteByte('_')
		}
		out.WriteRune(unicode.ToLower(letter))
	}
	return out.String()
}
