package plugin

import "testing"

func TestSelectSubjectByIdentifier(t *testing.T) {
	facts := Facts{Subjects: []Subject{
		{ID: ".", Kind: "container", Path: "."},
		{ID: "Switchers.pkg/PackageInfo", Kind: "package", Path: "Switchers.pkg/PackageInfo", Package: &PackageFacts{Identifier: "com.example.switchers", Version: "10.4.1"}},
		{ID: "Bridge.pkg/PackageInfo", Kind: "package", Path: "Bridge.pkg/PackageInfo", Package: &PackageFacts{Identifier: "com.example.bridge", Version: "10.4"}},
		{ID: "Switchers.pkg/Payload/Control.app", Kind: "app", Path: "Switchers.pkg/Payload/Control.app", App: &AppFacts{BundleID: "com.example.control"}},
	}}
	for _, tc := range []struct {
		name     string
		selector SubjectSelector
		id, want string
	}{
		{name: "package identifier", selector: SubjectSelector{PackageID: "com.example.switchers"}, id: "Switchers.pkg/PackageInfo"},
		{name: "bundle identifier", selector: SubjectSelector{BundleID: "com.example.control"}, id: "Switchers.pkg/Payload/Control.app"},
		{name: "identifier and path disagree", selector: SubjectSelector{PackageID: "com.example.switchers", Path: "Bridge.pkg/PackageInfo"}, want: "selector matched no subject; package identifiers: com.example.bridge, com.example.switchers"},
		{name: "unknown package", selector: SubjectSelector{PackageID: "com.example.missing"}, want: "selector matched no subject; package identifiers: com.example.bridge, com.example.switchers"},
		{name: "unknown application", selector: SubjectSelector{BundleID: "com.example.missing"}, want: "selector matched no subject; bundle identifiers: com.example.control"},
		{name: "unknown path", selector: SubjectSelector{Path: "Missing.pkg/PackageInfo"}, want: "selector matched no subject"},
		{name: "several packages", selector: SubjectSelector{Kind: "package"}, want: "selector matched 2 subjects; require exactly one: Switchers.pkg/PackageInfo (Switchers.pkg/PackageInfo), Bridge.pkg/PackageInfo (Bridge.pkg/PackageInfo)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subject, err := SelectSubject(facts, tc.selector)
			if tc.want != "" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("got %v, want %s", err, tc.want)
				}
				return
			}
			if err != nil || subject.ID != tc.id {
				t.Fatalf("selected %q: %v", subject.ID, err)
			}
		})
	}
}
