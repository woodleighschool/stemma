package plugin

import (
	"encoding/json"
	"testing"
)

func TestResolverEvidenceCanBeOmittedOrExplicitlyCleared(t *testing.T) {
	schema, err := json.Marshal(SchemaFor[ResolveResponse]())
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []map[string]json.RawMessage{nil, {}} {
		data, err := json.Marshal(ResolveResponse{Evidence: evidence})
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateSchema(schema, data); err != nil {
			t.Fatalf("resolver response %s: %v", data, err)
		}
		var decoded ResolveResponse
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatal(err)
		}
		if (decoded.Evidence == nil) != (evidence == nil) {
			t.Fatalf("lost omitted vs explicit empty evidence: %s", data)
		}
	}
}
