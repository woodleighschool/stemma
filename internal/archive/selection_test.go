package archive

import "testing"

func TestLeavesKeepNamedFilesAndLeadToThem(t *testing.T) {
	leaves := Leaves{"Contents/Info.plist", "Contents/MacOS/" + Literal("Example [Beta]*"), "Contents/Resources/*.icns"}
	if err := leaves.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{
		"Contents/Info.plist":                  true,
		"Contents/MacOS/Example [Beta]*":       true,
		"Contents/MacOS/Example B":             false,
		"Contents/MacOS/helper":                false,
		"Contents/Resources/AppIcon.icns":      true,
		"Contents/Resources/en.lproj/App.icns": false,
		"Contents/Resources/AppIcon.icns.bak":  false,
		"Info.plist":                           false,
	} {
		if got := leaves.Keeps(name); got != want {
			t.Errorf("Keeps(%q) = %v, want %v", name, got, want)
		}
	}
	for dir, want := range map[string]bool{
		".":                           true,
		"Contents":                    true,
		"Contents/MacOS":              true,
		"Contents/Resources":          true,
		"Contents/Resources/en.lproj": false,
		"Contents/Frameworks":         false,
		"Contents/MacOSX":             false,
		"Content":                     false,
	} {
		if got := leaves.Leads(dir); got != want {
			t.Errorf("Leads(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestNilLeavesKeepEverything(t *testing.T) {
	var leaves Leaves
	if !leaves.Keeps("any/file") || !leaves.Leads("any/directory") || leaves.Validate() != nil {
		t.Fatal("nil leaves filtered an entry")
	}
	if (Leaves{}).Keeps("any/file") {
		t.Fatal("empty leaves kept a file")
	}
}

func TestLeavesRejectPatternedDirectoriesAndEscapes(t *testing.T) {
	for _, leaf := range []string{".", "", "/Contents/Info.plist", "../Info.plist", "Contents/*/Info.plist", "Contents/Resources/[", `Con\tents/Info.plist`} {
		if err := (Leaves{leaf}).Validate(); err == nil {
			t.Errorf("leaf %q accepted", leaf)
		}
	}
}
