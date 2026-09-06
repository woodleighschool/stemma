package pkgbuild

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	cpio "github.com/korylprince/go-cpio-odc"
)

func TestPackageTimestamps(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		t.Run(strconv.FormatBool(fixed), func(t *testing.T) {
			root, opts := fixture(t)
			opts.Timestamp = time.Time{}
			directoryDate := time.Date(2024, 3, 4, 5, 6, 7, 0, time.UTC)
			fileDate := time.Date(2023, 2, 3, 4, 5, 6, 0, time.UTC)
			sourceDates := map[string]time.Time{
				"Payload": directoryDate, "Payload/Library": directoryDate,
				"Payload/Library/Application Support": directoryDate, "Payload/Library/Application Support/Fixture": directoryDate,
				"Payload/Library/Application Support/Fixture/message.txt": fileDate,
				"Scripts/preinstall": fileDate, "Scripts/postinstall": directoryDate,
			}
			for name, date := range sourceDates {
				if err := os.Chtimes(filepath.Join(root, name), date, date); err != nil {
					t.Fatal(err)
				}
			}
			if fixed {
				opts.Timestamp = time.Date(2025, 4, 5, 6, 7, 8, 0, time.UTC)
			}
			before := time.Now().UTC().Truncate(time.Second)
			output := filepath.Join(t.TempDir(), "dates.pkg")
			if err := Build(t.Context(), root, output, opts); err != nil {
				t.Fatal(err)
			}
			after := time.Now().UTC().Truncate(time.Second)
			toc, members := timestampMembers(t, output)
			var observed struct {
				Creation string `xml:"toc>creation-time"`
				Files    []struct {
					Name     string `xml:"name"`
					Modified string `xml:"mtime"`
				} `xml:"toc>file"`
			}
			if err := xml.Unmarshal(toc, &observed); err != nil {
				t.Fatal(err)
			}
			created, err := time.Parse("2006-01-02T15:04:05", observed.Creation)
			if err != nil {
				t.Fatalf("XAR creation time: %q: %v", observed.Creation, err)
			}
			if fixed {
				if !created.Equal(opts.Timestamp) {
					t.Fatal("XAR creation does not use explicit timestamp")
				}
			} else if created.Before(before) || created.After(after) {
				t.Fatal("generated XAR date is not captured build time")
			}
			for _, member := range observed.Files {
				stamp, err := time.Parse(time.RFC3339, member.Modified)
				if err != nil || !stamp.Equal(created) {
					t.Fatalf("XAR %s date disagrees with creation: %q", member.Name, member.Modified)
				}
			}
			for _, kind := range []string{"Payload", "Scripts"} {
				files := cpioTimestamps(t, members[kind])
				for name, stamp := range files {
					want := sourceDates[path.Join(kind, name)]
					if fixed {
						want = opts.Timestamp
					} else if kind == "Scripts" && name == "." {
						want = created
					}
					if want.IsZero() || !stamp.Equal(want) {
						t.Errorf("%s/%s: mtime %s, want %s", kind, name, stamp, want)
					}
				}
			}
			bomDates := bomTimestamps(t, members["Bom"])
			for _, stamp := range bomDates {
				want := fileDate
				if stamp.directory {
					want = directoryDate
				}
				if fixed {
					want = opts.Timestamp
				}
				if stamp.seconds != uint32(want.Unix()) {
					t.Errorf("BOM date %d, want %d", stamp.seconds, want.Unix())
				}
			}
			if len(bomDates) != len(sourceDates)-2 {
				t.Fatal("BOM timestamp entries missing")
			}
		})
	}
}

func TestTimestampRange(t *testing.T) {
	for _, second := range []int64{-1, 0, math.MaxUint32, math.MaxUint32 + 1} {
		t.Run(strconv.FormatInt(second, 10), func(t *testing.T) {
			root, opts := fixture(t)
			opts.Timestamp = time.Unix(second, 0)
			output := filepath.Join(t.TempDir(), "out.pkg")
			err := Build(t.Context(), root, output, opts)
			if second < 0 || second > math.MaxUint32 {
				if err == nil {
					t.Fatal("unrepresentable timestamp accepted")
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed build left output")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func timestampMembers(t *testing.T, filename string) ([]byte, map[string][]byte) {
	t.Helper()
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	size := binary.BigEndian.Uint64(data[8:16])
	toc, err := inflateTOC(data[28 : 28+size])
	if err != nil {
		t.Fatal(err)
	}
	var doc xarDocument
	if err := xml.Unmarshal(toc, &doc); err != nil {
		t.Fatal(err)
	}
	members := map[string][]byte{}
	for _, f := range doc.TOC.Files {
		start := int64(28+size) + f.Data.Offset
		members[f.Name] = data[start : start+f.Data.Length]
	}
	return toc, members
}
func cpioTimestamps(t *testing.T, data []byte) map[string]time.Time {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gz.Close() }()
	reader := cpio.NewReader(gz)
	out := map[string]time.Time{}
	for {
		file, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out[path.Clean(file.Path)] = file.ModifiedTime
	}
	return out
}

type bomTimestamp struct {
	directory bool
	seconds   uint32
}

func bomTimestamps(t *testing.T, data []byte) []bomTimestamp {
	t.Helper()
	// PathInfo2 stores a type byte and an unsigned Unix mtime at byte 14.
	// The file and directory records are 35 and 31 bytes respectively.
	index := binary.BigEndian.Uint32(data[16:20])
	count := binary.BigEndian.Uint32(data[index : index+4])
	var out []bomTimestamp
	for i := range count {
		entry := index + 4 + 8*i
		start := binary.BigEndian.Uint32(data[entry : entry+4])
		size := binary.BigEndian.Uint32(data[entry+4 : entry+8])
		block := data[start : start+size]
		if (len(block) == 31 || len(block) == 35) && (block[0] == 1 || block[0] == 2) && block[1] == 1 {
			out = append(out, bomTimestamp{directory: block[0] == 2, seconds: binary.BigEndian.Uint32(block[14:18])})
		}
	}
	return out
}
