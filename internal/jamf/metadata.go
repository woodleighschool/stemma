package jamf

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

type metadata struct {
	fields    map[string]json.RawMessage
	packageID string
	category  categoryName
	policies  []*installPolicy
	patch     *patchConfig
	retention plugin.Retention
}

// decodeMetadata validates the declaration and translates package field names
// at the transport boundary. Policies keep their declared settings until names resolve.
func decodeMetadata(data json.RawMessage) (metadata, error) {
	declared, err := decodeObject(data)
	if err != nil {
		return metadata{}, fmt.Errorf("jamf metadata: %w", err)
	}
	result := metadata{fields: map[string]json.RawMessage{}}
	for key, value := range declared {
		switch key {
		case "package_id":
			var id uint64
			if err := json.Unmarshal(value, &id); err != nil || id == 0 {
				return metadata{}, errors.New("package_id must be a positive integer")
			}
			result.packageID = strconv.FormatUint(id, 10)
		case "retention":
			if result.retention, err = decodeRetention(declared); err != nil {
				return metadata{}, err
			}
		case "patch":
			if result.patch, err = decodePatch(declared); err != nil {
				return metadata{}, err
			}
		case "policies":
			if result.policies, err = decodePolicies(declared); err != nil {
				return metadata{}, err
			}
		case "category":
			if err := json.Unmarshal(value, &result.category); err != nil || result.category.name != nil && strings.TrimSpace(*result.category.name) == "" {
				return metadata{}, errors.New("jamf category must be a category name or null")
			}
		default:
			rule, ok := packageFields[key]
			if !ok {
				return metadata{}, fmt.Errorf("unsupported Jamf package metadata %q", key)
			}
			if err := rule.validate(value); err != nil {
				return metadata{}, fmt.Errorf("jamf %s: %w", key, err)
			}
			result.fields[rule.native] = value
		}
	}
	if strings.Contains(stringField(result.fields, "notes"), "[stemma:v1 ") {
		return metadata{}, errors.New("jamf notes contains a reserved Stemma marker")
	}
	return result, nil
}

// A fieldRule is one declared package setting and the Jamf property it sets.
type fieldRule struct {
	native      string
	kind        string
	nullable    bool
	description string
}

func (r fieldRule) validate(value json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if r.nullable {
			return nil
		}
		return errors.New("null is not allowed")
	}
	var target any
	switch r.kind {
	case "string":
		target = new(string)
	case "bool":
		target = new(bool)
	case "int":
		target = new(int64)
	}
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("expected %s", r.kind)
	}
	return nil
}

var packageFields = map[string]fieldRule{
	"info":            {"info", "string", true, "Package information. Null clears the field."},
	"notes":           {"notes", "string", true, "Administrator notes. Null clears the text. Stemma keeps its identity marker on the final line and preserves the remote text when this is omitted."},
	"priority":        {"priority", "int", false, "Installation order among packages installed together; lower values install first. Defaults to 10 when creating a package."},
	"os_requirements": {"osRequirements", "string", true, "Operating system requirement expression, such as 14.x, 15.x. Null clears the field."},
	"reboot_required": {"rebootRequired", "bool", false, "Require a restart after installation. Explicit false is managed."},
}

// fieldName names a Jamf package property the way declarations do, so plans
// read in the catalog's terms.
func fieldName(native string) string {
	for name, rule := range packageFields {
		if rule.native == native {
			return name
		}
	}
	return map[string]string{"categoryId": "category", "fileName": "filename"}[native]
}

// rejectNulls fails on JSON nulls, except as the values of the named keys.
func rejectNulls(data json.RawMessage, nullable ...string) error {
	if len(data) == 0 {
		return errors.New("empty JSON value")
	}
	switch data[0] {
	case 'n':
		return errors.New("null is not supported")
	case '{':
		fields, err := decodeObject(data)
		if err != nil {
			return err
		}
		for key, value := range fields {
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) && slices.Contains(nullable, key) {
				continue
			}
			if err := rejectNulls(value, nullable...); err != nil {
				return err
			}
		}
	case '[':
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := rejectNulls(value, nullable...); err != nil {
				return err
			}
		}
	}
	return nil
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}

func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	if len(data) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("expected a JSON field name")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate JSON field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON data")
	}
	return fields, nil
}
