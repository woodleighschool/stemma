package intune

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
)

const (
	win32Type = "#microsoft.graph.win32LobApp"
	dmgType   = "#microsoft.graph.macOSDmgApp"
	pkgType   = "#microsoft.graph.macOSPkgApp"
	lobType   = "#microsoft.graph.macOSLobApp"
)

type object = map[string]any

func decodeObject(data []byte) (object, error) {
	var result object
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("expected JSON object")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected one JSON object")
	}
	return result, nil
}

func minimumOSFields() []string {
	fields := []string{"v10_7", "v10_8", "v10_9", "v10_10", "v10_11", "v10_12", "v10_13", "v10_14", "v10_15", "v11_0", "v12_0", "v13_0"}
	fields = append(fields, "v14_0", "v15_0", "v26_0")
	return fields
}

// Minimum OS is one selection even though Graph encodes it as boolean flags.
func selectedOS(value any) string {
	m, _ := value.(object)
	selected := ""
	for key, value := range m {
		if value == true {
			if selected != "" {
				return ""
			}
			selected = key
		}
	}
	return selected
}

func fields(value object, allowed ...string) error {
	for key := range value {
		if !slices.Contains(allowed, key) {
			return fmt.Errorf("unsupported field %q", key)
		}
	}
	return nil
}

func enum(value any, allowed ...string) bool {
	text, ok := value.(string)
	return ok && slices.Contains(allowed, text)
}
func text(value any) string         { result, _ := value.(string); return result }
func raw(value any) json.RawMessage { data, _ := json.Marshal(value); return data }

func mergeOwned(current, desired object) object {
	result := object{}
	maps.Copy(result, current)
	for key, value := range desired {
		if child, ok := value.(object); ok {
			previous, _ := current[key].(object)
			result[key] = mergeOwned(previous, child)
		} else {
			result[key] = value
		}
	}
	return result
}

func ownedEqual(current any, desired any) bool {
	if object, ok := desired.(object); ok {
		previous, ok := current.(map[string]any)
		if !ok {
			return len(object) == 0
		}
		for key, value := range object {
			if !ownedEqual(previous[key], value) {
				return false
			}
		}
		return true
	}
	if list, ok := desired.([]any); ok {
		previous, ok := current.([]any)
		if !ok || len(previous) != len(list) {
			return false
		}
		used := make([]bool, len(previous))
		for _, wanted := range list {
			found := false
			for i, observed := range previous {
				if !used[i] && ownedEqual(observed, wanted) {
					used[i] = true
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(current, desired)
}

func assignmentKey(value object) string {
	target, _ := value["target"].(object)
	return string(raw([]any{value["intent"], target["@odata.type"], target["groupId"]}))
}

func mergeItems(field string, current, desired []any) []any {
	key := func(item object) string {
		if field == "returnCodes" {
			return string(raw(item["returnCode"]))
		}
		return string(raw([]any{item["@odata.type"], item["ruleType"], item["productCode"], item["path"], item["fileOrFolderName"], item["keyPath"], item["valueName"]}))
	}
	previous := map[string]object{}
	for _, item := range current {
		if value, ok := item.(object); ok {
			previous[key(value)] = value
		}
	}
	result := make([]any, 0, len(desired))
	for _, item := range desired {
		value := item.(object)
		result = append(result, mergeOwned(previous[key(value)], value))
	}
	return result
}

func reconcileAssignments(current, desired []any) ([]any, bool) {
	byKey := map[string]object{}
	for _, item := range current {
		if v, ok := item.(object); ok {
			byKey[assignmentKey(v)] = v
		}
	}
	result := make([]any, 0, len(desired))
	changed := len(current) != len(desired)
	for _, item := range desired {
		v := item.(object)
		previous, ok := byKey[assignmentKey(v)]
		if !ok || !ownedEqual(previous, v) {
			changed = true
		}
		merged := mergeOwned(previous, v)
		delete(merged, "id")
		delete(merged, "@odata.context")
		result = append(result, merged)
	}
	return result, changed
}
