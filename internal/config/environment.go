package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
)

var environmentPlaceholder = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

func parseConfig(data []byte, value any) (map[string]any, error) {
	document, err := parseDocument(data, value)
	if err != nil {
		return nil, err
	}
	if _, err := expandEnvironment(document["spec"]); err != nil {
		return nil, err
	}
	encoded, err := yaml.Marshal(document)
	if err != nil {
		return nil, err
	}
	if err := decodeStrict(encoded, value); err != nil {
		return nil, err
	}
	return document, nil
}

func expandEnvironment(value any) (any, error) {
	switch value := value.(type) {
	case string:
		match := environmentPlaceholder.FindStringSubmatch(value)
		if len(match) == 2 {
			resolved, ok := os.LookupEnv(match[1])
			if !ok {
				return nil, fmt.Errorf("environment variable %s is not set", match[1])
			}
			return resolved, nil
		}
	case map[string]any:
		for key, child := range value {
			if strings.Contains(key, "${") {
				return nil, errors.New("environment placeholders are not supported in mapping keys")
			}
			resolved, err := expandEnvironment(child)
			if err != nil {
				return nil, err
			}
			value[key] = resolved
		}
	case []any:
		for i, child := range value {
			resolved, err := expandEnvironment(child)
			if err != nil {
				return nil, err
			}
			value[i] = resolved
		}
	}
	return value, nil
}
