package apple

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/deploymenttheory/go-macos-pkg/pkg/cpio"
)

func directoryEntry(name string) payloadEntry {
	return payloadEntry{cpio.Header{Name: name, Mode: cpio.ModeDir | 0o755, NLink: 2}, nil}
}

func fileEntry(name string, perm uint32, body string) payloadEntry {
	return payloadEntry{cpio.Header{Name: name, Mode: cpio.ModeRegular | perm, NLink: 1}, []byte(body)}
}

func applicationPackage(t *testing.T, installLocation string, entries []payloadEntry) string {
	t.Helper()
	metadata := `<pkg-info identifier="org.example.example" version="1.0" install-location="` + installLocation + `"><payload installKBytes="20"/></pkg-info>`
	return writePayloadPackage(t, []payloadMember{{"PackageInfo", []byte(metadata)}, {"Payload", compressPayload(t, "gzip", cpioPayload(t, entries))}})
}

func TestExtractApplicationWritesOnlyTheBundle(t *testing.T) {
	const app = "./Applications/Example.app"
	plist := plistEntry(t, app+"/Contents/Info.plist", "Example")
	name := applicationPackage(t, "/", []payloadEntry{
		directoryEntry("."), directoryEntry("./Applications"), directoryEntry(app), directoryEntry(app + "/Contents"), plist,
		directoryEntry(app + "/Contents/MacOS"), fileEntry(app+"/Contents/MacOS/Example", 0o755, "#!/bin/sh\nexit 0\n"),
		{cpio.Header{Name: app + "/Contents/._MacOS", Mode: cpio.ModeRegular | 0o644, NLink: 2}, []byte("sidecar")},
		directoryEntry(app + "/Contents/Resources"), fileEntry(app+"/Contents/Resources/AppIcon.icns", 0o644, "icns"),
		{cpio.Header{Name: app + "/Contents/Current", Mode: cpio.ModeSymlink | 0o755, NLink: 1}, []byte("Resources")},
		directoryEntry("./Library"), fileEntry("./Library/readme.txt", 0o644, "outside the bundle"),
	})
	facts, err := InspectPackageContents(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	selected := facts.Applications[0]
	if selected.Path != "Payload/Applications/Example.app" {
		t.Fatalf("application path %q", selected.Path)
	}
	destination := t.TempDir()
	bundle, err := ExtractApplication(t.Context(), name, selected.Path, selected.InstalledPath, destination)
	if err != nil {
		t.Fatal(err)
	}
	if bundle != filepath.Join(destination, "Example.app") {
		t.Fatalf("bundle path %q", bundle)
	}
	info, err := os.ReadFile(filepath.Join(bundle, "Contents/Info.plist"))
	if err != nil || !bytes.Equal(info, plist.body) {
		t.Fatalf("Info.plist: %v", err)
	}
	executable, err := os.Stat(filepath.Join(bundle, "Contents/MacOS/Example"))
	if err != nil || executable.Mode().Perm() != 0o755 {
		t.Fatalf("executable: %v %v", executable, err)
	}
	if target, err := os.Readlink(filepath.Join(bundle, "Contents/Current")); err != nil || target != "Resources" {
		t.Fatalf("symlink: %q %v", target, err)
	}
	entries, err := os.ReadDir(destination)
	if err != nil || len(entries) != 1 {
		t.Fatalf("destination holds %v, %v", entries, err)
	}
	if _, err := ExtractApplication(t.Context(), name, "Payload/Applications/Other.app", "", t.TempDir()); err == nil {
		t.Fatal("extracted an application the payload does not hold")
	}
}

func TestExtractApplicationFromBundleRootPayload(t *testing.T) {
	name := applicationPackage(t, "/Applications/Example.app", []payloadEntry{
		directoryEntry("."), directoryEntry("./Contents"), plistEntry(t, "./Contents/Info.plist", "Example"),
		directoryEntry("./Contents/MacOS"), fileEntry("./Contents/MacOS/Example", 0o755, "#!/bin/sh\n"),
	})
	facts, err := InspectPackageContents(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	selected := facts.Applications[0]
	bundle, err := ExtractApplication(t.Context(), name, selected.Path, selected.InstalledPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(bundle) != "Example.app" {
		t.Fatalf("bundle path %q", bundle)
	}
	if _, err := os.Stat(filepath.Join(bundle, "Contents/Info.plist")); err != nil {
		t.Fatal(err)
	}
}
