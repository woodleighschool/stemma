package source

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

func TestWingetMetadataLockAndColdReplay(t *testing.T) {
	body := []byte("synthetic Windows installer")
	digest := fmt.Sprintf("%x", sha256.Sum256(body))
	manifest := wingetTestManifest(`InstallerType: inno
Scope: machine
InstallerLocale: en-US
InstallerSwitches:
  Silent: /root-silent
  Log: /root-log
Installers:
- Architecture: x64
  Scope: user
  InstallerSwitches:
    Silent: /user-silent
  InstallerUrl: https://vendor.test/user.exe
  InstallerSha256: "` + digest + `"
- Architecture: x64
  InstallerUrl: https://vendor.test/machine.exe
  InstallerSha256: "` + digest + `"
`)
	responses := wingetTestSource(t, manifest)
	m := manager(t)
	metadata := 0
	m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		metadata++
		data, ok := responses[req.URL.Path]
		if !ok {
			t.Fatalf("update tried to download %s", req.URL)
		}
		return wingetTestResponse(data), nil
	})
	input := plugin.Input{Resolver: "winget", Config: map[string]any{"package": "Example.Tool", "scope": "user"}}
	entry, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if metadata != 3 || entry.Content.SHA256 != digest || entry.Content.Filename != "user.exe" {
		t.Fatalf("metadata=%d entry=%+v", metadata, entry)
	}
	var evidence map[string]any
	if err := json.Unmarshal(entry.Evidence["winget.installer"], &evidence); err != nil {
		t.Fatal(err)
	}
	switches := evidence["installer_switches"].(map[string]any)
	if evidence["scope"] != "user" || switches["silent"] != "/user-silent" || switches["log"] != "/root-log" || switches["silent_with_progress"] != nil {
		t.Fatalf("incorrect declared inheritance: %v", evidence)
	}
	input.Config["scope"] = "machine"
	machine, err := m.Resolve(t.Context(), input)
	if err != nil || machine.Content.Filename != "machine.exe" || metadata != 3 {
		t.Fatalf("shared snapshot: %v %v metadata=%d", machine, err, metadata)
	}
	input.Config["scope"] = "user"
	cold := New(m.Store, m.Root, false)
	downloads := 0
	cold.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://vendor.test/user.exe" {
			t.Fatalf("frozen replay discovered metadata: %s", req.URL)
		}
		downloads++
		return wingetTestResponse(body), nil
	})
	if _, err := cold.FetchLocked(t.Context(), input, entry); err != nil {
		t.Fatal(err)
	}
	if downloads != 1 {
		t.Fatalf("downloads=%d", downloads)
	}
}

func TestWingetDigestChain(t *testing.T) {
	for _, stage := range []string{"versions", "manifest"} {
		t.Run(stage, func(t *testing.T) {
			manifest := wingetTestManifest("InstallerType: msi\nInstallers:\n- Architecture: x64\n  InstallerUrl: https://vendor.test/tool.msi\n  InstallerSha256: " + strings.Repeat("a", 64) + "\n")
			responses := wingetTestSource(t, manifest)
			m := manager(t)
			m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				data := responses[req.URL.Path]
				if stage == "versions" && strings.HasSuffix(req.URL.Path, ".mszyml") || stage == "manifest" && strings.Contains(req.URL.Path, "/manifests/") {
					data = append(append([]byte{}, data...), '\n')
				}
				return wingetTestResponse(data), nil
			})
			_, err := m.Resolve(t.Context(), plugin.Input{Resolver: "winget", Config: map[string]any{"package": "Example.Tool"}})
			if err == nil || !strings.Contains(err.Error(), "SHA256 mismatch") {
				t.Fatalf("unverified %s accepted: %v", stage, err)
			}
		})
	}
}

func TestWingetSelectionAndTransport(t *testing.T) {
	base := `InstallerType: msi
Scope: machine
PackageLocale: en-US
Installers:
- Architecture: x64
  InstallerUrl: https://vendor.test/tool.msi
  InstallerSha256: ` + strings.Repeat("a", 64) + "\n"
	for _, test := range []struct {
		name, patch string
		config      wingetConfig
		failure     string
	}{
		{"one", "", wingetConfig{}, ""},
		{"locale is absent", "", wingetConfig{Locale: "en-US"}, "0 installers match"},
		{"exact architecture", "", wingetConfig{Architecture: "arm64"}, "0 installers match"},
		{"exact type", "", wingetConfig{InstallerType: "wix"}, "0 installers match"},
		{"ambiguous", "- Architecture: x64\n  InstallerUrl: https://vendor.test/other.msi\n  InstallerSha256: " + strings.Repeat("b", 64) + "\n", wingetConfig{}, "2 installers match"},
		{"authentication", "  Authentication:\n    AuthenticationType: microsoftEntraId\n", wingetConfig{}, "authenticated"},
		{"prohibited", "  DownloadCommandProhibited: true\n", wingetConfig{}, "prohibits"},
		{"store", "  InstallerType: msstore\n", wingetConfig{}, "unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := test.config
			config.Package = "Example.Tool"
			if config.Architecture == "" {
				config.Architecture = "x64"
			}
			found, err := selectWinget(wingetTestManifest(base+test.patch), config, "2.0")
			if test.failure != "" {
				if err == nil || !strings.Contains(err.Error(), test.failure) {
					t.Fatalf("expected %q: %v", test.failure, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(found.Evidence["winget.installer"]), "installer_locale") {
				t.Fatal("package locale became installer locale")
			}
		})
	}
}

func TestWingetTypeDependentInheritance(t *testing.T) {
	rootYAML := `InstallerType: zip
NestedInstallerType: msi
NestedInstallerFiles:
- RelativeFilePath: payload/setup.msi
ProductCode: root-product
PackageFamilyName: root-family
AppsAndFeaturesEntries:
- DisplayName: Root App
Dependencies:
  WindowsFeatures: [RootFeature]
InstallerSuccessCodes: [0, 3010]
ExpectedReturnCodes:
- InstallerReturnCode: 1603
  ReturnResponse: installInProgress
InstallationMetadata:
  DefaultInstallLocation: root-location
  Files:
  - RelativeFilePath: tool.exe
`
	for _, test := range []struct {
		name, leaf string
		check      func(*testing.T, map[string]any)
	}{
		{"zip defaults", "Architecture: x64", func(t *testing.T, result map[string]any) {
			t.Helper()
			if result["nested_installer_type"] != "msi" || result["product_code"] != "root-product" || result["package_family_name"] != nil || result["apps_and_features_entries"] == nil {
				t.Fatalf("ZIP inheritance: %v", result)
			}
		}},
		{"msix", "Architecture: arm64\nInstallerType: msix", func(t *testing.T, result map[string]any) {
			t.Helper()
			if result["nested_installer_type"] != nil || result["nested_installer_files"] != nil || result["product_code"] != nil || result["apps_and_features_entries"] != nil || result["package_family_name"] != "root-family" {
				t.Fatalf("MSIX inherited incompatible defaults: %v", result)
			}
		}},
		{"replace dependencies and lists", "Dependencies:\n  ExternalDependencies: [LeafDependency]\nInstallerSuccessCodes: [123]\nExpectedReturnCodes: []\nInstallationMetadata:\n  DefaultInstallLocation: leaf-location", func(t *testing.T, result map[string]any) {
			t.Helper()
			deps := result["dependencies"].(map[string]any)
			metadata := result["installation_metadata"].(map[string]any)
			if deps["windows_features"] != nil || deps["external_dependencies"] == nil || len(result["installer_success_codes"].([]any)) != 1 || len(result["expected_return_codes"].([]any)) != 0 || metadata["default_install_location"] != "leaf-location" || metadata["files"] == nil {
				t.Fatalf("override mismatch: %v", result)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var root, leaf wingetFields
			if err := yaml.Unmarshal([]byte(rootYAML), &root); err != nil {
				t.Fatal(err)
			}
			if err := yaml.Unmarshal([]byte(test.leaf), &leaf); err != nil {
				t.Fatal(err)
			}
			claims, err := effectiveWingetInstaller(root, leaf)
			if err != nil {
				t.Fatal(err)
			}
			result, err := wingetEvidence(claims)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, result)
		})
	}
}

func TestWingetDecompression(t *testing.T) {
	data := bytes.Repeat([]byte("shared dictionary metadata across block boundaries\n"), 3000)
	compressed := wingetTestCompress(t, data)
	decoded, err := decompressWinget(compressed)
	if err != nil || !bytes.Equal(decoded, data) {
		t.Fatalf("multiblock decode: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"header", func(data []byte) []byte { data[0] ^= 1; return data }},
		{"checksum", func(data []byte) []byte { data[6] ^= 1; return data }},
		{"algorithm", func(data []byte) []byte { data[7] = 3; return data }},
		{"truncated header", func(data []byte) []byte { return data[:20] }},
		{"truncated block", func(data []byte) []byte { return data[:len(data)-1] }},
		{"bad block length", func(data []byte) []byte { binary.LittleEndian.PutUint32(data[24:], ^uint32(0)); return data }},
		{"trailing data", func(data []byte) []byte { return append(data, 0) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decompressWinget(test.mutate(bytes.Clone(compressed))); err == nil {
				t.Fatal("malformed MSZIP accepted")
			}
		})
	}
}

func wingetTestManifest(body string) []byte {
	return []byte("PackageIdentifier: Example.Tool\nPackageVersion: '2.0'\nManifestVersion: 1.12.0\nManifestType: merged\n" + body)
}

func wingetTestCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	result := []byte{10, 81, 229, 192, 24, 0, 0, 2}
	result = binary.LittleEndian.AppendUint64(result, uint64(len(data)))
	result = binary.LittleEndian.AppendUint64(result, uint64(min(len(data), 32768)))
	result[6] = byte(crc32.Update(crc32.ChecksumIEEE(result[:6]), crc32.IEEETable, result[7:24]) & 0xff)
	for offset := 0; offset < len(data); offset += 32768 {
		var block bytes.Buffer
		writer, err := flate.NewWriterDict(&block, flate.BestCompression, data[max(0, offset-32768):offset])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(data[offset:min(offset+32768, len(data))]); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		result = binary.LittleEndian.AppendUint32(result, uint32(block.Len()+2))
		result = append(result, 'C', 'K')
		result = append(result, block.Bytes()...)
	}
	return result
}

func wingetTestSource(t *testing.T, manifest []byte) map[string][]byte {
	t.Helper()
	manifestHash := fmt.Sprintf("%x", sha256.Sum256(manifest))
	versions := wingetTestCompress(t, []byte("sV: '1.0'\nvD:\n- v: '2.0'\n  rP: manifests/e/Example/Tool/2.0/test\n  s256H: "+manifestHash+"\n"))
	digest := sha256.Sum256(versions)
	versionHash := hex.EncodeToString(digest[:])
	filename := filepath.Join(t.TempDir(), "index.db")
	db, err := sql.Open("sqlite3", (&url.URL{Scheme: "file", Path: filename}).String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), `CREATE TABLE metadata (name TEXT PRIMARY KEY, value TEXT); INSERT INTO metadata VALUES ('majorVersion','2'),('minorVersion','0'); CREATE TABLE packages (id TEXT, latest_version TEXT, hash BLOB);`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(t.Context(), "INSERT INTO packages VALUES (?,?,?)", "Example.Tool", "2.0", digest[:]); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("Public/index.db")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return map[string][]byte{
		"/cache/source2.msix": archive.Bytes(),
		"/cache/packages/Example.Tool/" + versionHash[:8] + "/versionData.mszyml": versions,
		"/cache/manifests/e/Example/Tool/2.0/test":                                manifest,
	}
}

func wingetTestResponse(data []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(data))}
}

func TestWingetRefreshUsesPublishedMetadataHashes(t *testing.T) {
	manifest := wingetTestManifest("InstallerType: msi\nInstallers:\n- Architecture: x64\n  InstallerUrl: https://vendor.test/tool.msi\n  InstallerSha256: " + strings.Repeat("a", 64) + "\n")
	responses := wingetTestSource(t, manifest)
	m := manager(t)
	requests := 0
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		body, ok := responses[req.URL.Path]
		if !ok {
			t.Fatalf("unexpected download: %s", req.URL)
		}
		response := wingetTestResponse(body)
		response.Header.Set("ETag", `"source-one"`)
		return response, nil
	})
	m.Client.Transport = transport
	input := plugin.Input{Resolver: "winget", Config: map[string]any{"package": "example.tool"}}
	first, err := m.Resolve(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("cold metadata requests=%d", requests)
	}

	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprintf("changed=%t", changed), func(t *testing.T) {
			fresh := New(m.Store, m.Root, false)
			count := 0
			fresh.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				count++
				if req.URL.Path != "/cache/source2.msix" {
					t.Fatalf("refetched digest-pinned metadata: %s", req.URL)
				}
				if req.Header.Get("If-None-Match") != `"source-one"` {
					t.Fatal("source was not revalidated")
				}
				if !changed {
					return &http.Response{StatusCode: http.StatusNotModified, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				archive, err := zip.NewReader(bytes.NewReader(responses[req.URL.Path]), int64(len(responses[req.URL.Path])))
				if err != nil {
					t.Fatal(err)
				}
				var body bytes.Buffer
				writer := zip.NewWriter(&body)
				for _, file := range archive.File {
					if err := writer.Copy(file); err != nil {
						t.Fatal(err)
					}
				}
				if err := writer.SetComment("a new source snapshot with the same selected package"); err != nil {
					t.Fatal(err)
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				return wingetTestResponse(body.Bytes()), nil
			})
			current, err := fresh.Resolve(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if count != 1 || !current.Equal(first) {
				t.Fatalf("unchanged selected publication: %d %+v", count, current)
			}
		})
	}
}

func TestWingetVersionSelectedBeforeInstaller(t *testing.T) {
	newManifest := wingetTestManifest("InstallerType: msi\nInstallers:\n- Architecture: arm64\n  InstallerUrl: https://vendor.test/new.msi\n  InstallerSha256: " + strings.Repeat("a", 64) + "\n")
	oldManifest := []byte(strings.ReplaceAll(strings.ReplaceAll(string(newManifest), "'2.0'", "'1.0'"), "arm64", "x64"))
	newHash, oldHash := sha256.Sum256(newManifest), sha256.Sum256(oldManifest)
	versions := wingetTestCompress(t, []byte(fmt.Sprintf("sV: '1.0'\nvD:\n- v: '1.0'\n  rP: manifests/e/Example/Tool/1.0/test\n  s256H: %x\n- v: '2.0'\n  rP: manifests/e/Example/Tool/2.0/test\n  s256H: %x\n", oldHash, newHash)))
	versionHash := fmt.Sprintf("%x", sha256.Sum256(versions))
	for _, version := range []string{"latest", "1.0"} {
		t.Run(version, func(t *testing.T) {
			m := manager(t)
			m.winget.packages = map[string]wingetPackage{"example.tool": {ID: "Example.Tool", Latest: "2.0", SHA256: versionHash}}
			m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, ".mszyml") {
					return wingetTestResponse(versions), nil
				}
				if strings.Contains(req.URL.Path, "/1.0/") {
					if version == "latest" {
						t.Fatal("latest fell back to an older installer")
					}
					return wingetTestResponse(oldManifest), nil
				}
				return wingetTestResponse(newManifest), nil
			})
			entry, err := m.Resolve(t.Context(), plugin.Input{Resolver: "winget", Config: map[string]any{"package": "Example.Tool", "version": version}})
			if version == "latest" {
				if err == nil || !strings.Contains(err.Error(), "0 installers match") {
					t.Fatalf("latest: %v", err)
				}
			} else if err != nil || !strings.Contains(string(entry.Observation), `"version":"1.0"`) {
				t.Fatalf("exact version: %+v %v", entry, err)
			}
		})
	}
}

func TestWingetRejectsSignedInstallerURL(t *testing.T) {
	manifest := wingetTestManifest("InstallerType: msi\nInstallers:\n- Architecture: x64\n  InstallerUrl: https://vendor.test/tool.msi?token=secret\n  InstallerSha256: " + strings.Repeat("a", 64) + "\n")
	_, err := selectWinget(manifest, wingetConfig{Package: "Example.Tool", Architecture: "x64"}, "2.0")
	if err == nil || !strings.Contains(err.Error(), "credentials or expiration") {
		t.Fatalf("signed URL: %v", err)
	}
}

func TestWingetRefreshDoesNotUseStaleIndex(t *testing.T) {
	manifest := wingetTestManifest("InstallerType: msi\nInstallers:\n- Architecture: x64\n  InstallerUrl: https://vendor.test/tool.msi\n  InstallerSha256: " + strings.Repeat("a", 64) + "\n")
	responses := wingetTestSource(t, manifest)
	m := manager(t)
	m.Client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return wingetTestResponse(responses[req.URL.Path]), nil
	})
	input := plugin.Input{Resolver: "winget", Config: map[string]any{"package": "Example.Tool"}}
	if _, err := m.Resolve(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	fresh := New(m.Store, m.Root, false)
	fresh.Client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("unavailable"))}, nil
	})
	if _, err := fresh.Resolve(t.Context(), input); err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("stale index reused: %v", err)
	}
}

// This public WinGet metadata sample was compressed by Windows, independently of the test writer.
func TestWingetWindowsCompressionFixture(t *testing.T) {
	data, err := hex.DecodeString("0a51e5c018000c029c000000000000009c0000000000000083000000434b55cb310ec2300c40d13da7c8054852bbb5e3ac20c1c8c49ed46941a2546a50cf4f57c62fbddf1ec9762e98fd92ccc9ee470cbd0b2e066427d158bbdd935df2e735d5f66d7ef6d7759ddfd59f9fdbba54ffa7bd8ec2c7d260a05bb25151870ca25dd1125461ea30c75e1422414122c4ca30e52c4158f24891b810280301d691cd0f")
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decompressWinget(data)
	if err != nil {
		t.Fatal(err)
	}
	if actual := fmt.Sprintf("%x", sha256.Sum256(decoded)); actual != "651931d271096683e03bb775dcf8fecd3893a35742963be02af1c71fb96df9c2" {
		t.Fatalf("independent fixture checksum=%s", actual)
	}
}

func TestWingetRechecksCachedMetadataDigest(t *testing.T) {
	m := manager(t)
	data := []byte("reviewed metadata")
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	key, err := fingerprint(struct {
		Kind   string `json:"kind"`
		SHA256 string `json:"sha256"`
	}{"winget.metadata/1", digest})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Store.RememberSource(key, []byte("corrupted cache")); err != nil {
		t.Fatal(err)
	}
	requests := 0
	m.Client.Transport = roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		requests++
		return wingetTestResponse([]byte("incorrect server response")), nil
	})
	if _, err := m.wingetMetadata(t.Context(), "manifests/example", digest); err == nil || !strings.Contains(err.Error(), "SHA256 mismatch") {
		t.Fatalf("corrupt cache or response accepted: %v", err)
	}
	if requests != 1 {
		t.Fatalf("cache was not rechecked: requests=%d", requests)
	}
}
