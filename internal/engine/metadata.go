package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/woodleighschool/stemma/internal/config"
	"reflect"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

func validateReferences(software plugin.ResourceResult) error {
	validate := func(value any) error {
		_, err := mapFactReferences(value, func(reference string) (any, error) {
			_, _, err := parseFactReference(software, reference)
			return nil, err
		})
		return err
	}
	for destination, native := range software.Destinations {
		if err := validate(native); err != nil {
			return fmt.Errorf("destination %s: %w", destination, err)
		}
	}

	return nil
}

func resolveMetadata(software plugin.ResourceResult, native map[string]any, facts plugin.Facts, evidence map[string]json.RawMessage) (map[string]any, map[string]string, error) {
	selected := map[string]plugin.Subject{}
	resolved, err := mapFactReferences(native, func(reference string) (any, error) {
		keys := sortedKeys(evidence)
		slices.SortFunc(keys, func(a, b string) int { return len(b) - len(a) })
		for _, key := range keys {
			if strings.HasPrefix(reference, key+".") {
				var value any
				if err := json.Unmarshal(evidence[key], &value); err != nil {
					return nil, err
				}
				for field := range strings.SplitSeq(strings.TrimPrefix(reference, key+"."), ".") {
					object, ok := value.(map[string]any)
					if !ok {
						return nil, fmt.Errorf("fact %s is not an object", key)
					}
					value, ok = object[field]
					if !ok || value == nil {
						return nil, fmt.Errorf("required fact %s is missing", reference)
					}
				}
				return value, nil
			}
		}
		name, fields, err := parseFactReference(software, reference)
		if err != nil {
			return nil, err
		}
		if _, present := selected[name]; !present {
			selector, exists := software.Subjects[name]
			if !exists {
				return nil, fmt.Errorf("required evidence %s is missing", reference)
			}
			match, err := plugin.SelectSubject(facts, selector)
			if err != nil {
				return nil, fmt.Errorf("subject %s: %w", name, err)
			}
			selected[name] = match
		}
		value, err := readFact(reflect.ValueOf(selected[name]), fields)
		if err != nil {
			return nil, fmt.Errorf("fact %s: %w", reference, err)
		}
		return value, nil
	})
	if err != nil {
		return nil, nil, err
	}
	explicit := resolved.(map[string]any)
	origins := make(map[string]string, len(explicit))
	for key, value := range native {
		origins[key] = "explicit"
		if hasFactReference(value) {
			origins[key] = "fact"
		}
	}
	return explicit, origins, nil
}

func mapFactReferences(value any, resolve func(string) (any, error)) (any, error) {
	switch value := value.(type) {
	case map[string]any:
		if reference, exists := value["$fact"]; exists {
			name, ok := reference.(string)
			if !ok || len(value) != 1 {
				return nil, errors.New("fact reference must be a sole-key {$fact: subject.field} object")
			}
			return resolve(name)
		}
		result := make(map[string]any, len(value))
		for key, child := range value {
			mapped, err := mapFactReferences(child, resolve)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", key, err)
			}
			result[key] = mapped
		}
		return result, nil
	case []any:
		result := make([]any, len(value))
		for i, child := range value {
			mapped, err := mapFactReferences(child, resolve)
			if err != nil {
				return nil, fmt.Errorf("item %d: %w", i, err)
			}
			result[i] = mapped
		}
		return result, nil
	default:
		return value, nil
	}
}

func parseFactReference(software plugin.ResourceResult, reference string) (string, []string, error) {
	name, field, ok := strings.Cut(reference, ".")
	if !ok || field == "" {
		return "", nil, fmt.Errorf("invalid fact reference %q", reference)
	}
	if _, exists := software.Subjects[name]; !exists {
		for part := range strings.SplitSeq(reference, ".") {
			if !safeOutputName(part) {
				return "", nil, fmt.Errorf("invalid fact reference %q", reference)
			}
		}
		return name, strings.Split(field, "."), nil
	}
	fields := strings.Split(field, ".")
	typ := reflect.TypeFor[plugin.Subject]()
	for _, field := range fields {
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		if typ.Kind() == reflect.Map && field != "" {
			typ = typ.Elem()
			continue
		}
		index := factField(typ, field)
		if index < 0 {
			return "", nil, fmt.Errorf("unknown fact field in %q", reference)
		}
		typ = typ.Field(index).Type
	}
	return name, fields, nil
}

func factField(typ reflect.Type, name string) int {
	if typ.Kind() != reflect.Struct || name == "" {
		return -1
	}
	for i := range typ.NumField() {
		field, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if field == name {
			return i
		}
	}
	return -1
}

func readFact(value reflect.Value, fields []string) (any, error) {
	for _, field := range fields {
		if value.Kind() == reflect.Pointer {
			if value.IsNil() {
				return nil, errors.New("required observed value is missing")
			}
			value = value.Elem()
		}
		if value.Kind() == reflect.Map {
			value = value.MapIndex(reflect.ValueOf(field))
			if !value.IsValid() {
				return nil, errors.New("required observed value is missing")
			}
		} else {
			value = value.Field(factField(value.Type(), field))
		}
	}
	if value.Kind() == reflect.String && value.String() == "" || (value.Kind() == reflect.Pointer || value.Kind() == reflect.Map || value.Kind() == reflect.Slice) && value.IsNil() {
		return nil, errors.New("required observed value is missing")
	}
	data, err := json.Marshal(value.Interface())
	if err != nil {
		return nil, err
	}
	var result any
	err = json.Unmarshal(data, &result)
	return result, err
}

func destinationMetadata(input map[string]any) map[string]any {
	result := config.Merge(input, nil)
	delete(result, "installer")
	delete(result, "inputs")
	return result
}
