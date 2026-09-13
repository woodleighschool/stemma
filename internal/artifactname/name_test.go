package artifactname

import (
	"strings"
	"testing"
)

func TestFilename(t *testing.T) {
	if got := Filename("editor_", "4.2", "", "pkg"); got != "editor_-4.2.pkg" {
		t.Fatalf("resource name changed: %q", got)
	}
	for _, tc := range []struct{ version, want string }{
		{"4.2", "editor-4.2.pkg"},
		{"4.2 (123) / arm64", "editor-4.2-123-arm64.pkg"},
		{"", "editor-abcdef012345.pkg"},
		{strings.Repeat("1", 300), "editor-" + strings.Repeat("1", 80) + ".pkg"},
	} {
		if got := Filename("editor", tc.version, strings.Repeat("abcdef012345", 6)[:64], "pkg"); got != tc.want {
			t.Errorf("version %q: %q, want %q", tc.version, got, tc.want)
		}
	}
}
