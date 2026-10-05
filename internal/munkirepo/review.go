package munkirepo

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// reviewCreation groups every initial field. Collection contents and long text
// remain in After for detailed review; unrecognised fields are never dropped.
func reviewCreation(document map[string]any) []string {
	groups := make([][]string, 5)
	for _, key := range slices.Sorted(maps.Keys(document)) {
		group := 4
		switch key {
		case "name", "display_name", "version", "description", "catalogs", "category", "developer":
			group = 0
		case "installer_item_location", "installer_item_size", "installer_item_hash", "installer_type", "installed_size":
			group = 1
		case "installs", "receipts", "installcheck_script", "uninstallcheck_script", "version_comparison_key":
			group = 2
		case "minimum_os_version", "maximum_os_version", "supported_architectures", "RestartAction", "unattended_install", "unattended_uninstall", "uninstallable", "uninstall_method", "blocking_applications", "requires", "update_for", "preinstall_script", "postinstall_script", "preuninstall_script", "postuninstall_script":
			group = 3
		}
		value := document[key]
		data, _ := json.Marshal(value)
		if text, ok := value.(string); ok {
			switch {
			case strings.Contains(text, "\n"):
				data = []byte(fmt.Sprintf("%d lines", len(strings.Split(strings.TrimSuffix(text, "\n"), "\n"))))
			case key == "installer_item_hash":
				if len(text) > 16 {
					text = text[:16] + "…"
				}
				data = []byte(text)
			default:
				data = []byte(text)
			}
		}
		if key == "installer_item_size" || key == "installed_size" {
			data = append(data, []byte(" KiB")...)
		}
		if key == "installs" || key == "receipts" {
			var items []map[string]any
			if json.Unmarshal(data, &items) == nil {
				names := make([]string, 0, len(items))
				identity := "path"
				if key == "receipts" {
					identity = "packageid"
				}
				for _, item := range items {
					name, ok := item[identity].(string)
					if !ok {
						name = "(no " + identity + ")"
					}
					if version, ok := item["version"].(string); ok {
						name += " · " + version
					}
					names = append(names, name)
				}
				noun := "entries"
				if len(items) == 1 {
					noun = "entry"
				}
				data = []byte(fmt.Sprintf("%d %s", len(items), noun))
				if len(names) > 0 {
					data = append(data, []byte(": "+strings.Join(names, ", "))...)
				}
			}
		}
		groups[group] = append(groups[group], "  "+key+": "+string(data))
	}
	var lines []string
	for i, title := range []string{"Package", "Installer", "Detection", "Install behaviour", "Other fields"} {
		if len(groups[i]) > 0 {
			lines = append(lines, title)
			lines = append(lines, groups[i]...)
		}
	}
	return lines
}
