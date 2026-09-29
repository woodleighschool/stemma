package intune

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
)

var (
	operators = map[string]string{
		"equal": "equal", "not_equal": "notEqual", "greater_than": "greaterThan", "greater_than_or_equal": "greaterThanOrEqual",
		"less_than": "lessThan", "less_than_or_equal": "lessThanOrEqual",
	}
	fileProperties     = map[string]string{"exists": "exists", "version": "version", "size_mb": "sizeInMB", "modified": "modifiedDate", "created": "createdDate"}
	registryProperties = map[string]string{"exists": "exists", "does_not_exist": "doesNotExist", "string": "string", "integer": "integer", "version": "version"}
)

// detection translates the complete detection-rule collection. Rules are
// ANDed, with at most one MSI rule, and a script rule must stand alone.
func detection(value any) (any, error) {
	list, ok := value.([]any)
	if !ok || len(list) > 100 {
		return nil, errors.New("must be an array of at most 100 rules")
	}
	result := make([]any, 0, len(list))
	msi := false
	for _, item := range list {
		rule, ok := item.(object)
		if !ok {
			return nil, errors.New("rule must be an object")
		}
		var native object
		var err error
		switch rule["type"] {
		case "msi":
			if msi {
				return nil, errors.New("only one msi rule is supported")
			}
			msi = true
			native, err = msiRule(rule)
		case "file":
			native, err = propertyRule(rule, fileProperties, map[string]string{"path": "path", "name": "fileOrFolderName"}, "exists")
			if native != nil {
				native["@odata.type"] = "#microsoft.graph.win32LobAppFileSystemRule"
			}
		case "registry":
			native, err = propertyRule(rule, registryProperties, map[string]string{"key": "keyPath", "value_name": "valueName"}, "exists", "does_not_exist")
			if native != nil {
				native["@odata.type"] = "#microsoft.graph.win32LobAppRegistryRule"
			}
		case "script":
			if len(list) != 1 {
				return nil, errors.New("a script rule must be the only detection rule")
			}
			native, err = scriptRule(rule)
		default:
			return nil, errors.New("rule type must be msi, file, registry or script")
		}
		if err != nil {
			return nil, err
		}
		native["ruleType"] = "detection"
		result = append(result, native)
	}
	return result, nil
}

func msiRule(rule object) (object, error) {
	if err := fields(rule, "type", "product_code", "product_version", "operator"); err != nil {
		return nil, err
	}
	if text(rule["product_code"]) == "" {
		return nil, errors.New("msi rule requires product_code")
	}
	native := object{"@odata.type": "#microsoft.graph.win32LobAppProductCodeRule", "productCode": rule["product_code"], "productVersionOperator": "notConfigured"}
	if value, exists := rule["operator"]; exists {
		operator, err := choice(value, operators)
		if err != nil {
			return nil, fmt.Errorf("operator %w", err)
		}
		if text(rule["product_version"]) == "" {
			return nil, errors.New("an msi rule with an operator requires product_version")
		}
		native["productVersionOperator"] = operator
	}
	if value, exists := rule["product_version"]; exists {
		if _, ok := value.(string); !ok {
			return nil, errors.New("product_version must be a string")
		}
		native["productVersion"] = value
	}
	return native, nil
}

// propertyRule translates a file or registry rule. An existence check takes
// no operator; every other property compares against a value. A version
// comparison defaults to greater than or equal to the managed version, which
// derivation supplies.
func propertyRule(rule object, properties, locations map[string]string, existence ...string) (object, error) {
	allowed := []string{"type", "check_32bit", "property", "operator", "value"}
	for key := range locations {
		allowed = append(allowed, key)
	}
	if err := fields(rule, allowed...); err != nil {
		return nil, err
	}
	native := object{"operator": "notConfigured"}
	for key, graph := range locations {
		value, exists := rule[key]
		if !exists {
			continue
		}
		if _, ok := value.(string); !ok {
			return nil, fmt.Errorf("%s must be a string", key)
		}
		native[graph] = value
	}
	for _, key := range []string{"path", "name", "key"} {
		if _, located := locations[key]; located && text(rule[key]) == "" {
			return nil, fmt.Errorf("%s rule requires %s", rule["type"], key)
		}
	}
	if value, exists := rule["check_32bit"]; exists {
		if _, ok := value.(bool); !ok {
			return nil, errors.New("check_32bit must be boolean")
		}
		native["check32BitOn64System"] = value
	}
	property, err := choice(rule["property"], properties)
	if err != nil {
		return nil, fmt.Errorf("property %w", err)
	}
	native["operationType"] = property
	_, hasOperator := rule["operator"]
	value, hasValue := rule["value"]
	if slices.Contains(existence, text(rule["property"])) {
		if hasOperator || hasValue {
			return nil, fmt.Errorf("a %s check takes no operator or value", rule["property"])
		}
		return native, nil
	}
	version := rule["property"] == "version"
	operator := "greaterThanOrEqual"
	if hasOperator || !version {
		if operator, err = choice(rule["operator"], operators); err != nil {
			return nil, fmt.Errorf("operator %w", err)
		}
	}
	native["operator"] = operator
	if !hasValue && version {
		return native, nil
	}
	comparison, ok := value.(string)
	if !hasValue || !ok || (comparison == "" && rule["property"] != "string") {
		return nil, fmt.Errorf("a %s comparison requires a value", rule["property"])
	}
	native["comparisonValue"] = comparison
	return native, nil
}

// scriptRule carries the PowerShell text; Graph's base64 encoding stays here.
func scriptRule(rule object) (object, error) {
	if err := fields(rule, "type", "script", "enforce_signature_check", "run_as_32bit"); err != nil {
		return nil, err
	}
	script := text(rule["script"])
	if script == "" || len(script) > 200000 {
		return nil, errors.New("script must be PowerShell text of at most 200000 bytes")
	}
	native := object{"@odata.type": "#microsoft.graph.win32LobAppPowerShellScriptRule", "scriptContent": base64.StdEncoding.EncodeToString([]byte(script))}
	for key, graph := range map[string]string{"enforce_signature_check": "enforceSignatureCheck", "run_as_32bit": "runAs32Bit"} {
		if value, exists := rule[key]; exists {
			if _, ok := value.(bool); !ok {
				return nil, fmt.Errorf("%s must be boolean", key)
			}
			native[graph] = value
		}
	}
	return native, nil
}
