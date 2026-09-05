// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

// Independent Go test port of WrapTune-MacOS tools/verify-intunewin.py.
package intunewin_test

import (
	"archive/zip"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/intunewin"
)

// Duplicating the documented wire layout here is deliberate: importing the
// writer's constants or parsers would let the same mistake validate itself.
const (
	oracleContentPath  = "IntuneWinPackage/Contents/IntunePackage.intunewin"
	oracleMetadataPath = "IntuneWinPackage/Metadata/Detection.xml"
	oracleReadLimit    = 64 << 20
)

type verifiedPayload struct {
	setup string
	files map[string][]byte
}

func TestIndependentVerifier(t *testing.T) {
	source, expected := oracleSource(t)
	for range 2 {
		output := filepath.Join(t.TempDir(), "fixture.intunewin")
		if _, err := intunewin.Write(t.Context(), source, "install/setup.cmd", output); err != nil {
			t.Fatal(err)
		}
		verified, err := verifyEnvelope(output)
		if err != nil {
			t.Fatalf("independent verification: %v", err)
		}
		if verified.setup != "install/setup.cmd" || len(verified.files) != len(expected) {
			t.Fatalf("recovered setup=%q files=%d", verified.setup, len(verified.files))
		}
		for name, want := range expected {
			if !bytes.Equal(verified.files[name], want) {
				t.Errorf("recovered %s differs from source", name)
			}
		}
	}
}

func TestIndependentVerifierRejectsCorruption(t *testing.T) {
	source, _ := oracleSource(t)
	original := filepath.Join(t.TempDir(), "original.intunewin")
	if _, err := intunewin.Write(t.Context(), source, "install/setup.cmd", original); err != nil {
		t.Fatal(err)
	}
	originalEntries, err := oracleReadOuter(original)
	if err != nil {
		t.Fatal(err)
	}
	originalFields, err := oracleReadXML(originalEntries[oracleMetadataPath])
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string][]byte)
		want   string
	}{
		{"ciphertext", func(entries map[string][]byte) { entries[oracleContentPath][55] ^= 1 }, "HMAC"},
		{"header-mac", func(entries map[string][]byte) { entries[oracleContentPath][0] ^= 1 }, "header MAC"},
		{"xml-iv", func(entries map[string][]byte) {
			oracleReplaceXML(entries, "InitializationVector", base64.StdEncoding.EncodeToString(make([]byte, 16)))
		}, "header IV"},
		{"encryption-key-length", func(entries map[string][]byte) {
			oracleReplaceXML(entries, "EncryptionKey", base64.StdEncoding.EncodeToString(make([]byte, 16)))
		}, "EncryptionKey"},
		{"mac-key-length", func(entries map[string][]byte) {
			oracleReplaceXML(entries, "MacKey", base64.StdEncoding.EncodeToString(make([]byte, 16)))
		}, "MacKey"},
		{"base64", func(entries map[string][]byte) { oracleReplaceXML(entries, "MacKey", "not valid base64!") }, "MacKey"},
		{"identical-keys", func(entries map[string][]byte) {
			oracleReplaceXML(entries, "MacKey", originalFields["EncryptionInfo/EncryptionKey"])
		}, "distinct"},
		{"truncated-header", func(entries map[string][]byte) { entries[oracleContentPath] = entries[oracleContentPath][:47] }, "layout"},
		{"partial-cipher-block", func(entries map[string][]byte) {
			blob := entries[oracleContentPath]
			entries[oracleContentPath] = blob[:len(blob)-1]
		}, "layout"},
		{"digest", func(entries map[string][]byte) {
			oracleReplaceXML(entries, "FileDigest", base64.StdEncoding.EncodeToString(make([]byte, 32)))
		}, "digest"},
		{"plaintext-size", func(entries map[string][]byte) { oracleReplaceXML(entries, "UnencryptedContentSize", "1") }, "size"},
		{"setup-file", func(entries map[string][]byte) { oracleReplaceXML(entries, "SetupFile", "missing.exe") }, "SetupFile"},
		{"profile", func(entries map[string][]byte) { oracleReplaceXML(entries, "ProfileIdentifier", "ProfileVersion2") }, "profile"},
		{"digest-algorithm", func(entries map[string][]byte) { oracleReplaceXML(entries, "FileDigestAlgorithm", "SHA1") }, "algorithm"},
		{"file-name", func(entries map[string][]byte) { oracleReplaceXML(entries, "FileName", "other.intunewin") }, "FileName"},
		{"missing-content", func(entries map[string][]byte) { delete(entries, oracleContentPath) }, "outer"},
		{"trailing-xml", func(entries map[string][]byte) {
			entries[oracleMetadataPath] = append(entries[oracleMetadataPath], []byte("<Other/>")...)
		}, "XML"},
		{"duplicate-xml-field", func(entries map[string][]byte) {
			entries[oracleMetadataPath] = bytes.Replace(entries[oracleMetadataPath], []byte("</SetupFile>"), []byte("</SetupFile><SetupFile>other.exe</SetupFile>"), 1)
		}, "duplicate"},
		{"invalid-padding", func(entries map[string][]byte) {
			blob := entries[oracleContentPath]
			key, _ := base64.StdEncoding.DecodeString(originalFields["EncryptionInfo/EncryptionKey"])
			block, _ := aes.NewCipher(key)
			padded := make([]byte, len(blob)-48)
			cipher.NewCBCDecrypter(block, blob[32:48]).CryptBlocks(padded, blob[48:])
			// Set the last plaintext octet to zero through CBC's preceding block.
			// Re-authentication forces the verifier to reach its padding check.
			blob[len(blob)-17] ^= padded[len(padded)-1]
			oracleAuthenticate(entries, originalFields)
		}, "PKCS7"},
		{"invalid-padding-octets", func(entries map[string][]byte) {
			oracleReseal(entries, originalFields, bytes.Repeat([]byte("p"), 24))
			blob := entries[oracleContentPath]
			blob[len(blob)-18] ^= 1
			oracleAuthenticate(entries, originalFields)
		}, "padding octets"},
		{"not-an-inner-zip", func(entries map[string][]byte) {
			oracleReseal(entries, originalFields, []byte("not an installer payload archive"))
		}, "inner ZIP"},
		{"duplicate-inner-entry", func(entries map[string][]byte) {
			oracleReseal(entries, originalFields, oracleInnerZip(t, []string{"install/setup.cmd", "install/setup.cmd"}))
		}, "duplicate"},
		{"unsafe-inner-path", func(entries map[string][]byte) {
			oracleReseal(entries, originalFields, oracleInnerZip(t, []string{"install/setup.cmd", "../outside"}))
		}, "unsafe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			entries := make(map[string][]byte, len(originalEntries))
			for name, data := range originalEntries {
				entries[name] = bytes.Clone(data)
			}
			test.mutate(entries)
			output := filepath.Join(t.TempDir(), "corrupt.intunewin")
			oracleWriteOuter(t, output, entries, false)
			if _, err := verifyEnvelope(output); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q failure", err, test.want)
			}
		})
	}
	t.Run("duplicate-outer-entry", func(t *testing.T) {
		output := filepath.Join(t.TempDir(), "duplicate.intunewin")
		oracleWriteOuter(t, output, originalEntries, true)
		if _, err := verifyEnvelope(output); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("duplicate metadata: %v", err)
		}
	})
}

// verifyEnvelope only consumes emitted bytes. The production package is used by
// the tests to generate inputs, never to interpret or authenticate their output.
func verifyEnvelope(filename string) (verifiedPayload, error) {
	entries, err := oracleReadOuter(filename)
	if err != nil {
		return verifiedPayload{}, err
	}
	fields, err := oracleReadXML(entries[oracleMetadataPath])
	if err != nil {
		return verifiedPayload{}, err
	}
	if fields["@ToolVersion"] == "" || fields["Name"] == "" {
		return verifiedPayload{}, errors.New("XML requires ToolVersion and Name")
	}
	if fields["FileName"] != "IntunePackage.intunewin" {
		return verifiedPayload{}, errors.New("unexpected FileName")
	}
	if fields["EncryptionInfo/ProfileIdentifier"] != "ProfileVersion1" {
		return verifiedPayload{}, errors.New("unsupported encryption profile")
	}
	if fields["EncryptionInfo/FileDigestAlgorithm"] != "SHA256" {
		return verifiedPayload{}, errors.New("unsupported digest algorithm")
	}
	material := map[string][]byte{}
	for name, size := range map[string]int{"EncryptionKey": 32, "MacKey": 32, "InitializationVector": 16, "Mac": 32, "FileDigest": 32} {
		text := fields["EncryptionInfo/"+name]
		decoded, err := base64.StdEncoding.Strict().DecodeString(text)
		if err != nil || len(decoded) != size || strings.ContainsAny(text, "\r\n\t ") {
			return verifiedPayload{}, fmt.Errorf("invalid %s key material", name)
		}
		material[name] = decoded
	}
	if bytes.Equal(material["EncryptionKey"], material["MacKey"]) {
		return verifiedPayload{}, errors.New("encryption and MAC keys must be distinct")
	}
	blob := entries[oracleContentPath]
	if len(blob) < 64 || (len(blob)-48)%16 != 0 {
		return verifiedPayload{}, errors.New("invalid encrypted blob layout")
	}
	if !hmac.Equal(blob[:32], material["Mac"]) {
		return verifiedPayload{}, errors.New("header MAC disagrees with metadata")
	}
	if !bytes.Equal(blob[32:48], material["InitializationVector"]) {
		return verifiedPayload{}, errors.New("header IV disagrees with metadata")
	}
	mac := hmac.New(sha256.New, material["MacKey"])
	_, _ = mac.Write(blob[32:])
	if !hmac.Equal(mac.Sum(nil), material["Mac"]) {
		return verifiedPayload{}, errors.New("HMAC authentication failed")
	}
	block, err := aes.NewCipher(material["EncryptionKey"])
	if err != nil {
		return verifiedPayload{}, fmt.Errorf("AES256 key: %w", err)
	}
	padded := make([]byte, len(blob)-48)
	cipher.NewCBCDecrypter(block, material["InitializationVector"]).CryptBlocks(padded, blob[48:])
	padding := int(padded[len(padded)-1])
	if padding < 1 || padding > 16 || padding > len(padded) {
		return verifiedPayload{}, errors.New("invalid PKCS7 padding length")
	}
	for _, value := range padded[len(padded)-padding:] {
		if int(value) != padding {
			return verifiedPayload{}, errors.New("invalid PKCS7 padding octets")
		}
	}
	plain := padded[:len(padded)-padding]
	size, err := strconv.ParseInt(fields["UnencryptedContentSize"], 10, 64)
	if err != nil || size <= 0 || int64(len(plain)) != size {
		return verifiedPayload{}, errors.New("plaintext size disagrees with metadata")
	}
	digest := sha256.Sum256(plain)
	if !hmac.Equal(digest[:], material["FileDigest"]) {
		return verifiedPayload{}, errors.New("plaintext digest disagrees with metadata")
	}
	payload, err := zip.NewReader(bytes.NewReader(plain), int64(len(plain)))
	if err != nil {
		return verifiedPayload{}, fmt.Errorf("invalid inner ZIP: %w", err)
	}
	result := verifiedPayload{setup: strings.ReplaceAll(fields["SetupFile"], "\\", "/"), files: map[string][]byte{}}
	if result.setup == "" {
		return verifiedPayload{}, errors.New("missing SetupFile")
	}
	seen := map[string]bool{}
	var total int
	for _, entry := range payload.File {
		if seen[entry.Name] {
			return verifiedPayload{}, errors.New("duplicate inner ZIP entry")
		}
		seen[entry.Name] = true
		if path.IsAbs(entry.Name) || strings.Contains(entry.Name, "\\") || path.Clean(entry.Name) == ".." || strings.HasPrefix(path.Clean(entry.Name), "../") || strings.Contains(entry.Name, ":") {
			return verifiedPayload{}, errors.New("unsafe inner ZIP path")
		}
		if entry.FileInfo().IsDir() {
			continue
		}
		if !entry.Mode().IsRegular() {
			return verifiedPayload{}, errors.New("unsupported inner ZIP file type")
		}
		data, err := oracleReadEntry(entry)
		if err != nil {
			return verifiedPayload{}, fmt.Errorf("inner ZIP content: %w", err)
		}
		total += len(data)
		if total > oracleReadLimit {
			return verifiedPayload{}, errors.New("inner ZIP exceeds test limit")
		}
		result.files[entry.Name] = data
	}
	if _, exists := result.files[result.setup]; !exists {
		return verifiedPayload{}, errors.New("SetupFile is absent from inner ZIP")
	}
	if strings.HasSuffix(strings.ToLower(result.setup), ".msi") {
		if product, exists := fields["MsiInfo/MsiProductCode"]; exists && product == "" {
			return verifiedPayload{}, errors.New("MSI metadata contains an empty product code")
		}
	}
	return result, nil
}

func oracleReadOuter(filename string) (map[string][]byte, error) {
	archive, err := zip.OpenReader(filename)
	if err != nil {
		return nil, fmt.Errorf("outer ZIP: %w", err)
	}
	defer func() { _ = archive.Close() }()
	entries := map[string][]byte{}
	for _, entry := range archive.File {
		if _, exists := entries[entry.Name]; exists {
			return nil, errors.New("duplicate outer ZIP entry")
		}
		if entry.Name != oracleContentPath && entry.Name != oracleMetadataPath {
			return nil, errors.New("unexpected outer ZIP entry")
		}
		content, err := oracleReadEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("outer ZIP entry: %w", err)
		}
		entries[entry.Name] = content
	}
	if len(entries) != 2 || len(entries[oracleContentPath]) == 0 || len(entries[oracleMetadataPath]) == 0 {
		return nil, errors.New("missing required outer ZIP entries")
	}
	return entries, nil
}

func oracleReadEntry(entry *zip.File) ([]byte, error) {
	if entry.UncompressedSize64 > oracleReadLimit {
		return nil, errors.New("entry exceeds test limit")
	}
	reader, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(io.LimitReader(reader, oracleReadLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > oracleReadLimit {
		return nil, errors.New("entry exceeds test limit")
	}
	return data, nil
}

// Token-based parsing is intentionally separate from production's XML models.
func oracleReadXML(data []byte) (map[string]string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	fields := map[string]string{}
	seen := map[string]bool{}
	var stack []string
	rootSeen, rootClosed := false, false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if rootClosed {
				return nil, errors.New("XML contains trailing elements")
			}
			if len(stack) == 0 {
				if rootSeen || value.Name.Local != "ApplicationInfo" || value.Name.Space != "" {
					return nil, errors.New("XML root must be ApplicationInfo")
				}
				rootSeen = true
				for _, attribute := range value.Attr {
					if attribute.Name.Local == "ToolVersion" {
						fields["@ToolVersion"] = attribute.Value
					}
				}
			}
			stack = append(stack, value.Name.Local)
			if len(stack) > 4 {
				return nil, errors.New("XML exceeds supported nesting")
			}
			key := strings.Join(stack[1:], "/")
			if seen[key] {
				return nil, errors.New("duplicate XML element")
			}
			seen[key] = true
			fields[key] = ""
		case xml.CharData:
			text := strings.TrimSpace(string(value))
			if len(stack) == 0 {
				if text != "" {
					return nil, errors.New("XML has text outside its root")
				}
				continue
			}
			fields[strings.Join(stack[1:], "/")] += string(value)
		case xml.EndElement:
			if len(stack) == 0 {
				return nil, errors.New("XML has an unexpected end element")
			}
			key := strings.Join(stack[1:], "/")
			fields[key] = strings.TrimSpace(fields[key])
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				rootClosed = true
			}
		case xml.Directive:
			return nil, errors.New("unsupported XML directive")
		}
	}
	if !rootSeen || !rootClosed || len(stack) != 0 {
		return nil, errors.New("incomplete XML document")
	}
	return fields, nil
}

func oracleSource(t *testing.T) (string, map[string][]byte) {
	t.Helper()
	root := t.TempDir()
	contents := map[string][]byte{"install/setup.cmd": []byte("@echo off\r\necho Synthetic fixture\r\n"), "assets/config.txt": bytes.Repeat([]byte("setting=value\n"), 128), "empty.dat": {}}
	for name, data := range contents {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, contents
}

func oracleReplaceXML(entries map[string][]byte, name, value string) {
	data := entries[oracleMetadataPath]
	start := bytes.Index(data, []byte("<"+name+">")) + len(name) + 2
	end := bytes.Index(data, []byte("</"+name+">"))
	if start < len(name)+2 || end < start {
		panic("test mutation names an absent XML field")
	}
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	entries[oracleMetadataPath] = append(append(bytes.Clone(data[:start]), escaped.Bytes()...), data[end:]...)
}

func oracleAuthenticate(entries map[string][]byte, fields map[string]string) {
	macKey, _ := base64.StdEncoding.DecodeString(fields["EncryptionInfo/MacKey"])
	authentication := hmac.New(sha256.New, macKey)
	_, _ = authentication.Write(entries[oracleContentPath][32:])
	mac := authentication.Sum(nil)
	copy(entries[oracleContentPath][:32], mac)
	oracleReplaceXML(entries, "Mac", base64.StdEncoding.EncodeToString(mac))
}

func oracleReseal(entries map[string][]byte, fields map[string]string, plaintext []byte) {
	key, _ := base64.StdEncoding.DecodeString(fields["EncryptionInfo/EncryptionKey"])
	iv, _ := base64.StdEncoding.DecodeString(fields["EncryptionInfo/InitializationVector"])
	block, _ := aes.NewCipher(key)
	padding := 16 - len(plaintext)%16
	padded := append(bytes.Clone(plaintext), bytes.Repeat([]byte{byte(padding)}, padding)...)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	entries[oracleContentPath] = append(append(make([]byte, 32), iv...), ciphertext...)
	digest := sha256.Sum256(plaintext)
	oracleReplaceXML(entries, "FileDigest", base64.StdEncoding.EncodeToString(digest[:]))
	oracleReplaceXML(entries, "UnencryptedContentSize", strconv.Itoa(len(plaintext)))
	oracleAuthenticate(entries, fields)
}

func oracleInnerZip(t *testing.T, names []string) []byte {
	t.Helper()
	var data bytes.Buffer
	archive := zip.NewWriter(&data)
	for _, name := range names {
		file, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(file, "synthetic fixture"); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func oracleWriteOuter(t *testing.T, filename string, entries map[string][]byte, duplicate bool) {
	t.Helper()
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	archive := zip.NewWriter(file)
	for _, name := range []string{oracleContentPath, oracleMetadataPath, oracleMetadataPath} {
		content, exists := entries[name]
		if !exists {
			continue
		}
		writer, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(content); err != nil {
			t.Fatal(err)
		}
		if name == oracleMetadataPath && !duplicate {
			break
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
}
