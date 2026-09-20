package macpkg

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestScriptContract(t *testing.T) {
	schema, err := json.Marshal(plugin.SchemaFor[Script]())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		value string
		valid bool
	}{
		{`"#!/bin/zsh --no-rcs\nset -e\n"`, true},
		{`{"$input":"postinstall"}`, true},
		{`{"$input":"hooks","path":"postinstall"}`, true},
		{`{"$input":"vendor","path":"."}`, true},
		{`{"$input":"hooks","unexpected":true}`, false},
		{`{"path":"postinstall"}`, false},
		{`null`, false},
		{`42`, false},
	} {
		t.Run(test.value, func(t *testing.T) {
			var script Script
			if err := json.Unmarshal([]byte(test.value), &script); (err == nil) != test.valid {
				t.Fatalf("decode: %v", err)
			}
			if err := plugin.ValidateSchema(schema, []byte(test.value)); (err == nil) != test.valid {
				t.Fatalf("schema: %v", err)
			}
			if test.valid {
				encoded, err := json.Marshal(script)
				if err != nil || string(encoded) != test.value {
					t.Fatalf("round trip: %s, %v", encoded, err)
				}
			}
		})
	}
}
