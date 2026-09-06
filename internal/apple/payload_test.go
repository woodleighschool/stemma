package apple

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cpio "github.com/korylprince/go-cpio-odc"
	xzdecode "github.com/mikelolasagasti/xz"
	"github.com/ulikunitz/xz"
	"howett.net/plist"
)

func TestPackageContentsRetainsOriginalAndVersions(t *testing.T) {
	before := readTestFile(t, "testdata/fixture.pkg")
	facts, err := InspectPackageContents(t.Context(), "testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Applications) != 1 || len(facts.Packages) != 1 {
		t.Fatalf("unexpected subjects: %+v", facts)
	}
	app := facts.Applications[0]
	if app.App.Version != "1.2.3" || app.App.Build != "42" || app.InstalledPath != "/Applications/SignedFixture.app" || app.Path != "Payload/SignedFixture.app" || app.PackagePath != "PackageInfo" {
		t.Fatalf("lost app versions or provenance: %+v", app)
	}
	if !facts.Packages[0].HasPayload || facts.Packages[0].InstalledSize == 0 {
		t.Fatalf("missing payload metadata: %+v", facts.Packages[0])
	}
	if !bytes.Equal(before, readTestFile(t, "testdata/fixture.pkg")) {
		t.Fatal("inspection changed package")
	}
}

func TestPackageContentsRetainsAllComponents(t *testing.T) {
	var members []payloadMember
	for i, appName := range []string{"Example", "Companion"} {
		component := appName + ".pkg"
		metadata := fmt.Sprintf(`<pkg-info identifier="org.example.%s" version="%d.0" install-location="/Applications"><payload installKBytes="20"/></pkg-info>`, strings.ToLower(appName), i+3)
		members = append(members, payloadMember{path.Join(component, "PackageInfo"), []byte(metadata)})
		members = append(members, payloadMember{path.Join(component, "Payload"), cpioPayload(t, []cpio.File{plistEntry(t, "./"+appName+".app/Contents/Info.plist", appName)})})
	}
	members = append(members, payloadMember{"Distribution", []byte(`<installer-gui-script><pkg-ref id="org.example.example">#Example.pkg</pkg-ref><pkg-ref id="org.example.companion">#Companion.pkg</pkg-ref></installer-gui-script>`)})
	name := writePayloadPackage(t, members)
	facts, err := InspectPackageContents(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Packages) != 2 || len(facts.Applications) != 2 {
		t.Fatalf("lost component subjects: %+v", facts)
	}
	for i, app := range facts.Applications {
		if app.App.Version != "2.4" || app.App.Build != "2048" || app.PackagePath != facts.Packages[i].Path || app.Path != path.Join(path.Dir(app.PackagePath), "Payload", app.App.Name+".app") || app.InstalledPath != "/Applications/"+app.App.Name+".app" {
			t.Fatalf("lost component provenance: %+v", app)
		}
	}
}

func TestPackagePayloadCompression(t *testing.T) {
	payload := cpioPayload(t, []cpio.File{plistEntry(t, "./Applications/Example.app/Contents/Info.plist", "Example")})
	for _, compression := range []string{"plain", "gzip", "xz", "pbzx"} {
		t.Run(compression, func(t *testing.T) {
			data := compressPayload(t, compression, payload)
			name := writePayloadPackage(t, []payloadMember{{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="7.0" install-location="/"/>`)}, {"Payload", data}})
			facts, err := InspectPackageContents(t.Context(), name)
			if err != nil {
				t.Fatal(err)
			}
			if len(facts.Applications) != 1 || facts.Applications[0].InstalledPath != "/Applications/Example.app" || facts.Packages[0].Version != "7.0" {
				t.Fatalf("wrong facts: %+v", facts)
			}
		})
	}
}

func TestPackageContentsRejectsUnsafeAndIncompletePayloads(t *testing.T) {
	for _, test := range []struct {
		name string
		file cpio.File
	}{
		{"traversal", cpio.File{Path: "./../outside", Body: []byte("data"), FileMode: 0644}},
		{"absolute", cpio.File{Path: "/Applications/Example.app/Contents/Info.plist", FileMode: 0644}},
		{"symlink_info", cpio.File{Path: "./Example.app/Contents/Info.plist", Body: []byte("/Applications/Example.app/Contents/Info.plist"), FileMode: os.ModeSymlink | 0777}},
		{"hardlink_info", cpio.File{Path: "./Example.app/Contents/Info.plist", FileMode: 0644, NLink: 2}},
		{"malformed_info", cpio.File{Path: "./Example.app/Contents/Info.plist", FileMode: 0644, Body: []byte("malformed plist")}},
		{"oversized_info", cpio.File{Path: "./Example.app/Contents/Info.plist", FileMode: 0644, Body: make([]byte, maxMetadata+1)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := writePayloadPackage(t, []payloadMember{{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1" install-location="/"/>`)}, {"Payload", cpioPayload(t, []cpio.File{test.file})}})
			if facts, err := InspectPackageContents(t.Context(), name); err == nil || len(facts.Applications) != 0 {
				t.Fatalf("accepted incomplete inspection: %+v, %v", facts, err)
			}
		})
	}
	t.Run("ancestor_symlink", func(t *testing.T) {
		payload := cpioPayload(t, []cpio.File{plistEntry(t, "./Example.app/Contents/Info.plist", "Example"), {Path: "./Example.app", FileMode: os.ModeSymlink | 0777, Body: []byte("outside")}})
		budget := newPayloadBudget()
		if _, err := readPayload(bytes.NewReader(payload), budget, false); err == nil {
			t.Fatal("accepted symlink application ancestor")
		}
	})
	t.Run("truncated", func(t *testing.T) {
		payload := cpioPayload(t, []cpio.File{plistEntry(t, "./Example.app/Contents/Info.plist", "Example")})
		budget := newPayloadBudget()
		if _, err := readPayload(bytes.NewReader(payload[:90]), budget, false); err == nil {
			t.Fatal("accepted truncated payload")
		}
	})
	t.Run("corrupt_gzip_checksum", func(t *testing.T) {
		payload := compressPayload(t, "gzip", cpioPayload(t, []cpio.File{plistEntry(t, "./Example.app/Contents/Info.plist", "Example")}))
		payload[len(payload)-5] ^= 1
		budget := newPayloadBudget()
		if _, err := readPayload(bytes.NewReader(payload), budget, false); err == nil {
			t.Fatal("accepted corrupt compression trailer after plist")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		file := plistEntry(t, "./Example.app/Contents/Info.plist", "Example")
		payload := cpioPayload(t, []cpio.File{file, file})
		budget := newPayloadBudget()
		if _, err := readPayload(bytes.NewReader(payload), budget, false); err == nil {
			t.Fatal("accepted duplicate plist")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := InspectPackageContents(ctx, "testdata/fixture.pkg"); !errors.Is(err, context.Canceled) {
			t.Fatalf("lost cancellation: %v", err)
		}
	})
}

func TestPackageContentsRejectsUnsupportedLayouts(t *testing.T) {
	for _, test := range []struct {
		name    string
		members []payloadMember
	}{
		{"nested_pkg", []payloadMember{{"Component.pkg", []byte("xar!")}}},
		{"orphan_payload", []payloadMember{{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1"/>`)}, {"Other.pkg/Payload", []byte("unsupported")}}},
		{"missing_payload", []payloadMember{{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1"><payload installKBytes="1"/></pkg-info>`)}}},
		{"unsupported_payload", []payloadMember{{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1"/>`)}, {"Payload", []byte("not cpio")}}},
		{"unsafe_location", []payloadMember{{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1" install-location="/../Applications"/>`)}}},
		{"external_component", []payloadMember{{"Component.pkg/PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1"/>`)}, {"Distribution", []byte(`<installer-gui-script><pkg-ref id="org.example.remote">https://example.test/Other.pkg</pkg-ref></installer-gui-script>`)}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := InspectPackageContents(t.Context(), writePayloadPackage(t, test.members)); err == nil {
				t.Fatal("accepted unsupported or malformed layout")
			}
		})
	}
}

func TestPayloadBudgetsSpanComponents(t *testing.T) {
	payload := cpioPayload(t, []cpio.File{plistEntry(t, "./Example.app/Contents/Info.plist", "Example")})
	for _, test := range []struct {
		name   string
		budget *payloadBudget
	}{
		{"bytes", &payloadBudget{bytes: int64(len(payload)), metadata: maxPackageMetadata, entries: maxPayloadEntries}},
		{"metadata", &payloadBudget{bytes: maxEntrySize, metadata: int64(len(plistEntry(t, "unused", "Example").Body)), entries: maxPayloadEntries}},
		{"entries", &payloadBudget{bytes: maxEntrySize, metadata: maxPackageMetadata, entries: 2}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := readPayload(bytes.NewReader(payload), test.budget, false); err != nil {
				t.Fatal(err)
			}
			if _, err := readPayload(bytes.NewReader(payload), test.budget, false); err == nil {
				t.Fatal("component reset aggregate inspection budget")
			}
		})
	}
}

func TestPBZXRawAndCompressedChunks(t *testing.T) {
	first := bytes.Repeat([]byte{'x'}, pbzxChunkSize)
	last := []byte("last chunk")
	for _, finalFlag := range []uint64{0, uint64(len(last))} {
		var payload bytes.Buffer
		payload.WriteString("pbzx")
		for _, v := range []uint64{pbzxChunkSize, pbzxChunkSize, pbzxChunkSize} {
			if err := binary.Write(&payload, binary.BigEndian, v); err != nil {
				t.Fatal(err)
			}
		}
		payload.Write(first)
		compressed := compressPayload(t, "xz", last)
		for _, v := range []uint64{finalFlag, uint64(len(compressed))} {
			if err := binary.Write(&payload, binary.BigEndian, v); err != nil {
				t.Fatal(err)
			}
		}
		payload.Write(compressed)
		r, err := newPBZXReader(&payload)
		if err != nil {
			t.Fatal(err)
		}
		got := sha256.New()
		n, err := io.Copy(got, r)
		want := sha256.New()
		want.Write(first)
		want.Write(last)
		if err != nil || n != int64(len(first)+len(last)) || !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
			t.Fatalf("PBZX chunk sequence: %d, %v", n, err)
		}
	}
}

func TestPayloadRejectsOversizedXZDictionary(t *testing.T) {
	stream := []byte{'\xfd', '7', 'z', 'X', 'Z', 0, 0, 4, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(stream[8:], crc32.ChecksumIEEE(stream[6:8]))
	// LZMA2 property 30 declares a 128 MiB dictionary. No body is necessary:
	// the decoder must reject the dictionary before allocating or reading it.
	block := []byte{2, 0, 0x21, 1, 30, 0, 0, 0, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(block[8:], crc32.ChecksumIEEE(block[:8]))
	stream = append(stream, block...)
	if _, err := readPayload(bytes.NewReader(stream), newPayloadBudget(), false); !errors.Is(err, xzdecode.ErrMemlimit) {
		t.Fatalf("XZ dictionary limit not enforced: %v", err)
	}
}

func plistEntry(t *testing.T, name, appName string) cpio.File {
	t.Helper()
	data, err := plist.Marshal(AppFacts{BundleID: "org.example." + strings.ToLower(appName), Name: appName, Version: "2.4", Build: "2048", Executable: appName, MinimumOS: "13.0"}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return cpio.File{Path: name, FileMode: 0644, NLink: 1, Body: data}
}

func cpioPayload(t *testing.T, files []cpio.File) []byte {
	t.Helper()
	var payload bytes.Buffer
	w := cpio.NewWriter(&payload, 512)
	for i, file := range files {
		file.Inode = uint64(i + 1)
		file.ModifiedTime = time.Unix(0, 0)
		if err := w.WriteFile(&file); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return payload.Bytes()
}

func compressPayload(t *testing.T, kind string, data []byte) []byte {
	t.Helper()
	if kind == "plain" {
		return data
	}
	var result bytes.Buffer
	var w io.WriteCloser
	if kind == "gzip" {
		w = gzip.NewWriter(&result)
	} else {
		var err error
		w, err = xz.NewWriter(&result)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if kind != "pbzx" {
		return result.Bytes()
	}
	header := make([]byte, 28)
	copy(header, "pbzx")
	binary.BigEndian.PutUint64(header[4:], pbzxChunkSize)
	binary.BigEndian.PutUint64(header[12:], uint64(len(data)))
	binary.BigEndian.PutUint64(header[20:], uint64(result.Len()))
	return append(header, result.Bytes()...)
}

type payloadMember struct {
	name string
	data []byte
}

func writePayloadPackage(t *testing.T, members []payloadMember) string {
	t.Helper()
	var heap bytes.Buffer
	var files []xarFile
	for _, member := range members {
		digest := sha256.Sum256(member.data)
		data := &xarData{Offset: int64(32 + heap.Len()), Size: int64(len(member.data)), Length: int64(len(member.data)), Archived: xarChecksum{Style: "sha256", Value: fmt.Sprintf("%x", digest)}, Extracted: xarChecksum{Style: "sha256", Value: fmt.Sprintf("%x", digest)}}
		data.Encoding.Style = "application/octet-stream"
		heap.Write(member.data)
		insertPayloadMember(&files, strings.Split(member.name, "/"), data)
	}
	document := struct {
		XMLName xml.Name `xml:"xar"`
		TOC     struct {
			Checksum xarChecksum `xml:"checksum"`
			Files    []xarFile   `xml:"file"`
		} `xml:"toc"`
	}{}
	document.TOC.Checksum = xarChecksum{Style: "sha256", Size: 32}
	document.TOC.Files = files
	toc, err := xml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err := zw.Write(toc); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(compressed.Bytes())
	header := make([]byte, 28)
	copy(header, "xar!")
	binary.BigEndian.PutUint16(header[4:], 28)
	binary.BigEndian.PutUint16(header[6:], 1)
	binary.BigEndian.PutUint64(header[8:], uint64(compressed.Len()))
	binary.BigEndian.PutUint64(header[16:], uint64(len(toc)))
	binary.BigEndian.PutUint32(header[24:], 3)
	name := filepath.Join(t.TempDir(), "fixture.pkg")
	content := header
	content = append(content, compressed.Bytes()...)
	content = append(content, digest[:]...)
	content = append(content, heap.Bytes()...)
	writeTestFile(t, name, content, 0600)
	return name
}

func insertPayloadMember(files *[]xarFile, parts []string, data *xarData) {
	if len(parts) == 1 {
		*files = append(*files, xarFile{Names: []string{parts[0]}, Type: "file", Data: data})
		return
	}
	for i := range *files {
		if (*files)[i].Names[0] == parts[0] {
			insertPayloadMember(&(*files)[i].Files, parts[1:], data)
			return
		}
	}
	*files = append(*files, xarFile{Names: []string{parts[0]}, Type: "directory"})
	insertPayloadMember(&(*files)[len(*files)-1].Files, parts[1:], data)
}
