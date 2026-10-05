package munkirepo

import (
	"strings"
	"testing"
)

func TestCreationReviewRetainsUnfamiliarFieldsAndDetection(t *testing.T) {
	got := strings.Join(reviewCreation(map[string]any{
		"name": "Example", "version": "2.0", "catalogs": []string{"testing"},
		"receipts":       []any{map[string]any{"packageid": "org.example.app", "version": "2.0"}},
		"installs":       []any{map[string]any{"path": "/Applications/Example.app"}},
		"future_setting": map[string]any{"enabled": false}, "postinstall_script": "#!/bin/sh\necho done\n",
	}), "\n")
	for _, want := range []string{"Package", "version: 2.0", "catalogs: [\"testing\"]", "Detection", "org.example.app · 2.0", "/Applications/Example.app", "Install behaviour", "postinstall_script: 2 lines", "Other fields", "future_setting: {\"enabled\":false}"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
}
