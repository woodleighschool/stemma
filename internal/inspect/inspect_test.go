package inspect

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/plugin"
)

func TestReadKeepsPackageAndApplicationFactsSeparate(t *testing.T) {
	data := readFixture(t, "../apple/testdata/fixture.pkg")
	name := filepath.Join(t.TempDir(), "vendor-download")
	writeFixture(t, name, data)
	facts, err := Read(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if facts.Version != plugin.FactsVersion || len(facts.Subjects) != 3 {
		t.Fatalf("wrong subjects: %+v", facts)
	}
	root, receipt, app := facts.Subjects[0], facts.Subjects[1], facts.Subjects[2]
	digest := sha256.Sum256(data)
	if root.ID != "." || root.Kind != "container" || root.SHA256 != hex.EncodeToString(digest[:]) || root.App != nil || root.Package != nil {
		t.Fatalf("lost installer identity: %+v", root)
	}
	if receipt.Package == nil || receipt.Parent != root.ID || receipt.Path != "PackageInfo" || receipt.Package.Version != "1.2.3" || !receipt.Package.HasPayload || receipt.Package.InstalledSize == 0 {
		t.Fatalf("lost receipt provenance: %+v", receipt)
	}
	if app.App == nil || app.Parent != receipt.ID || app.Path != "Payload/SignedFixture.app" || app.InstalledPath != "/Applications/SignedFixture.app" || app.App.Version != "1.2.3" || app.App.Build != "42" || app.SHA256 != "" {
		t.Fatalf("lost application provenance: %+v", app)
	}
	if !bytes.Equal(data, readFixture(t, name)) {
		t.Fatal("inspection altered installer")
	}
	other := filepath.Join(t.TempDir(), "different-name.pkg")
	writeFixture(t, other, data)
	renamed, err := Read(t.Context(), other)
	if err != nil || !reflect.DeepEqual(facts, renamed) {
		t.Fatalf("subject identity depends on temporary name: %+v, %v", renamed, err)
	}
}

func TestReadMetadataDoesNotScanPayload(t *testing.T) {
	data := readFixture(t, "../apple/testdata/fixture.pkg")
	tocSize := binary.BigEndian.Uint64(data[8:16])
	zr, err := zlib.NewReader(bytes.NewReader(data[28 : 28+tocSize]))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	var archive struct {
		Files []struct {
			Name   string `xml:"name"`
			Offset int    `xml:"data>offset"`
		} `xml:"toc>file"`
	}
	if err := xml.NewDecoder(zr).Decode(&archive); err != nil {
		t.Fatal(err)
	}
	mutated := false
	for _, file := range archive.Files {
		if file.Name == "Payload" {
			data[28+int(tocSize)+file.Offset] ^= 1
			mutated = true
		}
	}
	if !mutated {
		t.Fatal("fixture has no payload")
	}
	name := filepath.Join(t.TempDir(), "vendor.pkg")
	writeFixture(t, name, data)
	facts, err := ReadMetadata(t.Context(), name)
	if err != nil || len(facts.Subjects) != 2 || facts.Subjects[1].Package == nil {
		t.Fatalf("metadata read scanned payload: %+v, %v", facts, err)
	}
	if _, err := Read(t.Context(), name); err == nil {
		t.Fatal("full inspection accepted corrupt payload")
	}
}

func TestReadAppAndMSIByContents(t *testing.T) {
	t.Run("app", func(t *testing.T) {
		name := filepath.Join(t.TempDir(), "renamed-bundle")
		if err := os.CopyFS(name, os.DirFS("../apple/testdata/Fixture.app")); err != nil {
			t.Fatal(err)
		}
		facts, err := Read(t.Context(), name)
		if err != nil || len(facts.Subjects) != 1 || facts.Subjects[0].App == nil || facts.Subjects[0].App.Build != "42" || facts.Subjects[0].InstalledPath != "" {
			t.Fatalf("wrong app facts: %+v, %v", facts, err)
		}
	})
	t.Run("msi", func(t *testing.T) {
		name := filepath.Join(t.TempDir(), "download.bin")
		writeFixture(t, name, readFixture(t, "../msi/testdata/test.msi"))
		facts, err := Read(t.Context(), name)
		if err != nil || len(facts.Subjects) != 1 || facts.Subjects[0].MSI == nil {
			t.Fatalf("wrong MSI facts: %+v, %v", facts, err)
		}
		if facts.Subjects[0].MSI.ProductVersion != "1.2.3" || facts.Subjects[0].MSI.PackageCode == "" || facts.Subjects[0].MSI.UpgradeCode == "" {
			t.Fatalf("lost MSI properties: %+v", facts.Subjects[0].MSI)
		}
	})
}

func TestReadRejectsMalformedClaimsAndReportsUnsupportedDMG(t *testing.T) {
	for _, extension := range []string{".pkg", ".msi", ".app", ".dmg"} {
		t.Run(extension, func(t *testing.T) {
			name := filepath.Join(t.TempDir(), "malformed"+extension)
			writeFixture(t, name, []byte("not an installer"))
			for _, read := range []func(context.Context, string) (plugin.Facts, error){Read, ReadMetadata} {
				if _, err := read(t.Context(), name); err == nil {
					t.Fatal("accepted claimed malformed artifact")
				}
			}
		})
	}
	name := filepath.Join(t.TempDir(), "disk.bin")
	data := make([]byte, 1024)
	copy(data[512:], "koly")
	writeFixture(t, name, data)
	if _, err := Read(t.Context(), name); !errors.Is(err, apple.ErrUnsupported) {
		t.Fatalf("DMG inspection not marked unsupported: %v", err)
	}
	facts, err := ReadMetadata(t.Context(), name)
	if err != nil || len(facts.Subjects) != 1 || facts.Subjects[0].Kind != "container" || facts.Subjects[0].App != nil {
		t.Fatalf("wrong shallow DMG facts: %+v, %v", facts, err)
	}
}

func TestReadLocalTreeAndVersionlessFile(t *testing.T) {
	root := t.TempDir()
	writeFixture(t, filepath.Join(root, "postinstall"), []byte("#!/bin/sh\nexit 0\n"))
	if err := os.CopyFS(filepath.Join(root, "Payload/Fixture.app"), os.DirFS("../apple/testdata/Fixture.app")); err != nil {
		t.Fatal(err)
	}
	facts, err := Read(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Subjects) != 4 {
		t.Fatalf("wrong local subjects: %+v", facts)
	}
	app, script := facts.Subjects[2], facts.Subjects[3]
	if app.App == nil || app.Parent != "Payload" || app.Path != "Payload/Fixture.app" || app.InstalledPath != "" {
		t.Fatalf("local path became installed path: %+v", app)
	}
	if script.Kind != "file" || script.App != nil || script.Package != nil || script.MSI != nil || len(script.SHA256) != 64 {
		t.Fatalf("script facts invented metadata: %+v", script)
	}
	shallow, err := ReadMetadata(t.Context(), root)
	if err != nil || len(shallow.Subjects) != 1 {
		t.Fatalf("shallow read scanned tree: %+v, %v", shallow, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Read(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFixture(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
}
