package pkgbuild

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/signature"
)

func fixture(t *testing.T) (string, Options) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Payload/Library/Application Support/Fixture/message.txt"), []byte("original payload\n"), 0o640)
	writeFile(t, filepath.Join(root, "Scripts/preinstall"), []byte("#!/bin/sh\n# Original fixture: must never run during build.\nexit 93\n"), 0o644)
	writeFile(t, filepath.Join(root, "Scripts/postinstall"), []byte("#!/bin/sh\nexit 94\n"), 0o755)
	return root, Options{Identifier: "au.edu.vic.woodleigh.stemma.local-fixture", Version: "1.2.3", Timestamp: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), Payload: "Payload", Scripts: "Scripts"}
}
func writeFile(t *testing.T, name string, contents []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, contents, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
}
func TestBuildIntegrityReproducibilityAndInputChanges(t *testing.T) {
	root, opts := fixture(t)
	output := filepath.Join(t.TempDir(), "first.pkg")
	if err := Build(t.Context(), root, output, opts); err != nil {
		t.Fatal(err)
	}
	facts, err := apple.InspectPackage(output)
	if err != nil || len(facts.Packages) != 1 {
		t.Fatalf("inspection: %+v: %v", facts, err)
	}
	if facts.Packages[0].Identifier != opts.Identifier || facts.Packages[0].Version != opts.Version {
		t.Fatalf("wrong package facts: %+v", facts.Packages)
	}
	if _, err := apple.VerifyPackage(t.Context(), output, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("built package claimed a signer: %v", err)
	}
	first, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Unix(2000000000, 0)
	if err := os.Chtimes(filepath.Join(root, "Payload/Library/Application Support/Fixture/message.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(t.TempDir(), "second.pkg")
	if err := Build(t.Context(), root, second, opts); err != nil {
		t.Fatal(err)
	}
	next, _ := os.ReadFile(second)
	if !bytes.Equal(first, next) {
		t.Fatal("explicit timestamp did not isolate package bytes from source mtimes")
	}
	writeFile(t, filepath.Join(root, "Scripts/postinstall"), []byte("#!/bin/sh\nexit 95\n"), 0o755)
	third := filepath.Join(t.TempDir(), "third.pkg")
	if err := Build(t.Context(), root, third, opts); err != nil {
		t.Fatal(err)
	}
	changed, _ := os.ReadFile(third)
	if sha256.Sum256(first) == sha256.Sum256(changed) {
		t.Fatal("script edit did not change artifact")
	}
	first[len(first)-1] ^= 1
	tampered := filepath.Join(t.TempDir(), "tampered.pkg")
	if err := os.WriteFile(tampered, first, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := apple.VerifyPackage(t.Context(), tampered, signature.Signer{}); err == nil || strings.Contains(err.Error(), "not signed") {
		t.Fatalf("tampered archive passed integrity: %v", err)
	}
}
func TestBuildRejectsUnsupportedOrUnsafeInputs(t *testing.T) {
	for _, name := range []string{"empty", "traversal", "scripts-file", "no-hooks", "nested-hook", "symlink-hook", "directory-hook", "oversize-hook", "script-symlink", "script-metadata", "symlink", "oversize", "total-size", "output-in-root", "output-via-symlink", "script-ancestor", "existing-output", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			root, opts := fixture(t)
			output := filepath.Join(t.TempDir(), "out.pkg")
			ctx := t.Context()
			switch name {
			case "empty":
				opts.Payload = ""
				opts.Scripts = ""
			case "traversal":
				opts.Scripts = "../outside"
			case "scripts-file":
				opts.Scripts = "Scripts/preinstall"
			case "no-hooks", "nested-hook", "symlink-hook", "directory-hook":
				opts.Payload = ""
				if err := os.RemoveAll(filepath.Join(root, "Scripts")); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(root, "Scripts/resource"), []byte("resource"), 0o644)
				switch name {
				case "nested-hook":
					writeFile(t, filepath.Join(root, "Scripts/nested/postinstall"), []byte("#!/bin/sh\nexit 91\n"), 0o755)
				case "symlink-hook":
					if err := os.Symlink("resource", filepath.Join(root, "Scripts/postinstall")); err != nil {
						t.Fatal(err)
					}
				case "directory-hook":
					if err := os.Mkdir(filepath.Join(root, "Scripts/postinstall"), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			case "oversize-hook":
				writeFile(t, filepath.Join(root, "Scripts/postinstall"), bytes.Repeat([]byte("#"), MaxScriptSize+1), 0o755)
			case "script-symlink":
				if err := os.Symlink("../Payload", filepath.Join(root, "Scripts/escape")); err != nil {
					t.Fatal(err)
				}
			case "script-metadata":
				opts.ScriptMetadata = map[string]EntryMetadata{"missing": {UID: 501}}
			case "symlink":
				if err := os.Symlink("../../../../../outside", filepath.Join(root, "Payload/Library/Application Support/Fixture/link")); err != nil {
					t.Skip(err)
				}
			case "oversize", "total-size":
				f, err := os.Create(filepath.Join(root, "Payload/large"))
				if err != nil {
					t.Fatal(err)
				}
				size := maxFileSize + 1
				if name == "total-size" {
					size = MaxPayloadSize + 1
				}
				if err := f.Truncate(size); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			case "output-in-root":
				output = filepath.Join(root, "output.pkg")
			case "output-via-symlink":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(root, alias); err != nil {
					t.Skip(err)
				}
				output = filepath.Join(alias, "output.pkg")
			case "script-ancestor":
				if err := os.Symlink("Scripts", filepath.Join(root, "ScriptsAlias")); err != nil {
					t.Skip(err)
				}
				opts.Scripts = "ScriptsAlias"
			case "existing-output":
				writeFile(t, output, []byte("keep me"), 0o644)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err := Build(ctx, root, output, opts); err == nil {
				t.Fatal("invalid build succeeded")
			}
			if name == "existing-output" {
				data, _ := os.ReadFile(output)
				if string(data) != "keep me" {
					t.Fatal("existing output was modified")
				}
			} else if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed build left output: %v", err)
			}
		})
	}
}

func TestBuildSymlinksStayWithinEachArchive(t *testing.T) {
	for _, area := range []string{"Payload", "Scripts"} {
		for _, tc := range []struct {
			name, target string
			valid        bool
		}{
			{"chain", "alias/data/resource", true},
			{"dangling", "alias/data/missing", true},
			{"escape", "alias/data/../..", false},
		} {
			t.Run(area+"/"+tc.name, func(t *testing.T) {
				root, opts := fixture(t)
				writeFile(t, filepath.Join(root, area, "data/resource"), []byte("resource"), 0o644)
				for name, target := range map[string]string{"alias": ".", "link": tc.target} {
					if err := os.Symlink(target, filepath.Join(root, area, name)); err != nil {
						t.Skip(err)
					}
				}
				output := filepath.Join(t.TempDir(), "links.pkg")
				err := Build(t.Context(), root, output, opts)
				if !tc.valid {
					if err == nil || !strings.Contains(err.Error(), "symlink") {
						t.Fatalf("escaping chain accepted: %v", err)
					}
					if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("failed build left output: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range scriptEntries(t, packageMember(t, output, area)) {
					if entry.name == "./link" {
						if entry.mode&0o170000 != 0o120000 || string(entry.contents) != tc.target {
							t.Fatalf("symlink changed: mode %o target %q", entry.mode, entry.contents)
						}
						return
					}
				}
				t.Fatal("missing symlink")
			})
		}
	}
}

func largeFixture(t *testing.T) (string, Options) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Fixture.app/Contents/Info.plist"), []byte(`<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.large</string><key>CFBundleExecutable</key><string>large</string><key>CFBundleShortVersionString</key><string>1.2</string><key>CFBundleVersion</key><string>12</string></dict></plist>`), 0o644)
	name := filepath.Join(root, "Fixture.app/Contents/MacOS/large")
	writeFile(t, name, []byte("synthetic executable header"), 0o755)
	f, err := os.OpenFile(name, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	const size = 65 << 20
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("preserve every trailing byte"), size-28); err != nil {
		t.Fatal(err)
	}
	return root, Options{Identifier: "org.example.large", Version: "1.2", Payload: "Fixture.app", InstallLocation: "/Applications/Fixture.app", Timestamp: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)}
}

func fileDigest(t *testing.T, filename string) [sha256.Size]byte {
	t.Helper()
	f, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	digest := sha256.New()
	if _, err := io.Copy(digest, f); err != nil {
		t.Fatal(err)
	}
	return [sha256.Size]byte(digest.Sum(nil))
}

func TestBuildLargeAppStreamsAndRemainsReproducible(t *testing.T) {
	root, opts := largeFixture(t)
	first := filepath.Join(t.TempDir(), "first.pkg")
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := Build(t.Context(), root, first, opts); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	// Payload size must not become a whole-file allocation in the writer.
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		t.Fatalf("large payload allocated %d bytes during packaging", allocated)
	}
	facts, err := apple.InspectPackageContents(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Applications) != 1 || facts.Applications[0].App.BundleID != "org.example.large" || facts.Applications[0].InstalledPath != "/Applications/Fixture.app" {
		t.Fatalf("wrong package app facts: %+v", facts.Applications)
	}
	if _, err := apple.VerifyPackage(t.Context(), first, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("large package integrity: %v", err)
	}
	second := filepath.Join(t.TempDir(), "second.pkg")
	if err := Build(t.Context(), root, second, opts); err != nil {
		t.Fatal(err)
	}
	if fileDigest(t, first) != fileDigest(t, second) {
		t.Fatal("large package is not reproducible")
	}
}

func TestScriptsOnlyPackageMetadata(t *testing.T) {
	root, opts := fixture(t)
	opts.Payload = ""
	output := filepath.Join(t.TempDir(), "out.pkg")
	if err := Build(t.Context(), root, output, opts); err != nil {
		t.Fatal(err)
	}
	facts, err := apple.InspectPackage(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range facts.Entries {
		if entry.Path == "Payload" || entry.Path == "Bom" {
			t.Fatal("scripts-only package has payload members")
		}
	}
	// The PackageInfo itself must have both hooks, independently of the Scripts member.
	body := packageMember(t, output, "PackageInfo")
	var info struct {
		Scripts *struct {
			Preinstall  *struct{} `xml:"preinstall"`
			Postinstall *struct{} `xml:"postinstall"`
		} `xml:"scripts"`
		Payload *struct{} `xml:"payload"`
	}
	if err := xml.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Scripts == nil || info.Scripts.Preinstall == nil || info.Scripts.Postinstall == nil || info.Payload != nil {
		t.Fatalf("incorrect scripts-only metadata: %s", body)
	}
}

func packageMember(t *testing.T, output, name string) []byte {
	t.Helper()
	f, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	header := make([]byte, 28)
	if _, err := io.ReadFull(f, header); err != nil {
		t.Fatal(err)
	}
	compressed := make([]byte, binary.BigEndian.Uint64(header[8:16]))
	if _, err := io.ReadFull(f, compressed); err != nil {
		t.Fatal(err)
	}
	toc, err := inflateTOC(compressed)
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		TOC struct {
			Files []struct {
				Name string `xml:"name"`
				Data struct {
					Size     int64 `xml:"size"`
					Length   int64 `xml:"length"`
					Offset   int64 `xml:"offset"`
					Encoding struct {
						Style string `xml:"style,attr"`
					} `xml:"encoding"`
				} `xml:"data"`
			} `xml:"file"`
		} `xml:"toc"`
	}
	if err := xml.Unmarshal(toc, &document); err != nil {
		t.Fatal(err)
	}
	for _, member := range document.TOC.Files {
		if member.Name == name {
			body := make([]byte, member.Data.Length)
			if _, err := f.ReadAt(body, int64(28+len(compressed))+member.Data.Offset); err != nil {
				t.Fatal(err)
			}
			if member.Data.Encoding.Style == "application/x-gzip" {
				body, err = inflateTOC(body)
				if err != nil {
					t.Fatal(err)
				}
			}
			return body
		}
	}
	t.Fatalf("missing package member %s", name)
	return nil
}

func inflateTOC(compressed []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}

func wrapperFixture(t *testing.T) (string, Options) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Scripts/postinstall"), []byte("#!/bin/sh\n# Resources remain beside this hook during installation.\nexit 93\n"), 0o644)
	writeFile(t, filepath.Join(root, "Scripts/Media.dmg"), bytes.Repeat([]byte("synthetic disk image resource\n"), MaxScriptSize/29+1), 0o640)
	writeFile(t, filepath.Join(root, "Scripts/config/defaults.plist"), []byte(`<?xml version="1.0"?><plist version="1.0"><dict><key>Enabled</key><true/></dict></plist>`), 0o640)
	writeFile(t, filepath.Join(root, "Scripts/config/preinstall"), []byte("#!/bin/sh\nexit 94\n"), 0o750)
	if err := os.Symlink("config/defaults.plist", filepath.Join(root, "Scripts/defaults.plist")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("config/preinstall", filepath.Join(root, "Scripts/preinstall")); err != nil {
		t.Fatal(err)
	}
	return root, Options{
		Identifier: "org.example.wrapper", Version: "1.2", Scripts: "Scripts",
		Timestamp: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
		ScriptMetadata: map[string]EntryMetadata{
			".":                     {Mode: new(uint32(0o750))},
			"Media.dmg":             {UID: 501, GID: 20},
			"config":                {Mode: new(uint32(0o710)), UID: 501, GID: 20},
			"config/defaults.plist": {Mode: new(uint32(0o440)), UID: 502, GID: 80},
			"defaults.plist":        {Mode: new(uint32(0o755))},
			"preinstall":            {Mode: new(uint32(0o755))},
			"postinstall":           {Mode: new(uint32(0o600))},
		},
	}
}

func TestWrapperScriptsPreserveResourcesAndDeclareOnlyRootRegularHooks(t *testing.T) {
	root, opts := wrapperFixture(t)
	output := filepath.Join(t.TempDir(), "wrapper.pkg")
	if err := Build(t.Context(), root, output, opts); err != nil {
		t.Fatal(err)
	}
	if _, err := apple.VerifyPackage(t.Context(), output, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
		t.Fatalf("wrapper package integrity: %v", err)
	}
	body := packageMember(t, output, "PackageInfo")
	var info struct {
		Scripts struct {
			Preinstall  []struct{} `xml:"preinstall"`
			Postinstall []struct {
				File string `xml:"file,attr"`
			} `xml:"postinstall"`
		} `xml:"scripts"`
		Payload *struct{} `xml:"payload"`
	}
	if err := xml.Unmarshal(body, &info); err != nil {
		t.Fatal(err)
	}
	if info.Payload != nil || len(info.Scripts.Preinstall) != 0 || len(info.Scripts.Postinstall) != 1 || info.Scripts.Postinstall[0].File != "./postinstall" {
		t.Fatalf("incorrect wrapper package metadata: %s", body)
	}
	want := map[string][3]uint32{
		".":                       {0o40750, 0, 0},
		"./Media.dmg":             {0o100640, 501, 20},
		"./config":                {0o40710, 501, 20},
		"./config/defaults.plist": {0o100440, 502, 80},
		"./config/preinstall":     {0o100750, 0, 0},
		"./defaults.plist":        {0o120755, 0, 0},
		"./preinstall":            {0o120755, 0, 0},
		"./postinstall":           {0o100755, 0, 0},
	}
	for _, name := range []string{"./Media.dmg", "./config/preinstall"} {
		info, err := os.Stat(filepath.Join(root, "Scripts", name))
		if err != nil {
			t.Fatal(err)
		}
		metadata := want[name]
		metadata[0] = 0o100000 | uint32(info.Mode().Perm())
		want[name] = metadata
	}
	var names []string
	for _, entry := range scriptEntries(t, packageMember(t, output, "Scripts")) {
		names = append(names, entry.name)
		if actual := [3]uint32{entry.mode, entry.uid, entry.gid}; actual != want[entry.name] {
			t.Fatalf("script metadata for %s: got %v, want %v", entry.name, actual, want[entry.name])
		}
		delete(want, entry.name)
		if !entry.modified.Equal(opts.Timestamp) {
			t.Fatalf("script timestamp for %s: %v", entry.name, entry.modified)
		}
		if entry.mode&0o170000 == 0o040000 {
			continue
		}
		var expected []byte
		if entry.mode&0o170000 == 0o120000 {
			link, err := os.Readlink(filepath.Join(root, "Scripts", entry.name))
			if err != nil {
				t.Fatal(err)
			}
			expected = []byte(link)
		} else {
			var err error
			expected, err = os.ReadFile(filepath.Join(root, "Scripts", entry.name))
			if err != nil {
				t.Fatal(err)
			}
		}
		if !bytes.Equal(entry.contents, expected) {
			t.Fatalf("script resource bytes differ for %s", entry.name)
		}
	}
	if len(want) != 0 || !slices.IsSorted(names) {
		t.Fatalf("missing or unordered script entries: missing %v, order %v", want, names)
	}
}

type scriptEntry struct {
	name           string
	mode, uid, gid uint32
	modified       time.Time
	contents       []byte
}

// Decode ODC independently of the package writer's CPIO dependency.
func scriptEntries(t *testing.T, compressed []byte) []scriptEntry {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	var entries []scriptEntry
	for {
		var header [76]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			t.Fatal(err)
		}
		if string(header[:6]) != "070707" {
			t.Fatal("invalid ODC header")
		}
		number := func(start, end int) uint64 {
			value, err := strconv.ParseUint(string(header[start:end]), 8, 64)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		name := make([]byte, number(59, 65))
		if _, err := io.ReadFull(r, name); err != nil || len(name) == 0 || name[len(name)-1] != 0 {
			t.Fatalf("invalid ODC name: %q %v", name, err)
		}
		if string(name[:len(name)-1]) == "TRAILER!!!" {
			return entries
		}
		entry := scriptEntry{
			name: string(name[:len(name)-1]), mode: uint32(number(18, 24)), uid: uint32(number(24, 30)), gid: uint32(number(30, 36)),
			modified: time.Unix(int64(number(48, 59)), 0), contents: make([]byte, number(65, 76)),
		}
		if _, err := io.ReadFull(r, entry.contents); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
}
