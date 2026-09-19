package munki

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"

	"github.com/woodleighschool/stemma/plugin"
)

type resourceRelationship struct {
	Resource plugin.ResourceReference `json:"resource" jsonschema:"required"`
	Version  string                   `json:"version,omitempty"`
}

// ResolveReferences translates explicit resource relationships into native
// pkginfo strings. Strings supplied by the author pass through unchanged.
func ResolveReferences[C any](request plugin.ReconcileRequest[C]) (plugin.ReconcileRequest[C], error) {
	var metadata map[string]json.RawMessage
	if len(request.Metadata) == 0 {
		return request, nil
	}
	if err := json.Unmarshal(request.Metadata, &metadata); err != nil {
		return request, err
	}
	var pkginfo map[string]json.RawMessage
	if data, exists := metadata["pkginfo"]; exists {
		if err := json.Unmarshal(data, &pkginfo); err != nil {
			return request, err
		}
	}
	for _, field := range []string{"requires", "update_for"} {
		data, exists := pkginfo[field]
		if !exists {
			continue
		}
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return request, fmt.Errorf("%s: %w", field, err)
		}
		for i, item := range items {
			if len(item) == 0 || item[0] != '{' {
				continue
			}
			var link resourceRelationship
			decoder := json.NewDecoder(bytes.NewReader(item))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&link); err != nil {
				return request, fmt.Errorf("%s: %w", field, err)
			}
			if err := link.Resource.Validate(); err != nil {
				return request, fmt.Errorf("%s: %w", field, err)
			}
			peer, exists := request.Peers[link.Resource.Key()]
			if !exists {
				return request, fmt.Errorf("%s: resource %s does not publish to this destination", field, link.Resource.Key())
			}
			var declared struct {
				Pkginfo struct {
					Name string `json:"name"`
				} `json:"pkginfo"`
			}
			if err := json.Unmarshal(peer, &declared); err != nil {
				return request, fmt.Errorf("%s resource %s: %w", field, link.Resource.Key(), err)
			}
			name := cmp.Or(declared.Pkginfo.Name, link.Resource.Name)
			if link.Version != "" {
				name += "--" + link.Version
			}
			items[i], _ = json.Marshal(name)
		}
		pkginfo[field], _ = json.Marshal(items)
	}
	if pkginfo != nil {
		metadata["pkginfo"], _ = json.Marshal(pkginfo)
		request.Metadata, _ = json.Marshal(metadata)
	}
	return request, nil
}
