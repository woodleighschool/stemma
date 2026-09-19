package apple

import "testing"

func TestIconPathResolvesOnlyResourceNames(t *testing.T) {
	for _, test := range []struct {
		name, want string
		invalid    bool
	}{
		{"", "", false}, {"AppIcon", "Contents/Resources/AppIcon.icns", false}, {"AppIcon.icns", "Contents/Resources/AppIcon.icns", false},
		{"Icon[1]", "Contents/Resources/Icon[1].icns", false}, {"../Icon", "", true}, {"/Icon", "", true}, {"dir/Icon", "", true}, {"..", "", true}, {".", "", true}, {"dir\\Icon", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := IconPath(test.name)
			if (err != nil) != test.invalid || got != test.want {
				t.Fatalf("IconPath(%q)=%q,%v", test.name, got, err)
			}
		})
	}
}
