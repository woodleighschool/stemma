package munkirepo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
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
