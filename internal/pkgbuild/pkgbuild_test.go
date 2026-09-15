package pkgbuild

import (
	"bytes"
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
	return root, Options{Identifier: "au.edu.vic.woodleigh.stemma.local-fixture", Version: "1.2.3", Timestamp: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC), Payload: "Payload", Scripts: map[string]string{"preinstall": "Scripts/preinstall", "postinstall": "Scripts/postinstall"}}
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
	for _, name := range []string{"empty", "traversal", "script-name", "symlink", "hardlink", "oversize", "total-size", "output-in-root", "output-via-symlink", "script-ancestor", "existing-output", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			root, opts := fixture(t)
			output := filepath.Join(t.TempDir(), "out.pkg")
			ctx := t.Context()
			switch name {
			case "empty":
				opts.Payload = ""
				opts.Scripts = nil
			case "traversal":
				opts.Scripts["preinstall"] = "../outside"
			case "script-name":
				opts.Scripts["prepare"] = "Scripts/preinstall"
			case "symlink":
				if err := os.Symlink("../../../../../outside", filepath.Join(root, "Payload/Library/Application Support/Fixture/link")); err != nil {
					t.Skip(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(root, "Payload/Library/Application Support/Fixture/message.txt"), filepath.Join(root, "Payload/link")); err != nil {
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
				opts.Scripts["preinstall"] = "ScriptsAlias/preinstall"
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
		if member.Name == "PackageInfo" {
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
	}
}

func inflateTOC(compressed []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	return io.ReadAll(r)
}
