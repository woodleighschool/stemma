package munkirepo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"

	"github.com/woodleighschool/stemma/plugin"
)

func hashValue(value any) string {
	data, _ := json.Marshal(value)
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
func mergeFields(base, overlay map[string]any) map[string]any {
	result := make(map[string]any)
	maps.Copy(result, base)
	maps.Copy(result, overlay)
	return result
}

// fieldChanges compares the fields a publication writes or clears with the
// pkginfo it replaces, which is nil for new software. Unmanaged fields of a
// replaced version are not reported, though a new version omits them, and the
// version itself is never a field.
func fieldChanges(replaced, desired map[string]any, cleared []string) []plugin.FieldChange {
	keys := slices.Concat(cleared, slices.Collect(maps.Keys(desired)))
	slices.Sort(keys)
	var fields []plugin.FieldChange
	for _, key := range slices.Compact(keys) {
		before, had := replaced[key]
		after, has := desired[key]
		if key == "version" || hashValue(before) == hashValue(after) {
			continue
		}
		field := plugin.FieldChange{Field: "pkginfo." + key}
		if had {
			field.Before = raw(before)
		}
		if has {
			field.After = raw(after)
		}
		fields = append(fields, field)
	}
	return fields
}
