package macpkg

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deploymenttheory/go-macos-pkg/pkg/cpio"
	"github.com/deploymenttheory/go-macos-pkg/pkg/xar"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/testutil/testarchive"
	"github.com/woodleighschool/stemma/internal/testutil/testdiskimage"
	"github.com/woodleighschool/stemma/plugin"
)

func TestContentsComposeIdenticallyFromTreeZIPTARAndDMG(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "installer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "installer", "run"), []byte("vendor executable\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "installer", "run"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "installer", "config.json"), []byte("{\"tenant\":\"synthetic\"}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	permissions := map[string]uint32{}
	for _, name := range []string{"run", "config.json"} {
		info, err := os.Stat(filepath.Join(root, "installer", name))
		if err != nil {
			t.Fatal(err)
		}
		permissions[name] = uint32(info.Mode().Perm())
	}
	inputs := map[string]plugin.Artifact{"tree": {Path: root, Tree: true}}
	for _, format := range []string{"zip", "tar", "dmg"} {
		filename := filepath.Join(t.TempDir(), "source."+format)
		switch format {
		case "zip":
			testarchive.Zip(t, filename, root)
		case "tar":
			file, err := os.Create(filename)
			if err != nil {
				t.Fatal(err)
			}
			err = archive.Pack(t.Context(), root, file)
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				t.Fatalf("tar: %v %v", err, closeErr)
			}
		case "dmg":
			testdiskimage.Write(t, filename, root)
		}
		inputs[format] = plugin.Artifact{Path: filename, Filename: "source." + format}
	}
	hook := "#!/bin/sh\nexit 93\n"
	spec := Spec{Package: Package{Identifier: "org.example.wrapper", Version: "1.0"},
		Payload: map[string]Entry{"/Library/Example": {Input: "vendor", Path: "installer"}},
		Scripts: map[string]Script{"postinstall": {Content: &hook}, "installer": {Input: "vendor", Path: "installer"}, "config.json": {Input: "vendor", Path: "installer/config.json"}},
	}
	var digest string
	for _, format := range []string{"tree", "zip", "tar", "dmg"} {
		t.Run(format, func(t *testing.T) {
			artifact, err := Build(t.Context(), spec, map[string]plugin.Artifact{"vendor": inputs[format]}, t.TempDir(), time.Unix(1, 0))
			if err != nil {
				t.Fatal(err)
			}
			if digest == "" {
				digest = artifact.SHA256
			} else if artifact.SHA256 != digest {
				t.Fatal("equivalent contents produced different packages")
			}
			files, modes := packageArchive(t, artifact.Path, "Scripts")
			if files["postinstall"] != hook || files["installer/run"] != "vendor executable\n" || files["config.json"] != files["installer/config.json"] {
				t.Fatalf("wrong Scripts contents: %v", files)
			}
			if modes["postinstall"]&0o777 != 0o755 || modes["installer/run"]&0o777 != permissions["run"] || modes["installer/config.json"]&0o777 != permissions["config.json"] {
				t.Fatalf("wrong modes: %v", modes)
			}
		})
	}
}

func TestWrapperKeepsOriginalMediaAndSelectsArchiveRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "tenant.txt"), []byte("synthetic configuration"), 0o644); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "vendor.zip")
	testarchive.Zip(t, filename, root)
	original, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\nexit 94\n"
	spec := Spec{Package: Package{Identifier: "org.example.wrapper", Version: "1"}, Scripts: map[string]Script{
		"postinstall": {Content: &hook}, "vendor.zip": {Input: "vendor"}, "expanded": {Input: "vendor", Path: "."},
	}}
	input := plugin.Artifact{Path: filename, Filename: "vendor.zip"}
	result, err := Build(t.Context(), spec, map[string]plugin.Artifact{"vendor": input}, t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := packageArchive(t, result.Path, "Scripts")
	if !bytes.Equal([]byte(files["vendor.zip"]), original) || files["expanded/tenant.txt"] != "synthetic configuration" {
		t.Fatal("original media or expanded content changed")
	}
	if err := os.WriteFile(filepath.Join(root, "tenant.txt"), []byte("changed configuration"), 0o644); err != nil {
		t.Fatal(err)
	}
	testarchive.Zip(t, filename, root)
	next, err := Build(t.Context(), spec, map[string]plugin.Artifact{"vendor": input}, t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if next.SHA256 == result.SHA256 {
		t.Fatal("changed source bytes did not rebuild wrapper")
	}
}

func TestContentPathsRejectTraversalAndScalarMembers(t *testing.T) {
	for _, name := range []string{"../outside", "/absolute", "Payload/data", "*"} {
		t.Run(name, func(t *testing.T) {
			spec, inputs := fixture(t)
			spec.Payload = map[string]Entry{"/Library/Example": {Input: "script", Path: name}}
			_, err := Build(t.Context(), spec, inputs, t.TempDir(), time.Time{})
			if err == nil {
				t.Fatal("invalid input member accepted")
			}
		})
	}
}

func TestCompositionPreservesConfinedSymlinks(t *testing.T) {
	spec, inputs := fixture(t)
	if err := os.Symlink("Example.otf", filepath.Join(inputs["fonts"].Path, "current")); err != nil {
		t.Skip(err)
	}
	info, err := os.Lstat(filepath.Join(inputs["fonts"].Path, "current"))
	if err != nil {
		t.Fatal(err)
	}
	spec.Scripts["fonts"] = Script{Input: "fonts"}
	result, err := Build(t.Context(), spec, inputs, t.TempDir(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, area := range []string{"Payload", "Scripts"} {
		files, modes := packageArchive(t, result.Path, area)
		name := "fonts/current"
		if area == "Payload" {
			name = "Library/Fonts/current"
		}
		if files[name] != "Example.otf" || modes[name] != cpio.ModeSymlink|uint32(info.Mode().Perm()) {
			t.Fatalf("lost symlink: %s %v", files[name], modes[name])
		}
	}
	spec.Payload = map[string]Entry{"/Library/Example": {Input: "fonts", Path: "current/child"}}
	if _, err := Build(t.Context(), spec, inputs, t.TempDir(), time.Time{}); err == nil {
		t.Fatal("selected through a symlink")
	}
}

func TestCompositionRejectsSymlinkDestinationParents(t *testing.T) {
	spec, inputs := fixture(t)
	root := inputs["fonts"].Path
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"alias": "."} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skip(err)
		}
	}
	content := "must not be written through a symlink"
	spec.Payload = nil
	spec.Scripts["."] = Script{Input: "fonts", Path: "."}
	spec.Scripts["alias/outside"] = Script{Content: &content}
	_, err := Build(t.Context(), spec, inputs, t.TempDir(), time.Time{})
	if err == nil || !strings.Contains(err.Error(), "destination parent") {
		t.Fatalf("symlink parent was not rejected during staging: %v", err)
	}
}

func packageArchive(t *testing.T, filename, member string) (map[string]string, map[string]uint32) {
	t.Helper()
	pkg, err := xar.OpenFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pkg.Close() }()
	for _, file := range pkg.Files() {
		if file.Name() != member {
			continue
		}
		data, err := pkg.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = data.Close() }()
		gz, err := gzip.NewReader(data)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = gz.Close() }()
		reader := cpio.NewReader(gz)
		files := map[string]string{}
		modes := map[string]uint32{}
		for {
			header, err := reader.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			name := strings.TrimPrefix(header.Name, "./")
			files[name] = string(body)
			modes[name] = header.Mode
		}
		return files, modes
	}
	t.Fatalf("package has no %s", member)
	return nil, nil
}
