package macsoftware

import "testing"

func TestNativeQuickLook(t *testing.T) {
	data, version, build, err := nativeIcon(t.Context(), "/System/Applications/TextEdit.app", 256)
	if err != nil {
		t.Fatal(err)
	}
	if !validPNG(data) || version == "" || build == "" {
		t.Fatal("missing native PNG or provenance")
	}
}
