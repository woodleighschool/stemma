package archive

import (
	"archive/tar"
	"archive/zip"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
)

func TestRejectUnsafeArchives(t *testing.T) {
	for name, entries := range map[string][]string{
		"traversal": {"../escape"}, "absolute": {"/escape"},
		"duplicate": {"app.exe", "app.exe"}, "case": {"App.exe", "app.exe"},
		"implicit parent case": {"Data/a", "data/b"},
	} {
		t.Run(name, func(t *testing.T) {
			input := writeZip(t, entries)
			out := filepath.Join(t.TempDir(), "out")
			if err := Extract(t.Context(), input, out); err == nil {
				t.Fatal("accepted unsafe archive")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("failed extraction retained partial output")
			}
		})
	}
	input := writeZip(t, []string{"one.exe", "two.exe", "__MACOSX/._one.exe"})
	out := filepath.Join(t.TempDir(), "out")
	if err := Extract(t.Context(), input, out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "__MACOSX")); !os.IsNotExist(err) {
		t.Fatal("AppleDouble sidecar extracted")
	}
}

func writeZip(t *testing.T, names []string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "input.zip")
	f, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(f)
	for _, name := range names {
		entry, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte("payload")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

// Extraction leaves sidecars behind, so one that holds a code signature stops
// it rather than leaving that code unsigned.
func TestExtractRefusesSignaturesItWouldDiscard(t *testing.T) {
	sidecar := func(attribute string) string {
		data, err := appledouble.FromXattrs(map[string][]byte{attribute: []byte("value")}).Encode()
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	const message = "App.app/Contents/MacOS/share/message.txt"
	for _, test := range []struct {
		name      string
		zip       map[string]string
		pax       map[string]string
		rejection string
	}{
		{name: "signature sidecar", zip: map[string]string{message: "sealed", "__MACOSX/App.app/Contents/MacOS/share/._message.txt": sidecar("com.apple.cs.CodeDirectory")}, rejection: message + " keeps its code signature"},
		{name: "signature beside its file", zip: map[string]string{message: "sealed", "App.app/Contents/MacOS/share/._message.txt": sidecar("com.apple.cs.CodeSignature")}, rejection: message + " keeps its code signature"},
		{name: "other attributes", zip: map[string]string{message: "sealed", "__MACOSX/App.app/Contents/MacOS/share/._message.txt": sidecar("com.apple.quarantine")}},
		{name: "GNU tar attribute", pax: map[string]string{"SCHILY.xattr.com.apple.cs.CodeDirectory": "value"}, rejection: "keeps its code signature"},
		{name: "libarchive attribute", pax: map[string]string{"LIBARCHIVE.xattr.com.apple.cs.CodeSignature": "dmFsdWU="}, rejection: "keeps its code signature"},
		{name: "other PAX attribute", pax: map[string]string{"LIBARCHIVE.xattr.com.apple.quarantine": "dmFsdWU="}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "input")
			f, err := os.Create(input)
			if err != nil {
				t.Fatal(err)
			}
			if test.zip != nil {
				w := zip.NewWriter(f)
				for _, name := range slices.Sorted(maps.Keys(test.zip)) {
					entry, err := w.Create(name)
					if err == nil {
						_, err = entry.Write([]byte(test.zip[name]))
					}
					if err != nil {
						t.Fatal(err)
					}
				}
				err = w.Close()
			} else {
				w := tar.NewWriter(f)
				err = w.WriteHeader(&tar.Header{Name: message, Mode: 0o644, Size: 6, Format: tar.FormatPAX, PAXRecords: test.pax})
				if err == nil {
					_, err = w.Write([]byte("sealed"))
				}
				if err == nil {
					err = w.Close()
				}
			}
			if err := errors.Join(err, f.Close()); err != nil {
				t.Fatal(err)
			}
			err = Extract(t.Context(), input, filepath.Join(t.TempDir(), "out"))
			if test.rejection == "" && err != nil || test.rejection != "" && (err == nil || !strings.Contains(err.Error(), test.rejection)) {
				t.Fatalf("got %v, want %q", err, test.rejection)
			}
		})
	}
}

func TestArchiveNameHintRequiresArchiveSuffix(t *testing.T) {
	for _, name := range []string{"setup.zip.exe", "setup.tar.exe", "download"} {
		t.Run(name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), name)
			if err := os.WriteFile(filename, []byte("scalar installer"), 0o644); err != nil {
				t.Fatal(err)
			}
			archived, err := IsArchive(t.Context(), filename)
			if err != nil || archived {
				t.Fatalf("scalar recognized as archive: %v %v", archived, err)
			}
		})
	}
}
