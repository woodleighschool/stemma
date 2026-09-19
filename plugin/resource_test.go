package plugin_test

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestResourceOutputReferencePreservesSelection(t *testing.T) {
	var input plugin.Input
	if err := json.Unmarshal([]byte(`{"resource":{"kind":"BuildMacPkg","name":"payload","output":"installer"}}`), &input); err != nil {
		t.Fatal(err)
	}
	if input.Resource.Key() != "stemma/v1alpha1/BuildMacPkg/payload" || input.Resource.Output != "installer" {
		t.Fatalf("lost output reference: %+v", input.Resource)
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var again plugin.Input
	if err := json.Unmarshal(data, &again); err != nil {
		t.Fatal(err)
	}
	if *input.Resource != *again.Resource {
		t.Fatalf("round trip changed selection: %s", data)
	}
	identity := input.Resource.ResourceReference
	identity.APIVersion = "stemma/v1alpha1"
	if identity.Key() != input.Resource.Key() {
		t.Fatal("explicit API version changed identity")
	}
}
