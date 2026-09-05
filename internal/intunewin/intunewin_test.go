package intunewin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPayloadIdentityAndRandomEnvelope(t *testing.T) {
	source := sourceFixture(t)
	one := filepath.Join(t.TempDir(), "one.intunewin")
	m1, err := Write(t.Context(), source, "setup.cmd", one)
	if err != nil {
		t.Fatal(err)
	}
	changed := time.Date(2025, 4, 3, 2, 1, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(source, "setup.cmd"), changed, changed); err != nil {
		t.Fatal(err)
	}
	two := filepath.Join(t.TempDir(), "two.intunewin")
	m2, err := Write(t.Context(), source, "setup.cmd", two)
	if err != nil {
		t.Fatal(err)
	}
	if m1.PayloadSHA256 != m2.PayloadSHA256 || m1.EncryptionInfo.FileDigest != m2.EncryptionInfo.FileDigest {
		t.Fatal("timestamps changed payload identity")
	}
	if m1.EncryptionInfo.EncryptionKey == m2.EncryptionInfo.EncryptionKey || m1.EncryptionInfo.MacKey == m2.EncryptionInfo.MacKey || m1.EncryptionInfo.InitializationVector == m2.EncryptionInfo.InitializationVector {
		t.Fatal("reused random encryption material")
	}
	verified, err := Inspect(one)
	if err != nil {
		t.Fatal(err)
	}
	if verified != m1 {
		t.Fatalf("metadata differs: %+v / %+v", verified, m1)
	}
	destination := filepath.Join(t.TempDir(), "recovered")
	if err := Extract(t.Context(), one, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"setup.cmd", "data/config.txt"} {
		want := readFile(t, filepath.Join(source, name))
		got := readFile(t, filepath.Join(destination, name))
		if !bytes.Equal(got, want) {
			t.Fatalf("recovered %s differs", name)
		}
	}
	if err := Extract(t.Context(), one, destination); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination: %v", err)
	}
}

func TestTamperedEnvelopeIsRejected(t *testing.T) {
	original := filepath.Join(t.TempDir(), "original.intunewin")
	if _, err := Write(t.Context(), sourceFixture(t), "setup.cmd", original); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"ciphertext", "xml-iv", "digest", "profile", "algorithm", "setup", "size", "key-size", "duplicate-metadata", "trailing-xml"} {
		t.Run(kind, func(t *testing.T) {
			entries := readEnvelope(t, original)
			var d detection
			if err := xml.Unmarshal(entries[metadataEntry], &d); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "ciphertext":
				entries[contentEntry][50] ^= 1
			case "xml-iv":
				d.EncryptionInfo.InitializationVector = base64.StdEncoding.EncodeToString(make([]byte, 16))
			case "digest":
				d.EncryptionInfo.FileDigest = base64.StdEncoding.EncodeToString(make([]byte, 32))
			case "profile":
				d.EncryptionInfo.ProfileIdentifier = "ProfileVersion2"
			case "algorithm":
				d.EncryptionInfo.FileDigestAlgorithm = "SHA1"
			case "setup":
				d.SetupFile = "missing.exe"
			case "size":
				d.Size++
			case "key-size":
				d.EncryptionInfo.EncryptionKey = base64.StdEncoding.EncodeToString(make([]byte, 16))
			}
			data, err := xml.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			entries[metadataEntry] = data
			if kind == "trailing-xml" {
				entries[metadataEntry] = append(data, []byte("<other/>")...)
			}
			output := filepath.Join(t.TempDir(), "bad.intunewin")
			writeEnvelope(t, output, entries, kind == "duplicate-metadata")
			if _, err := Inspect(output); err == nil {
				t.Fatal("accepted malformed envelope")
			}
			destination := filepath.Join(t.TempDir(), "out")
			if err := Extract(t.Context(), output, destination); err == nil {
				t.Fatal("extracted malformed envelope")
			}
			if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("published failed extraction: %v", err)
			}
		})
	}
}

func TestUnsafePayloadsAreRejected(t *testing.T) {
	cases := [][]string{
		{"../escape"}, {"/absolute"}, {`C:\escape`}, {"NUL.txt"}, {"name:stream"}, {"trailing."},
		{"setup.cmd", "setup.cmd"}, {"data/a", "Data/b"}, {"data/a", "data"}, {"data", "data/a"},
		{"SETUP.cmd", "setup.cmd"}, {"missing.cmd"}, {"caf\u00e9.txt"},
	}
	for _, names := range cases {
		t.Run(strings.Join(names, ","), func(t *testing.T) {
			output := packageEntries(t, names, false)
			if _, err := Inspect(output); err == nil {
				t.Fatalf("accepted paths %v", names)
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		if _, err := Inspect(packageEntries(t, []string{"setup.cmd"}, true)); err == nil {
			t.Fatal("accepted archive symlink")
		}
	})
}

func TestWriteRefusesUnsafeSourceAndExistingOutput(t *testing.T) {
	source := sourceFixture(t)
	output := filepath.Join(t.TempDir(), "out.intunewin")
	if _, err := Write(t.Context(), source, "../setup.cmd", output); err == nil {
		t.Fatal("accepted setup outside source")
	}
	if _, err := Write(t.Context(), source, "setup.cmd", filepath.Join(source, "out.intunewin")); err == nil {
		t.Fatal("accepted output inside source")
	}
	if err := os.WriteFile(output, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(t.Context(), source, "setup.cmd", output); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing output: %v", err)
	}
	if string(readFile(t, output)) != "existing" {
		t.Fatal("overwrote existing output")
	}
	if err := os.Symlink(filepath.Join(source, "setup.cmd"), filepath.Join(source, "link.cmd")); err != nil {
		t.Skipf("cannot create source symlink: %v", err)
	}
	if _, err := Write(t.Context(), source, "setup.cmd", filepath.Join(t.TempDir(), "out.intunewin")); err == nil {
		t.Fatal("accepted source symlink")
	}
}

func TestCanceledWritePublishesNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dir := t.TempDir()
	if _, err := Write(ctx, sourceFixture(t), "setup.cmd", filepath.Join(dir, "out.intunewin")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("output remains after cancellation: %v, %v", entries, err)
	}
}

func TestZipDirectoryLimits(t *testing.T) {
	data := make([]byte, 22)
	binary.LittleEndian.PutUint32(data, 0x06054b50)
	binary.LittleEndian.PutUint16(data[8:], 65535)
	binary.LittleEndian.PutUint16(data[10:], 65535)
	if err := checkZipDirectory(bytes.NewReader(data), int64(len(data))); err == nil {
		t.Fatal("accepted excess entry count")
	}
}

func sourceFixture(t *testing.T) string {
	t.Helper()
	source := t.TempDir()
	if err := os.Mkdir(filepath.Join(source, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "setup.cmd"), []byte("@echo off\r\necho fixture\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "data", "config.txt"), bytes.Repeat([]byte("configuration\n"), 10000), 0o644); err != nil {
		t.Fatal(err)
	}
	return source
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readEnvelope(t *testing.T, path string) map[string][]byte {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = zr.Close() }()
	result := map[string][]byte{}
	for _, entry := range zr.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name] = data
	}
	return result
}

func writeEnvelope(t *testing.T, path string, entries map[string][]byte, duplicate bool) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	zw := zip.NewWriter(file)
	names := []string{contentEntry, metadataEntry}
	if duplicate {
		names = append(names, metadataEntry)
	}
	for _, name := range names {
		writer, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(entries[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func packageEntries(t *testing.T, names []string, symlink bool) string {
	t.Helper()
	dir := t.TempDir()
	plain, err := os.Create(filepath.Join(dir, "plain"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = plain.Close() }()
	zw := zip.NewWriter(plain)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		if symlink {
			header.SetMode(os.ModeSymlink | 0o777)
		}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(writer, "fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	encrypted, err := os.Create(filepath.Join(dir, "encrypted"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = encrypted.Close() }()
	m := Metadata{Name: "setup.cmd", SetupFile: "setup.cmd"}
	if err := encrypt(t.Context(), plain, encrypted, &m); err != nil {
		t.Fatal(err)
	}
	data, err := marshalDetection(m)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "out.intunewin")
	file, err := os.Create(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := outerZip(t.Context(), file, encrypted, data); err != nil {
		t.Fatal(err)
	}
	return output
}
