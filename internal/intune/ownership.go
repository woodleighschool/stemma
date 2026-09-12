package intune

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

type derivedOwnership struct {
	Active    bool
	Unmanaged []string
	Paths     []string
}

func unmanagedFields(m object) ([]string, error) {
	value, present := m["unmanaged"]
	if !present {
		return nil, nil
	}
	list, ok := value.([]any)
	if !ok || len(list) > 100 {
		return nil, errors.New("unmanaged must be an array of at most 100 native field paths")
	}
	var result []string
	for _, value := range list {
		field := text(value)
		if !unmanagedPath(field) {
			return nil, fmt.Errorf("unsupported unmanaged native field path %q", field)
		}
		if authoredPath(m, field) {
			return nil, fmt.Errorf("unmanaged field %q overlaps explicitly authored native metadata", field)
		}
		result = append(result, field)
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func unmanagedPath(field string) bool {
	parts := strings.Split(field, ".")
	if field == "" || len(field) > 256 || slices.Contains([]string{"derive", "unmanaged", "type", "app_id", "retention", "content", "dependencies", "supersedes", "assignments"}, parts[0]) {
		return false
	}
	// Graph encodes one OS selection as boolean fields; ownership is atomic.
	if parts[0] == "minimumSupportedOperatingSystem" && len(parts) != 1 {
		return false
	}
	for _, variant := range MetadataSchema().OneOf {
		current := variant
		matched := true
		for _, part := range parts {
			if current.Properties == nil {
				matched = false
				break
			}
			next, exists := current.Properties.Get(part)
			if !exists {
				matched = false
				break
			}
			current = next
		}
		if matched {
			return true
		}
	}
	return false
}

func authoredPath(m object, field string) bool {
	parts := strings.Split(field, ".")
	for i, part := range parts {
		value, present := m[part]
		if !present {
			return false
		}
		child, objectValue := value.(object)
		if !objectValue {
			return true
		}
		if i == len(parts)-1 {
			return len(child) != 0
		}
		m = child
	}
	return false
}

func suppressedField(field string, unmanaged []string) bool {
	return slices.ContainsFunc(unmanaged, func(parent string) bool {
		return field == parent || strings.HasPrefix(field, parent+".")
	})
}

func mergeDerived(authored, defaults object, prefix, origin string, unmanaged []string, origins map[string]string) object {
	result := maps.Clone(authored)
	if result == nil {
		result = object{}
	}
	for key, value := range defaults {
		field := prefix + key
		if suppressedField(field, unmanaged) {
			continue
		}
		explicit, present := authored[key]
		if nested, ok := value.(object); ok && field != "minimumSupportedOperatingSystem" {
			previous, objectValue := explicit.(object)
			if present && !objectValue {
				continue
			}
			merged := mergeDerived(previous, nested, field+".", origin, unmanaged, origins)
			if len(merged) != 0 {
				result[key] = merged
			}
		} else if !present {
			result[key] = value
			origins[field] = origin
		}
	}
	return result
}

func (o *derivedOwnership) check(previous []string, desired object) error {
	if o == nil || !o.Active {
		return nil
	}
	for _, field := range previous {
		if !suppressedField(field, o.Unmanaged) && !authoredPath(desired, field) {
			return fmt.Errorf("previously derived Intune field %q is no longer available; author an explicit replacement or add the field to unmanaged", field)
		}
	}
	return nil
}

func (o *derivedOwnership) paths() []string {
	if o == nil || !o.Active {
		return nil
	}
	return slices.Clone(o.Paths)
}
