package apple

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deploymenttheory/go-macos-pkg/pkg/cpio"
	"github.com/deploymenttheory/go-macos-pkg/pkg/pbzx"
	"github.com/deploymenttheory/go-macos-pkg/pkg/xar"
	xzdecode "github.com/mikelolasagasti/xz"
	"github.com/ulikunitz/xz"
	"github.com/woodleighschool/stemma/internal/signature"
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
		members = append(members, payloadMember{path.Join(component, "Payload"), cpioPayload(t, []payloadEntry{plistEntry(t, "./"+appName+".app/Contents/Info.plist", appName)})})
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
	payload := cpioPayload(t, []payloadEntry{plistEntry(t, "./Applications/Example.app/Contents/Info.plist", "Example")})
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

func TestPackageContentsAcceptsDotSlashRoot(t *testing.T) {
	// Mozilla's Firefox PKG names its payload root "./" and every other entry
	// without a leading "./".
	name := applicationPackage(t, "/Applications", []payloadEntry{
		directoryEntry("./"), directoryEntry("Example.app"), directoryEntry("Example.app/Contents"),
		plistEntry(t, "Example.app/Contents/Info.plist", "Example"),
	})
	facts, err := InspectPackageContents(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts.Applications) != 1 || facts.Applications[0].InstalledPath != "/Applications/Example.app" {
		t.Fatalf("wrong applications: %+v", facts.Applications)
	}
}

func TestPackageContentsRejectsUnsafeAndIncompletePayloads(t *testing.T) {
	for _, test := range []struct {
		name   string
		header cpio.Header
		body   []byte
	}{
		{"traversal", cpio.Header{Name: "./../outside", Mode: cpio.ModeRegular | 0o644}, []byte("data")},
		{"file_root", cpio.Header{Name: "./", Mode: cpio.ModeRegular | 0o644}, []byte("data")},
		{"absolute", cpio.Header{Name: "/Applications/Example.app/Contents/Info.plist", Mode: cpio.ModeRegular | 0o644}, nil},
		{"symlink_info", cpio.Header{Name: "./Example.app/Contents/Info.plist", Mode: cpio.ModeSymlink | 0o777}, []byte("/Applications/Example.app/Contents/Info.plist")},
		{"hardlink_info", cpio.Header{Name: "./Example.app/Contents/Info.plist", Mode: cpio.ModeRegular | 0o644, NLink: 2}, nil},
		{"malformed_info", cpio.Header{Name: "./Example.app/Contents/Info.plist", Mode: cpio.ModeRegular | 0o644}, []byte("malformed plist")},
		{"oversized_info", cpio.Header{Name: "./Example.app/Contents/Info.plist", Mode: cpio.ModeRegular | 0o644}, make([]byte, maxMetadata+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			name := writePayloadPackage(t, []payloadMember{{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1" install-location="/"/>`)}, {"Payload", cpioPayload(t, []payloadEntry{{test.header, test.body}})}})
			if facts, err := InspectPackageContents(t.Context(), name); err == nil || len(facts.Applications) != 0 {
				t.Fatalf("accepted incomplete inspection: %+v, %v", facts, err)
			}
		})
	}
	t.Run("ancestor_symlink", func(t *testing.T) {
		payload := cpioPayload(t, []payloadEntry{plistEntry(t, "./Example.app/Contents/Info.plist", "Example"), {cpio.Header{Name: "./Example.app", Mode: cpio.ModeSymlink | 0o777}, []byte("outside")}})
		budget := newPayloadBudget()
		if _, err := readPayload(t.Context(), io.NopCloser(bytes.NewReader(payload)), budget, false); err == nil {
			t.Fatal("accepted symlink application ancestor")
		}
	})
	t.Run("truncated", func(t *testing.T) {
		payload := cpioPayload(t, []payloadEntry{plistEntry(t, "./Example.app/Contents/Info.plist", "Example")})
		budget := newPayloadBudget()
		if _, err := readPayload(t.Context(), io.NopCloser(bytes.NewReader(payload[:90])), budget, false); err == nil {
			t.Fatal("accepted truncated payload")
		}
	})
	t.Run("corrupt_gzip_checksum", func(t *testing.T) {
		payload := compressPayload(t, "gzip", cpioPayload(t, []payloadEntry{plistEntry(t, "./Example.app/Contents/Info.plist", "Example")}))
		payload[len(payload)-5] ^= 1
		budget := newPayloadBudget()
		if _, err := readPayload(t.Context(), io.NopCloser(bytes.NewReader(payload)), budget, false); err == nil {
			t.Fatal("accepted corrupt compression trailer after plist")
		}
	})
	t.Run("duplicate", func(t *testing.T) {
		file := plistEntry(t, "./Example.app/Contents/Info.plist", "Example")
		payload := cpioPayload(t, []payloadEntry{file, file})
		budget := newPayloadBudget()
		if _, err := readPayload(t.Context(), io.NopCloser(bytes.NewReader(payload)), budget, false); err == nil {
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

func TestPackageContentsVerifiesPayloadAtEOF(t *testing.T) {
	sentinel := []byte("bytes sealed only by XAR checksums")
	archive := cpioPayload(t, []payloadEntry{
		plistEntry(t, "./Example.app/Contents/Info.plist", "Example"),
		{cpio.Header{Name: "./Example.app/Contents/Resources/data", Mode: cpio.ModeRegular | 0o644, NLink: 1}, sentinel},
	})
	info := payloadMember{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1" install-location="/"/>`)}
	zero := strings.Repeat("00", sha256.Size)
	for _, compression := range []string{"plain", "gzip", "xz", "pbzx"} {
		payload := compressPayload(t, compression, archive)
		for _, test := range []struct {
			name   string
			tamper func(*xar.Data)
			want   string
		}{
			{"intact", func(*xar.Data) {}, ""},
			{"archived_checksum", func(data *xar.Data) { data.ArchivedChecksum.Value = zero }, "archived checksum mismatch"},
			{"extracted_checksum", func(data *xar.Data) { data.ExtractedChecksum.Value = zero }, "extracted checksum mismatch"},
			{"declared_size", func(data *xar.Data) { data.Size++ }, "short by 1 bytes"},
		} {
			t.Run(compression+"/"+test.name, func(t *testing.T) {
				name := writeTamperedPayloadPackage(t, []payloadMember{info, {"Payload", payload}}, func(member string, data *xar.Data) {
					if member == "Payload" {
						test.tamper(data)
					}
				})
				facts, err := InspectPackageContents(t.Context(), name)
				if test.want == "" {
					if err != nil || len(facts.Applications) != 1 {
						t.Fatalf("intact payload: %+v, %v", facts, err)
					}
				} else if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("payload verification error = %v, want %q", err, test.want)
				}
			})
		}
	}
	t.Run("plain/stored_bytes", func(t *testing.T) {
		name := writePayloadPackage(t, []payloadMember{info, {"Payload", archive}})
		content := readTestFile(t, name)
		content[bytes.Index(content, sentinel)] ^= 1
		writeTestFile(t, name, content, 0o600)
		if _, err := InspectPackageContents(t.Context(), name); err == nil || !strings.Contains(err.Error(), "archived checksum mismatch") {
			t.Fatalf("corrupt stored payload error = %v", err)
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
	payload := cpioPayload(t, []payloadEntry{plistEntry(t, "./Example.app/Contents/Info.plist", "Example")})
	for _, test := range []struct {
		name   string
		budget *payloadBudget
	}{
		{"bytes", &payloadBudget{bytes: int64(len(payload)), metadata: maxPackageMetadata, entries: maxPayloadEntries}},
		{"metadata", &payloadBudget{bytes: maxEntrySize, metadata: int64(len(plistEntry(t, "unused", "Example").body)), entries: maxPayloadEntries}},
		{"entries", &payloadBudget{bytes: maxEntrySize, metadata: maxPackageMetadata, entries: 2}},
	} {
		for _, compression := range []string{"plain", "pbzx"} {
			t.Run(test.name+"/"+compression, func(t *testing.T) {
				budget := *test.budget
				data := compressPayload(t, compression, payload)
				if _, err := readPayload(t.Context(), io.NopCloser(bytes.NewReader(data)), &budget, false); err != nil {
					t.Fatal(err)
				}
				if _, err := readPayload(t.Context(), io.NopCloser(bytes.NewReader(data)), &budget, false); err == nil {
					t.Fatal("component reset aggregate inspection budget")
				}
			})
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
	if _, err := readPayload(t.Context(), io.NopCloser(bytes.NewReader(stream)), newPayloadBudget(), false); !errors.Is(err, xzdecode.ErrMemlimit) {
		t.Fatalf("XZ dictionary limit not enforced: %v", err)
	}
}

func TestPackageRestartActions(t *testing.T) {
	for _, test := range []struct{ action, want string }{
		{"none", ""}, {"logout", "RequireLogout"}, {"restart", "RequireRestart"}, {"shutdown", "RequireShutdown"}, {"unknown", ""},
	} {
		t.Run(test.action, func(t *testing.T) {
			metadata := fmt.Sprintf(`<pkg-info identifier="org.example.app" version="1" postinstall-action="%s"/>`, test.action)
			facts, err := InspectPackageMetadata(t.Context(), writePayloadPackage(t, []payloadMember{{"PackageInfo", []byte(metadata)}}))
			if err != nil {
				t.Fatal(err)
			}
			if facts.RestartAction != test.want {
				t.Fatalf("restart action = %q, want %q", facts.RestartAction, test.want)
			}
		})
	}
}

func TestDistributionDeclarations(t *testing.T) {
	distribution := `<installer-gui-script>
		<product id="org.example.suite" version="9.4"/>
		<volume-check><allowed-os-versions><os-version min="12.9"/><os-version min="14.2"/></allowed-os-versions><allowed-os-versions><os-version min="99"/></allowed-os-versions></volume-check>
		<choice id="one" selected="false" enabled="false"><pkg-ref id="org.example.one"/></choice>
		<pkg-ref id="org.example.one" onConclusion="RecommendRestart" onConclusionScript="'RequireShutdown'">#One.pkg</pkg-ref>
		<pkg-ref id="org.example.two" onConclusion="requirelogout">#Two.pkg</pkg-ref>
	</installer-gui-script>`
	name := writePayloadPackage(t, []payloadMember{
		{"One.pkg/PackageInfo", []byte(`<pkg-info identifier="org.example.one" version="8" minimumSystemVersion="16" postinstall-action="shutdown"><payload installKBytes="75"/></pkg-info>`)},
		{"One.pkg/Payload", cpioPayload(t, nil)},
		{"Two.pkg/PackageInfo", []byte(`<pkg-info identifier="org.example.two" version="7"/>`)},
		{"Distribution", []byte(distribution)},
	})
	for _, contents := range []bool{false, true} {
		facts, err := inspectPackage(t.Context(), name, contents)
		if err != nil {
			t.Fatal(err)
		}
		if facts.Version != "9.4" || facts.MinimumOS != "14.2" || facts.RestartAction != "RequireLogout" {
			t.Fatalf("contents=%v: version %q, minimum OS %q, restart %q", contents, facts.Version, facts.MinimumOS, facts.RestartAction)
		}
	}
}

func TestPackageMinimumOSFallsBackToPayloadReceipts(t *testing.T) {
	name := writePayloadPackage(t, []payloadMember{
		{"One.pkg/PackageInfo", []byte(`<pkg-info identifier="org.example.one" version="8" minimumSystemVersion="16"/>`)},
		{"Two.pkg/PackageInfo", []byte(`<pkg-info identifier="org.example.two" version="7" minimumSystemVersion="13.10"><payload installKBytes="0"/></pkg-info>`)},
		{"Two.pkg/Payload", cpioPayload(t, nil)},
		{"Three.pkg/PackageInfo", []byte(`<pkg-info identifier="org.example.three" version="6" minimumSystemVersion="13.9" postinstall-action="restart"><payload installKBytes="75"/></pkg-info>`)},
		{"Three.pkg/Payload", cpioPayload(t, nil)},
		{"Distribution", []byte(`<installer-gui-script><pkg-ref id="org.example.one">#One.pkg</pkg-ref><pkg-ref id="org.example.two">#Two.pkg</pkg-ref><pkg-ref id="org.example.three">#Three.pkg</pkg-ref></installer-gui-script>`)},
	})
	facts, err := InspectPackageMetadata(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	if facts.MinimumOS != "13.10" || facts.RestartAction != "" || facts.Version != "" {
		t.Fatalf("minimum OS %q, restart %q, version %q", facts.MinimumOS, facts.RestartAction, facts.Version)
	}
}

type payloadEntry struct {
	header cpio.Header
	body   []byte
}

func plistEntry(t *testing.T, name, appName string) payloadEntry {
	t.Helper()
	data, err := plist.Marshal(AppFacts{BundleID: "org.example." + strings.ToLower(appName), Name: appName, Version: "2.4", Build: "2048", Executable: appName, MinimumOS: "13.0"}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return payloadEntry{cpio.Header{Name: name, Mode: cpio.ModeRegular | 0o644, NLink: 1}, data}
}

func cpioPayload(t *testing.T, files []payloadEntry) []byte {
	t.Helper()
	var payload bytes.Buffer
	w := cpio.NewWriter(&payload)
	for i, file := range files {
		file.header.Inode = uint64(i + 1)
		file.header.ModTime = time.Unix(0, 0)
		file.header.Size = int64(len(file.body))
		if err := w.WriteHeader(&file.header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(file.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
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
	var err error
	switch kind {
	case "gzip":
		w = gzip.NewWriter(&result)
	case "pbzx":
		w, err = pbzx.NewWriter(&result, pbzx.XZ, 0)
	default:
		w, err = xz.NewWriter(&result)
	}
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

type payloadMember struct {
	name string
	data []byte
}

func writePayloadPackage(t *testing.T, members []payloadMember) string {
	t.Helper()
	return writeTamperedPayloadPackage(t, members, func(string, *xar.Data) {})
}

func writeTamperedPayloadPackage(t *testing.T, members []payloadMember, tamper func(name string, data *xar.Data)) string {
	t.Helper()
	var heap bytes.Buffer
	var files []*xar.File
	for _, member := range members {
		digest := sha256.Sum256(member.data)
		data := &xar.Data{Offset: int64(32 + heap.Len()), Size: int64(len(member.data)), Length: int64(len(member.data)), ArchivedChecksum: &xar.Digest{Style: "sha256", Value: hex.EncodeToString(digest[:])}, ExtractedChecksum: &xar.Digest{Style: "sha256", Value: hex.EncodeToString(digest[:])}}
		data.Encoding.Style = "application/octet-stream"
		tamper(member.name, data)
		heap.Write(member.data)
		insertPayloadMember(&files, strings.Split(member.name, "/"), data)
	}
	document := struct {
		XMLName xml.Name `xml:"xar"`
		TOC     xar.TOC  `xml:"toc"`
	}{}
	document.TOC.Checksum = &xar.Checksum{Style: "sha256", Size: 32}
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

func insertPayloadMember(files *[]*xar.File, parts []string, data *xar.Data) {
	if len(parts) > 1 {
		for _, file := range *files {
			if file.Name() == parts[0] {
				insertPayloadMember(&file.Children, parts[1:], data)
				return
			}
		}
	}
	file := &xar.File{}
	file.SetName(parts[0])
	*files = append(*files, file)
	if len(parts) == 1 {
		file.Type.Value, file.Data = xar.TypeFile, data
	} else {
		file.Type.Value = xar.TypeDirectory
		insertPayloadMember(&file.Children, parts[1:], data)
	}
}

func TestLargePackageInventory(t *testing.T) {
	entries := make([]payloadEntry, 0, 110001)
	for i := range 110000 {
		entries = append(entries, payloadEntry{header: cpio.Header{Name: fmt.Sprintf("./files/%06d", i), Mode: cpio.ModeRegular | 0o644, NLink: 1}})
	}
	entries = append(entries, plistEntry(t, "./Example.app/Contents/Info.plist", "Example"))
	payload := cpioPayload(t, entries)
	apps, err := readPayload(t.Context(), io.NopCloser(bytes.NewReader(payload)), newPayloadBudget(), false)
	if err != nil || len(apps) != 1 {
		t.Fatalf("large inventory: %d apps, %v", len(apps), err)
	}
}

func TestPackageRepeatedResources(t *testing.T) {
	for _, repeated := range []string{"artwork", "different"} {
		t.Run(repeated, func(t *testing.T) {
			name := writePayloadPackage(t, []payloadMember{
				{"PackageInfo", []byte(`<pkg-info identifier="org.example.app" version="1" install-location="/Applications/"/>`)},
				{"Resources/background.png", []byte("artwork")},
				{"Resources/background.png", []byte(repeated)},
			})
			facts, err := InspectPackage(name)
			if repeated != "artwork" {
				if err == nil {
					t.Fatal("conflicting resources accepted")
				}
				return
			}
			if err != nil || facts.Packages[0].InstallLocation != "/Applications" {
				t.Fatalf("repeated artwork: %+v %v", facts, err)
			}
			// Entry checksums are checked before the absent signature is reported.
			if _, err := VerifyPackage(t.Context(), name, signature.Signer{}); err == nil || !strings.Contains(err.Error(), "not signed") {
				t.Fatalf("repeated artwork integrity: %v", err)
			}
			data := readTestFile(t, name)
			data[len(data)-1] ^= 1
			writeTestFile(t, name, data, 0o600)
			if _, err := InspectPackage(name); err == nil {
				t.Fatal("corrupt duplicate escaped verification")
			}
		})
	}
}

func TestPBZXPayloadPreservesLimitsAndTrailingChecks(t *testing.T) {
	cpio := cpioPayload(t, []payloadEntry{plistEntry(t, "./Example.app/Contents/Info.plist", "Example")})
	payload := compressPayload(t, "pbzx", cpio)
	for _, test := range []struct {
		name  string
		data  []byte
		limit int64
	}{
		{"trailing data", append(bytes.Clone(payload), []byte("trailing")...), maxEntrySize},
		{"aggregate payload limit", payload, int64(len(cpio) - 1)},
		{"CPIO early failure", compressPayload(t, "pbzx", []byte("invalid CPIO payload")), maxEntrySize},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := newPayloadBudget()
			budget.bytes = test.limit
			input, output := io.Pipe()
			done := make(chan error, 1)
			go func() {
				_, err := output.Write(test.data)
				_ = output.Close()
				done <- err
			}()
			if _, err := readPayload(t.Context(), input, budget, false); err == nil {
				t.Fatal("accepted invalid PBZX payload")
			}
			if err := <-done; err != nil && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("payload producer: %v", err)
			}
		})
	}
}

func TestPBZXPayloadCancellationClosesSource(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	input, output := io.Pipe()
	defer func() { _ = output.Close() }()
	var header [28]byte
	copy(header[:], "pbzx")
	binary.BigEndian.PutUint64(header[4:], pbzx.DefaultBlockSize)
	binary.BigEndian.PutUint64(header[12:], 1024)
	binary.BigEndian.PutUint64(header[20:], 256)
	written := make(chan error, 1)
	go func() { _, err := output.Write(header[:]); written <- err }()
	done := make(chan error, 1)
	go func() { _, err := readPayload(ctx, input, newPayloadBudget(), false); done <- err }()
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled payload: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("payload did not stop its blocked source and workers")
	}
	if _, err := output.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("source left open: %v", err)
	}
}

func TestPBZXPayloadEarlyFailureClosesSource(t *testing.T) {
	data := compressPayload(t, "pbzx", bytes.Repeat([]byte("invalid CPIO"), 100))
	input, output := io.Pipe()
	defer func() { _ = output.Close() }()
	written := make(chan error, 1)
	go func() { _, err := output.Write(data); written <- err }()
	done := make(chan error, 1)
	go func() { _, err := readPayload(t.Context(), input, newPayloadBudget(), false); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "CPIO") {
			t.Fatalf("payload failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CPIO failure waited for the blocked PBZX source")
	}
	if err := <-written; err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte{1}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("source left open: %v", err)
	}
}
