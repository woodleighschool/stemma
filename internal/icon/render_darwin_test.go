package icon

import (
	"path/filepath"
	"testing"
)

func TestNativeQuickLook(t *testing.T) {
	output := filepath.Join(t.TempDir(), "icon.png")
	result, err := Export(t.Context(), "/System/Applications/TextEdit.app", output, Options{Size: 256})
	if err != nil {
		t.Fatal(err)
	}
	if result.Renderer != "quicklook" || result.Width != 256 || result.Height != 256 || result.OSBuild == "" || result.OSVersion == "" {
		t.Fatalf("unexpected native icon: %+v", result)
	}
}
