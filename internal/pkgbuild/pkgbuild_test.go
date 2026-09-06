package pkgbuild

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/internal/apple"
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
	evidence, err := apple.VerifyPackage(output, apple.Policy{RequireIntegrity: true})
	if err != nil || evidence.Integrity.Status != apple.Valid {
		t.Fatalf("package integrity: %+v: %v", evidence, err)
	}
	if _, err := apple.VerifyPackage(output, apple.Policy{RequireSignature: true}); err == nil {
		t.Fatal("unsigned package authenticated")
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
	if _, err := apple.VerifyPackage(tampered, apple.Policy{RequireIntegrity: true}); err == nil {
		t.Fatal("tampered archive passed integrity")
	}
}
func TestBuildRejectsUnsupportedOrUnsafeInputs(t *testing.T) {
	for _, name := range []string{"empty", "traversal", "script-name", "bundle", "symlink", "hardlink", "oversize", "entry-limit", "output-in-root", "output-via-symlink", "script-ancestor", "existing-output", "cancelled"} {
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
			case "bundle":
				if err := os.Mkdir(filepath.Join(root, "Payload/Fake.app"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("message.txt", filepath.Join(root, "Payload/Library/Application Support/Fixture/link")); err != nil {
					t.Skip(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(root, "Payload/Library/Application Support/Fixture/message.txt"), filepath.Join(root, "Payload/link")); err != nil {
					t.Skip(err)
				}
			case "oversize":
				f, err := os.Create(filepath.Join(root, "Payload/large"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(maxFileSize + 1); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			case "entry-limit":
				for i := range maxEntries {
					writeFile(t, filepath.Join(root, "Payload", fmt.Sprintf("file-%d", i)), nil, 0o644)
				}
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
	var document xarDocument
	if err := xml.Unmarshal(toc, &document); err != nil {
		t.Fatal(err)
	}
	for _, member := range document.TOC.Files {
		if member.Name == "PackageInfo" {
			body := make([]byte, member.Data.Size)
			if _, err := f.ReadAt(body, int64(28+len(compressed))+member.Data.Offset); err != nil {
				t.Fatal(err)
			}
			var info packageInfo
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
