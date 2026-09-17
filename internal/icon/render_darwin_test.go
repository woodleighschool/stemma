package icon

import "testing"

func TestRenderProducesCanonicalAsset(t *testing.T) {
	data, err := Render(t.Context(), "/System/Applications/TextEdit.app", Size)
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(data); err != nil {
		t.Fatal(err)
	}
	if _, err := Render(t.Context(), t.TempDir(), Size); err == nil {
		t.Fatal("rendered a directory that is not a bundle")
	}
}
