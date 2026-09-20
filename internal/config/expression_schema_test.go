package config

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestExpressionSchemaRetainsNativeLiteralConstraints(t *testing.T) {
	native := json.RawMessage(`{
		"$id":"https://fixture.invalid/expressions",
		"type":"object","required":["enabled","settings"],"additionalProperties":false,
		"properties":{
			"enabled":{"type":"boolean"},
			"settings":{"$ref":"#/$defs/settings"},
			"labels":{"type":"array","items":{"type":"string","enum":["release","preview"]}},
			"version":{"type":"string","pattern":"^[0-9]+$"}
		},
		"$defs":{"settings":{"type":"object","required":["count"],"additionalProperties":false,"properties":{"count":{"type":"integer","minimum":1}}}}
	}`)
	schema, err := ExpressionSchema(native)
	if err != nil {
		t.Fatal(err)
	}
	for name, test := range map[string]struct {
		value string
		valid bool
	}{
		"literal":                          {`{"enabled":true,"settings":{"count":1}}`, true},
		"whole object":                     {`"{{ facts.metadata }}"`, true},
		"typed values":                     {`{"enabled":"{{ facts.enabled }}","settings":{"count":"{{ facts.count }}"}}`, true},
		"nested object":                    {`{"enabled":true,"settings":"{{ facts.settings }}"}`, true},
		"list passthrough":                 {`{"enabled":true,"settings":{"count":1},"labels":"{{ facts.labels }}"}`, true},
		"embedded string":                  {`{"enabled":true,"settings":{"count":1},"version":"1{{ facts.revision }}"}`, true},
		"missing required with expression": {`{"enabled":"{{ facts.enabled }}"}`, false},
		"unknown field with expression":    {`{"enabled":"{{ facts.enabled }}","settings":{"count":1},"typo":true}`, false},
		"invalid boolean literal":          {`{"enabled":"yes","settings":{"count":1}}`, false},
		"invalid nested integer":           {`{"enabled":true,"settings":{"count":0}}`, false},
		"invalid list item":                {`{"enabled":true,"settings":{"count":1},"labels":["other"]}`, false},
		"embedded boolean":                 {`{"enabled":"prefix {{ facts.enabled }}","settings":{"count":1}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			if err := plugin.ValidateSchema(schema, []byte(test.value)); (err == nil) != test.valid {
				t.Fatalf("valid=%v, expected %v: %v", err == nil, test.valid, err)
			}
		})
	}
}
