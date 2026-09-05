package config

import (
	"encoding/json"
	"testing"
)

func TestSchemaIncludesEditorDescriptions(t *testing.T) {
	data, err := Schema()
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Definitions map[string]struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	for name, fields := range map[string][]string{"Project": {"project", "recipes"}, "Source": {"type", "url", "sha256"}, "Verification": {"subject", "integrity"}, "MunkiMetadata": {"description", "catalogs", "unattended_install"}, "IntuneConnection": {"token_env", "client_id_env"}, "JamfMetadata": {"package_id", "categoryId"}} {
		for _, field := range fields {
			if schema.Definitions[name].Properties[field].Description == "" {
				t.Errorf("%s.%s lacks editor hover description", name, field)
			}
		}
	}
}
