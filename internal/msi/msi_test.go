package msi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMSIFixtureProperties(t *testing.T) {
	info, err := Read("testdata/test.msi")
	if err != nil {
		t.Fatal(err)
	}
	if info.ProductName != "Stemma MSI Fixture" || info.ProductCode != "{8B2D32B7-0BE9-4CF9-B1E7-42C27753A6B8}" || info.ProductVersion != "1.2.3" || info.PackageCode != "{71C6B8B7-EF12-4C0B-A390-AD3899831AFA}" || info.Manufacturer != "Woodleigh School" || info.Properties["ALLUSERS"] != "1" {
		t.Fatalf("fixture product facts differ: %+v", info)
	}
}

func TestFileVersionNamesOneVersionedFile(t *testing.T) {
	version, err := FileVersion("testdata/files.msi", "vendor.EXE")
	if err != nil || version != "7.2.1.48556" {
		t.Fatalf("versioned file: %q, %v", version, err)
	}
	for _, test := range []struct {
		msi, name, reason string
	}{
		{"testdata/files.msi", "VENDOR~1.EXE", "0 files"},
		{"testdata/files.msi", "missing.exe", "0 files"},
		{"testdata/files.msi", "shared.dll", "2 files"},
		{"testdata/files.msi", "fixture.txt", "no version"},
		{"testdata/files.msi", "helper.dll", "companion of VendorExe"},
		{"testdata/files.msi", "tool.exe", "invalid version"},
		{"testdata/test.msi", "fixture.txt", "no version"},
	} {
		t.Run(test.msi+"/"+test.name, func(t *testing.T) {
			version, err := FileVersion(test.msi, test.name)
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("FileVersion = %q, %v; want an error naming %q", version, err, test.reason)
			}
		})
	}
}

func TestMalformedCompoundMetadata(t *testing.T) {
	data, err := os.ReadFile("testdata/test.msi")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]byte){
		"bad signature":            func(data []byte) { data[0] = 0 },
		"bad sector shift":         func(data []byte) { binary.LittleEndian.PutUint16(data[30:], 63) },
		"bad byte order":           func(data []byte) { binary.LittleEndian.PutUint16(data[28:], 0) },
		"oversized DIFAT":          func(data []byte) { binary.LittleEndian.PutUint32(data[72:], 0xffffffff) },
		"invalid directory sector": func(data []byte) { binary.LittleEndian.PutUint32(data[48:], 0xffffffff) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := append([]byte(nil), data...)
			change(changed)
			path := filepath.Join(t.TempDir(), "invalid.msi")
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(path); err == nil {
				t.Fatal("accepted malformed MSI")
			}
		})
	}
}

func TestStringCodePages(t *testing.T) {
	value, _, err := readStringData([]byte{0x93, 'H', 'i', 0x94}, 0, 4, 1252)
	if err != nil || value != "\u201cHi\u201d" {
		t.Fatalf("Windows-1252: %q, %v", value, err)
	}
	for _, codepage := range []int{0, 65001, 1200} {
		if _, _, err := readStringData([]byte{0xff}, 0, 1, codepage); err == nil {
			t.Fatalf("accepted invalid codepage %d bytes", codepage)
		}
	}
}

func TestSectorChainLimits(t *testing.T) {
	read := func(uint32, []byte) error { return nil }
	if _, err := readChain(0, 1<<20, 512, 512, func(uint32) (uint32, error) { return cfbEndOfChain, nil }, read); err == nil {
		t.Fatal("accepted stream larger than backing file")
	}
	if _, err := readChain(0, -1, 512, 1<<20, func(uint32) (uint32, error) { return 0, nil }, read); err == nil {
		t.Fatal("accepted cyclic sector chain")
	}
}

// A large installer's cabinets are never read, so its size does not bound
// inspection.
func TestReadIgnoresCabinetSize(t *testing.T) {
	data, err := os.ReadFile("testdata/test.msi")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "large.msi")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Truncate(int64(len(data)) + 1<<30)
	}
	if err := errors.Join(err, f.Close()); err != nil {
		t.Fatal(err)
	}
	info, err := Read(path)
	if err != nil || info.ProductVersion != "1.2.3" {
		t.Fatalf("large MSI: %+v %v", info, err)
	}
}

func TestProductIconReadsTheIconTableStream(t *testing.T) {
	want, err := os.ReadFile("testdata/icon.ico")
	if err != nil {
		t.Fatal(err)
	}
	data, ok, err := ProductIcon("testdata/icon.msi")
	if err != nil || !ok || !bytes.Equal(data, want) {
		t.Fatalf("product icon: ok=%v %d bytes %v", ok, len(data), err)
	}
	if _, ok, err := ProductIcon("testdata/test.msi"); err != nil || ok {
		t.Fatalf("installer without ARPPRODUCTICON: ok=%v %v", ok, err)
	}
}
