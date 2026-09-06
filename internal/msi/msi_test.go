package msi

import (
	"encoding/binary"
	"os"
	"path/filepath"
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
	if _, err := readChain(0, 1<<30, 512, []uint32{cfbEndOfChain}, func(uint32) (int, error) { return 0, nil }, make([]byte, 512)); err == nil {
		t.Fatal("accepted stream larger than backing file")
	}
	if _, err := readChain(0, -1, 512, []uint32{0}, func(uint32) (int, error) { return 0, nil }, make([]byte, 512)); err == nil {
		t.Fatal("accepted cyclic sector chain")
	}
}
