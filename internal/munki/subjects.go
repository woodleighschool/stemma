package munki

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// resolveSubjects translates the inspected applications listed under installs
// into native application items, as derivation does for the selected
// application. Native items pass through unchanged.
func resolveSubjects(data json.RawMessage) (json.RawMessage, error) {
	var pkginfo map[string]json.RawMessage
	if err := json.Unmarshal(data, &pkginfo); err != nil {
		return nil, err
	}
	// Decoding the metadata reports malformed values; here they hold no subjects.
	var items []json.RawMessage
	var copies []CopyItem
	_ = json.Unmarshal(pkginfo["installs"], &items)
	_ = json.Unmarshal(pkginfo["items_to_copy"], &copies)
	translated := false
	for i, item := range items {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(item, &fields); err != nil || fields["type"] != nil {
			continue
		}
		native, err := subjectInstallItem(item, copies)
		if err != nil {
			return nil, fmt.Errorf("installs item %d: %w", i+1, err)
		}
		items[i], translated = native, true
	}
	if translated {
		pkginfo["installs"], _ = json.Marshal(items)
		return json.Marshal(pkginfo)
	}
	return data, nil
}

// subjectInstallItem builds the installs item that detects an inspected
// application at its installed path. A disk image's application is installed
// where its items_to_copy entry puts it.
func subjectInstallItem(data json.RawMessage, copies []CopyItem) (json.RawMessage, error) {
	var subject plugin.Subject
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&subject); err != nil || subject.Kind != "app" || subject.App == nil {
		return nil, errors.New("expected a native installs item or an inspected application, such as {{ facts.name }}")
	}
	endpoint, err := installedPath(subject.Path, subject.InstalledPath, copies)
	if err != nil {
		return nil, err
	}
	if endpoint == "" {
		return nil, fmt.Errorf("application %s has no known installed path; write a native installs item", subject.ID)
	}
	item, err := applicationItem(*subject.App, endpoint, "")
	if err != nil {
		return nil, fmt.Errorf("application %s: %w", subject.ID, err)
	}
	return json.Marshal(item)
}

func applicationItem(app plugin.AppFacts, endpoint, versionKey string) (InstallItem, error) {
	if versionKey == "" {
		versionKey = app.VersionKey()
	}
	version := app.Version
	if versionKey == "CFBundleVersion" {
		version = app.Build
	}
	if app.BundleID == "" || version == "" {
		return InstallItem{}, errors.New("bundle identifier and comparison version are required")
	}
	return InstallItem{Type: "application", Path: endpoint, BundleIdentifier: app.BundleID, BundleName: app.Name, BundleShortVersion: app.Version, BundleVersion: app.Build, VersionComparisonKey: versionKey, MinimumOSVersion: app.MinimumOS}, nil
}

func installedPath(source, observed string, copies []CopyItem) (string, error) {
	var copied string
	source = path.Clean(source)
	for _, action := range copies {
		item := path.Clean(action.SourceItem)
		if source != item && !strings.HasPrefix(source, item+"/") {
			continue
		}
		if copied != "" {
			return "", errors.New("application has multiple copy destinations; write a native installs item")
		}
		name := action.DestinationItem
		if name == "" {
			name = path.Base(item)
		}
		copied = path.Join(action.DestinationPath, name, strings.TrimPrefix(strings.TrimPrefix(source, item), "/"))
	}
	if observed != "" && copied != "" && observed != copied {
		return "", errors.New("application.installed_path disagrees with items_to_copy")
	}
	if copied != "" {
		return copied, nil
	}
	return observed, nil
}
