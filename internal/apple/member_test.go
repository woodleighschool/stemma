package apple

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestReadPackageMember(t *testing.T) {
	scripts := compressPayload(t, "gzip", cpioPayload(t, []payloadEntry{
		directoryEntry("."),
		fileEntry("./preinstall", 0o755, "#!/bin/sh\nexit 0\n"),
		directoryEntry("./lib"),
		fileEntry("./lib/helper.sh", 0o644, "helper\n"),
		fileEntry("./postinstall", 0o755, "#!/bin/sh\nexit 1\n"),
	}))
	// Nothing after the first file can be decoded.
	truncated := cpioPayload(t, []payloadEntry{fileEntry("./first", 0o644, "first\n"), fileEntry("./second", 0o644, "second\n")})
	truncated = truncated[:bytes.Index(truncated, []byte("second"))-40]
	name := writePayloadPackage(t, []payloadMember{
		{"Distribution", []byte("<installer-gui-script/>")},
		{"Example.pkg/PackageInfo", []byte(`<pkg-info identifier="org.example" version="1.0"/>`)},
		{"Example.pkg/Payload", cpioPayload(t, []payloadEntry{plistEntry(t, "./Example.app/Contents/Info.plist", "Example")})},
		{"Example.pkg/Scripts", scripts},
		{"Truncated.pkg/Scripts", truncated},
	})
	f, err := os.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ member, want, failure string }{
		{member: ".", want: "Distribution\nExample.pkg/\nTruncated.pkg/\n"},
		{member: "Distribution", want: "<installer-gui-script/>"},
		{member: "Example.pkg", want: "PackageInfo\nPayload/\nScripts/\n"},
		{member: "Example.pkg/Scripts", want: "lib/\npostinstall\npreinstall\n"},
		{member: "Example.pkg/Scripts/postinstall", want: "#!/bin/sh\nexit 1\n"},
		{member: "Example.pkg/Scripts/lib/helper.sh", want: "helper\n"},
		{member: "Example.pkg/Payload/Example.app/Contents", want: "Info.plist\n"},
		{member: "Truncated.pkg/Scripts/first", want: "first\n"},
		{member: "Truncated.pkg/Scripts/second", failure: "Truncated.pkg/Scripts: "},
		{member: "Distribtion", failure: `the package has no "Distribtion"; it holds Distribution, Example.pkg/, Truncated.pkg/`},
		{member: "Example.pkg/Script/postinstall", failure: `Example.pkg has no "Script"; it holds PackageInfo, Payload/, Scripts/`},
		{member: "Example.pkg/Scripts/postinstal", failure: `Example.pkg/Scripts has no "postinstal"; it holds lib/, postinstall, preinstall`},
		{member: "Example.pkg/Payload/Other.app/Contents/Info.plist", failure: `Example.pkg/Payload has no "Other.app"; it holds Example.app/`},
	} {
		t.Run(test.member, func(t *testing.T) {
			var got bytes.Buffer
			err := ReadPackageMember(t.Context(), f, info.Size(), test.member, &got)
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("error = %v, want %q", err, test.failure)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.String() != test.want {
				t.Fatalf("read %q, want %q", got.String(), test.want)
			}
		})
	}
}
