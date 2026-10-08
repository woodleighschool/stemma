package diskimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/deploymenttheory/go-apfs-v2/pkg/appledouble"
)

var imageTime = time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC)

// bundleFixture writes a bundle with an executable, a versioned framework and
// its symlinks, the shape code signing and the dynamic linker depend on.
func bundleFixture(t *testing.T) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "Example.app")
	files := map[string]fs.FileMode{
		"Contents/Info.plist":                                 0o644,
		"Contents/MacOS/example":                              0o755,
		"Contents/Resources/Base.lproj/Main.strings":          0o444,
		"Contents/Frameworks/Kit.framework/Versions/A/Kit":    0o755,
		"Contents/Frameworks/Kit.framework/Versions/A/é.data": 0o600,
	}
	for name, mode := range files {
		filename := filepath.Join(app, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filename, bytes.Repeat([]byte(name), 700), mode); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"Contents/Frameworks/Kit.framework/Versions/Current": "A",
		"Contents/Frameworks/Kit.framework/Kit":              "Versions/Current/Kit",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(app, filepath.FromSlash(name))); err != nil {
			t.Skip(err)
		}
	}
	return app
}

func TestWriteApplicationPreservesTheBundle(t *testing.T) {
	for _, compression := range []Compression{LZFSE, Zlib, LZMA} {
		t.Run(string(compression), func(t *testing.T) {
			app := bundleFixture(t)
			output := filepath.Join(t.TempDir(), "Example.dmg")
			if err := writeApplication(t.Context(), app, output, compression, imageTime); err != nil {
				t.Fatal(err)
			}
			image, err := Open(t.Context(), output)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = image.Close() }()
			if info, err := fs.Stat(image, "Example.app"); err != nil || !info.IsDir() {
				t.Fatalf("bundle missing from the image: %v", err)
			}
			if entries, err := image.ReadDir("."); err != nil || len(entries) != 1 {
				t.Fatalf("volume root holds more than the bundle: %v, %v", entries, err)
			}
			seen := 0
			err = filepath.WalkDir(app, func(filename string, _ fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				relative, err := filepath.Rel(filepath.Dir(app), filename)
				if err != nil {
					return err
				}
				name := filepath.ToSlash(relative)
				want, err := os.Lstat(filename)
				if err != nil {
					return err
				}
				got, err := image.Lstat(name)
				if err != nil {
					return err
				}
				seen++
				if !got.ModTime().Equal(imageTime) {
					t.Errorf("%s dated %v", name, got.ModTime())
				}
				switch {
				case want.Mode()&fs.ModeSymlink != 0:
					target, err := os.Readlink(filename)
					if err != nil {
						return err
					}
					if link, err := image.ReadLink(name); err != nil || link != filepath.ToSlash(target) || got.Mode()&fs.ModeSymlink == 0 {
						t.Errorf("%s links to %q as %v: %v", name, link, got.Mode(), err)
					}
				case got.Mode().Type() != want.Mode().Type() || got.Mode().Perm() != want.Mode().Perm():
					t.Errorf("%s mode %v, want %v", name, got.Mode(), want.Mode())
				case want.Mode().IsRegular():
					expected, err := os.ReadFile(filename)
					if err != nil {
						return err
					}
					file, err := image.Open(name)
					if err != nil {
						return err
					}
					actual, err := io.ReadAll(file)
					_ = file.Close()
					if err != nil || !bytes.Equal(actual, expected) {
						t.Errorf("%s bytes differ: %v", name, err)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if seen != 16 {
				t.Fatalf("compared %d entries", seen)
			}
		})
	}
}

func TestWriteApplicationPreservesZeroPermissions(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows does not represent zero Unix permissions")
	}
	app := bundleFixture(t)
	name := filepath.Join(app, "Contents/Resources/empty")
	if err := os.WriteFile(name, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, 0); err != nil {
		t.Fatal(err)
	}
	imagePath := filepath.Join(t.TempDir(), "Example.dmg")
	if err := writeApplication(t.Context(), app, imagePath, Zlib, imageTime); err != nil {
		t.Fatal(err)
	}
	image, err := Open(t.Context(), imagePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = image.Close() }()
	info, err := image.Lstat("Example.app/Contents/Resources/empty")
	if err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("zero permissions replaced by defaults: %v: %v", info, err)
	}
}

func TestWriteApplicationIsReproducible(t *testing.T) {
	digest := func(app string, compression Compression, timestamp time.Time) [sha256.Size]byte {
		t.Helper()
		output := filepath.Join(t.TempDir(), "Example.dmg")
		if err := writeApplication(t.Context(), app, output, compression, timestamp); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		return sha256.Sum256(data)
	}
	first, second := bundleFixture(t), bundleFixture(t)
	if err := os.Chtimes(filepath.Join(second, "Contents/Info.plist"), imageTime, imageTime.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, compression := range []Compression{LZFSE, Zlib, LZMA} {
		if digest(first, compression, imageTime) != digest(second, compression, imageTime) {
			t.Fatalf("equal bundles produced different %s images", compression)
		}
	}
	if digest(first, LZFSE, imageTime) == digest(first, LZFSE, imageTime.Add(time.Second)) {
		t.Fatal("timestamp did not reach the image")
	}
}

func TestWriteApplicationRejectsUnsupportedInput(t *testing.T) {
	app := bundleFixture(t)
	directory := filepath.Join(t.TempDir(), "Example")
	if err := os.Mkdir(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(t.TempDir(), "Example.dmg")
	if err := os.WriteFile(existing, []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, test := range map[string]struct {
		ctx         context.Context
		app         string
		output      string
		compression Compression
		timestamp   time.Time
	}{
		"not a bundle":        {t.Context(), directory, filepath.Join(t.TempDir(), "Example.dmg"), LZFSE, imageTime},
		"existing output":     {t.Context(), app, existing, LZFSE, imageTime},
		"unknown compression": {t.Context(), app, filepath.Join(t.TempDir(), "Example.dmg"), "bzip2", imageTime},
		"date before HFS+":    {t.Context(), app, filepath.Join(t.TempDir(), "Example.dmg"), LZFSE, time.Date(1903, 1, 1, 0, 0, 0, 0, time.UTC)},
		"date after HFS+":     {t.Context(), app, filepath.Join(t.TempDir(), "Example.dmg"), LZFSE, time.Date(2041, 1, 1, 0, 0, 0, 0, time.UTC)},
		"cancelled context":   {cancelled, app, filepath.Join(t.TempDir(), "Example.dmg"), LZFSE, imageTime},
	} {
		t.Run(name, func(t *testing.T) {
			if err := writeApplication(test.ctx, test.app, test.output, test.compression, test.timestamp); err == nil {
				t.Fatal("accepted")
			}
			entries, err := os.ReadDir(filepath.Dir(test.output))
			if err != nil || len(entries) > 1 || len(entries) == 1 && test.output != existing {
				t.Fatalf("left %v behind: %v", entries, err)
			}
		})
	}
	if data, err := os.ReadFile(existing); err != nil || string(data) != "kept" {
		t.Fatalf("existing output changed: %q, %v", data, err)
	}
}

func TestWriteApplicationRejectsWhatTheVolumeCannotHold(t *testing.T) {
	t.Run("special permission bits", func(t *testing.T) {
		app := bundleFixture(t)
		filename := filepath.Join(app, "Contents/MacOS/example")
		if err := os.Chmod(filename, fs.ModeSetuid|0o755); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(filename); err != nil || info.Mode()&fs.ModeSetuid == 0 {
			t.Skip("host does not record setuid")
		}
		if err := writeApplication(t.Context(), app, filepath.Join(t.TempDir(), "Example.dmg"), LZFSE, imageTime); err == nil {
			t.Fatal("setuid executable accepted")
		}
	})
	t.Run("names differing by case", func(t *testing.T) {
		app := bundleFixture(t)
		for _, name := range []string{"readme", "README"} {
			if err := os.WriteFile(filepath.Join(app, "Contents/Resources", name), []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if entries, err := os.ReadDir(filepath.Join(app, "Contents/Resources")); err != nil || len(entries) != 3 {
			t.Skip("host folds case")
		}
		if err := writeApplication(t.Context(), app, filepath.Join(t.TempDir(), "Example.dmg"), LZFSE, imageTime); err == nil {
			t.Fatal("case-conflicting names accepted")
		}
	})
	t.Run("escaping symlink", func(t *testing.T) {
		app := bundleFixture(t)
		if err := os.Symlink("../../../outside", filepath.Join(app, "Contents/Resources/outside")); err != nil {
			t.Skip(err)
		}
		err := writeApplication(t.Context(), app, filepath.Join(t.TempDir(), "Example.dmg"), LZFSE, imageTime)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("escaping symlink accepted: %v", err)
		}
	})
}

// Local fixtures use the same logical filesystem boundary as archive inputs.
func writeApplication(ctx context.Context, app, output string, compression Compression, timestamp time.Time) error {
	root, err := os.OpenRoot(filepath.Dir(app))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return WriteApplication(ctx, bundleFS{root.FS().(fs.ReadLinkFS)}, filepath.Base(app), output, compression, timestamp)
}

// Match archive.Tree's host-to-POSIX boundary for local bundle fixtures.
type bundleFS struct{ fs.ReadLinkFS }

func (b bundleFS) ReadLink(name string) (string, error) {
	target, err := b.ReadLinkFS.ReadLink(name)
	return filepath.ToSlash(target), err
}

func TestWriteApplicationLogicalSymlinks(t *testing.T) {
	for _, test := range []struct {
		name, target string
		reject       bool
	}{
		{"nested", "nested/file", false},
		{"parent inside bundle", "../Resources/nested/file", false},
		{"literal backslash", `nested\file`, false},
		{"escaping", "../../../outside", true},
		{"absolute", "/outside", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := fstest.MapFS{
				"Example.app/Contents/Resources/link": {Mode: fs.ModeSymlink | 0o755, Data: []byte(test.target)},
			}
			output := filepath.Join(t.TempDir(), "Example.dmg")
			err := WriteApplication(t.Context(), source, "Example.app", output, Zlib, imageTime)
			if test.reject {
				if err == nil || errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("escaping symlink accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			image, err := Open(t.Context(), output)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = image.Close() }()
			if target, err := image.ReadLink("Example.app/Contents/Resources/link"); err != nil || target != test.target {
				t.Fatalf("symlink target = %q, want %q: %v", target, test.target, err)
			}
		})
	}
}

func TestWriteApplicationHFSNames(t *testing.T) {
	for _, test := range []struct {
		name, first, second string
		reject              bool
	}{
		{"case", "readme", "README", true},
		{"canonical", "é", "e\u0301", true},
		{"ignored character", "file", "fi\u200cle", true},
		{"HFS decomposition exclusion", "\u212b", "Å", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := fstest.MapFS{
				"Example.app/" + test.first:  {Data: []byte("first"), Mode: 0o644},
				"Example.app/" + test.second: {Data: []byte("second"), Mode: 0o644},
			}
			output := filepath.Join(t.TempDir(), "Example.dmg")
			err := WriteApplication(t.Context(), source, "Example.app", output, Zlib, imageTime)
			if (err != nil) != test.reject {
				t.Fatalf("write = %v, reject = %v", err, test.reject)
			}
		})
	}
}

type metadataFS struct {
	fstest.MapFS

	attribute []byte
}

func (m metadataFS) XattrValues(name string) (map[string]appledouble.Value, error) {
	if name != "Example.app/file" {
		return nil, nil
	}
	return map[string]appledouble.Value{"com.apple.decmpfs": bytes.NewReader(m.attribute)}, nil
}

func TestWriteApplicationKeepsCompressionAttributesInactive(t *testing.T) {
	// Type 3 with an uncompressed inline block describes stale, different bytes.
	stored := []byte("stale")
	header := make([]byte, 17+len(stored))
	binary.LittleEndian.PutUint32(header, 0x636d7066)
	binary.LittleEndian.PutUint32(header[4:], 3)
	binary.LittleEndian.PutUint64(header[8:], uint64(len(stored)))
	header[16] = 0xff
	copy(header[17:], stored)
	for _, attribute := range [][]byte{header, []byte("invalid compression metadata")} {
		source := metadataFS{MapFS: fstest.MapFS{"Example.app/file": {Data: []byte("current logical bytes"), Mode: 0o644}}, attribute: attribute}
		output := filepath.Join(t.TempDir(), "Example.dmg")
		if err := WriteApplication(t.Context(), source, "Example.app", output, Zlib, imageTime); err != nil {
			t.Fatal(err)
		}
		image, err := Open(t.Context(), output)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = image.Close() })
		data, err := fs.ReadFile(image, "Example.app/file")
		if err != nil || string(data) != "current logical bytes" {
			t.Fatalf("contents = %q: %v", data, err)
		}
		attrs, err := image.Xattrs("Example.app/file")
		if err != nil || !bytes.Equal(attrs["com.apple.decmpfs"], attribute) {
			t.Fatalf("metadata changed: %v", err)
		}
	}
}
