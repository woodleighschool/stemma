package intune

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// cleared is an owned field the artifact gives no value that Graph clears with null.
type cleared struct{}

// mergeDerived fills the fields a derivation owns, which are the keys of
// defaults, unless the declaration sets them. An owned field the artifact
// gives no value is cleared where Graph allows it. Graph requires the others,
// so they are an error: omitting one would keep whatever an earlier artifact
// published.
func mergeDerived(declared, defaults object, origin string, origins map[string]string) (object, error) {
	merged, missing := fillDerived(declared, defaults, "", origin, origins)
	if len(missing) != 0 {
		for i, property := range missing {
			missing[i] = reportName(property)
		}
		slices.Sort(missing)
		return nil, fmt.Errorf("%s supplies no value for %s; set each field", origin, strings.Join(missing, ", "))
	}
	return merged, nil
}

func fillDerived(declared, defaults object, prefix, origin string, origins map[string]string) (object, []string) {
	result := maps.Clone(declared)
	if result == nil {
		result = object{}
	}
	var missing []string
	for key, value := range defaults {
		field := prefix + key
		explicit, present := declared[key]
		if nested, ok := value.(object); ok && field != "minimumSupportedOperatingSystem" {
			previous, objectValue := explicit.(object)
			if present && !objectValue {
				continue
			}
			merged, absent := fillDerived(previous, nested, field+".", origin, origins)
			missing = append(missing, absent...)
			if len(merged) != 0 {
				result[key] = merged
			}
		} else if !present {
			if value == (cleared{}) {
				result[key], origins[field] = nil, origin
				continue
			}
			if value == nil || value == "" {
				missing = append(missing, field)
				continue
			}
			result[key] = value
			origins[field] = origin
		}
	}
	return result, missing
}
