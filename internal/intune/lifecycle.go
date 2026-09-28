package intune

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/woodleighschool/stemma/plugin"
)

type lifecycle struct {
	Dependencies []relationshipReference
	Supersedes   []relationshipReference
}

type relationshipReference struct {
	Resource *plugin.ResourceReference
	AppID    string
	Install  bool
}

func lifecycleMetadata(m object) (lifecycle, error) {
	var result lifecycle
	for _, category := range []string{"dependencies", "supersedes"} {
		value, owned := m[category]
		if !owned {
			continue
		}
		if m["@odata.type"] != win32Type {
			return result, errors.New("dependencies and supersedes require a Win32 app")
		}
		list, ok := value.([]any)
		limit := 99
		flag := "auto_install"
		if category == "supersedes" {
			limit, flag = 9, "uninstall_previous"
		}
		if !ok || len(list) > limit {
			return result, fmt.Errorf("%s must be an array of at most %d software references", category, limit)
		}
		refs := make([]relationshipReference, 0, len(list))
		seen := map[string]bool{}
		for _, value := range list {
			item, ok := value.(object)
			if !ok {
				return result, errors.New("relationship must be an object")
			}
			if err := fields(item, "resource", "app_id", flag); err != nil {
				return result, err
			}
			var ref relationshipReference
			_, resource := item["resource"]
			_, external := item["app_id"]
			if resource == external {
				return result, fmt.Errorf("%s relationship requires exactly one of resource or app_id", category)
			}
			var key string
			if resource {
				decoder := json.NewDecoder(bytes.NewReader(raw(item["resource"])))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(&ref.Resource); err != nil {
					return result, fmt.Errorf("%s resource: %w", category, err)
				}
				if ref.Resource == nil {
					return result, errors.New("resource requires kind and name")
				}
				if err := ref.Resource.Validate(); err != nil {
					return result, err
				}
				key = ref.Resource.Key()
			} else {
				ref.AppID = text(item["app_id"])
				if ref.AppID == "" {
					return result, errors.New("relationship app_id must be a nonempty string")
				}
				key = "app_id/" + ref.AppID
			}
			install, ok := item[flag].(bool)
			if !ok || seen[key] {
				return result, fmt.Errorf("%s requires unique references and explicit %s booleans", category, flag)
			}
			seen[key] = true
			ref.Install = install
			refs = append(refs, ref)
		}
		if category == "dependencies" {
			result.Dependencies = refs
		} else {
			result.Supersedes = refs
		}
	}
	return result, nil
}
