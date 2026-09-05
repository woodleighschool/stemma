package archive

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestRejectUnsafeAndAmbiguousArchives(t *testing.T) {
	for name, entries := range map[string][]string{
		"traversal": {"../escape"}, "absolute": {"/escape"},
		"duplicate": {"app.exe", "app.exe"}, "case": {"App.exe", "app.exe"},
		"implicit parent case": {"Data/a", "data/b"},
		"AppleDouble":          {"Fixture.app/Contents/Info.plist", "__MACOSX/Fixture.app/Contents/._Info.plist"},
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
	input := writeZip(t, []string{"one.exe", "two.exe"})
	out := filepath.Join(t.TempDir(), "out")
	if err := Extract(t.Context(), input, out); err != nil {
		t.Fatal(err)
	}
	if _, err := Select(out, ""); err == nil {
		t.Fatal("silently chose ambiguous payload")
	}
	selected, err := Select(out, "two.exe")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(selected) != "two.exe" {
		t.Fatal(selected)
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
