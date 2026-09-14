package apple

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"

	"github.com/deploymenttheory/go-macos-pkg/pkg/pbzx"
	"github.com/mikelolasagasti/xz"
	"github.com/woodleighschool/stemma/internal/fileio"
)

const maxPayloadEntries = 1000000
const maxPackageMetadata = 32 << 20
const maxPayloadPadding = 1 << 20

type payloadBudget struct {
	metadata  int64
	bytes     int64
	entries   int
	pathBytes int64
}

func newPayloadBudget() *payloadBudget {
	return &payloadBudget{metadata: maxPackageMetadata, bytes: maxEntrySize, entries: maxPayloadEntries}
}

// PackageApp records a static application in a component's payload. InstalledPath
// combines the declared install location and payload path; scripts may change it.
type PackageApp struct {
	PackagePath   string   `json:"package_path"`
	Path          string   `json:"path"`
	InstalledPath string   `json:"installed_path,omitempty"`
	App           AppFacts `json:"app"`
}

// InspectPackageContents reads receipts and application Info.plists in flat
// packages. Payloads support ODC CPIO, optionally compressed with gzip, XZ or
// 16 MiB PBZX chunks decoded concurrently with bounded read-ahead. Unsupported
// layouts fail without returning partial facts.
// Inspection never extracts payloads, executes scripts or consults target paths.
func InspectPackageContents(ctx context.Context, filePath string) (PackageFacts, error) {
	return inspectPackage(ctx, filePath, true)
}

// InspectPackageMetadata reads bounded component receipts without reading
// payload contents or asserting that a package's complete contents are supported.
func InspectPackageMetadata(ctx context.Context, filePath string) (PackageFacts, error) {
	return inspectPackage(ctx, filePath, false)
}

func inspectPackage(ctx context.Context, filePath string, contents bool) (PackageFacts, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return PackageFacts{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return PackageFacts{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxEntrySize {
		return PackageFacts{}, fmt.Errorf("PKG input must be a regular file no larger than 16 GiB")
	}
	return inspectPackageReader(ctx, f, info.Size(), contents)
}

// InspectPackageReader inspects receipts and applications in a sized flat package
// without requiring the package to be materialized as a local file.
func InspectPackageReader(ctx context.Context, reader io.ReaderAt, size int64) (PackageFacts, error) {
	return inspectPackageReader(ctx, reader, size, true)
}

func inspectPackageReader(ctx context.Context, reader io.ReaderAt, size int64, contents bool) (PackageFacts, error) {
	if size < 0 || size > maxEntrySize {
		return PackageFacts{}, fmt.Errorf("PKG exceeds size limit")
	}
	archive, err := openXAR(contextReaderAt{ctx, reader}, size)
	if err != nil {
		return PackageFacts{}, err
	}
	facts := PackageFacts{Format: "xar", Entries: archive.entries, HasSignature: archive.reader.TOC().Signature != nil || archive.reader.TOC().XSignature != nil}
	payloads := make(map[string]bool)
	budget := newPayloadBudget()
	for _, entry := range archive.entries {
		if path.Base(entry.Path) != "PackageInfo" || entry.Type != "file" {
			continue
		}
		if entry.Size > budget.metadata {
			return PackageFacts{}, fmt.Errorf("package metadata exceeds read limit")
		}
		budget.metadata -= entry.Size
		metadata, err := archive.packageInfo(entry.Path)
		if err != nil {
			return PackageFacts{}, fmt.Errorf("%s: %w", entry.Path, err)
		}
		facts.Packages = append(facts.Packages, metadata)
		if !contents || !metadata.HasPayload {
			continue
		}
		payloadPath := path.Join(path.Dir(entry.Path), "Payload")
		payloads[payloadPath] = true
		apps, err := archive.payloadApps(ctx, payloadPath, budget, strings.HasSuffix(metadata.InstallLocation, ".app"))
		if err != nil {
			return PackageFacts{}, fmt.Errorf("%s: %w", payloadPath, err)
		}
		for _, app := range apps {
			app.PackagePath = entry.Path
			if metadata.InstallLocation != "" {
				app.InstalledPath = path.Join(metadata.InstallLocation, app.Path)
			}
			app.Path = path.Join(payloadPath, app.Path)
			facts.Applications = append(facts.Applications, app)
		}
	}
	if len(facts.Packages) == 0 {
		return PackageFacts{}, fmt.Errorf("%w: PKG contains no component PackageInfo", ErrUnsupported)
	}
	if !contents {
		return facts, nil
	}
	for _, entry := range archive.entries {
		if path.Base(entry.Path) == "Distribution" {
			if entry.Size > budget.metadata {
				return PackageFacts{}, fmt.Errorf("package metadata exceeds read limit")
			}
			budget.metadata -= entry.Size
			if err := archive.validateDistribution(entry.Path); err != nil {
				return PackageFacts{}, err
			}
		}
		if path.Base(entry.Path) == "Payload" && !payloads[entry.Path] {
			return PackageFacts{}, fmt.Errorf("%w: payload %q has no component receipt", ErrUnsupported, entry.Path)
		}
		if entry.Type == "file" && strings.HasSuffix(strings.ToLower(entry.Path), ".pkg") {
			return PackageFacts{}, fmt.Errorf("%w: nested package file %q", ErrUnsupported, entry.Path)
		}
	}
	return facts, ctx.Err()
}

func (a *xarArchive) validateDistribution(name string) error {
	var data bytes.Buffer
	if err := a.readEntry(name, &data, maxMetadata); err != nil {
		return err
	}
	if err := validatePackageXML(data.Bytes()); err != nil {
		return fmt.Errorf("distribution XML: %w", err)
	}
	decoder := xml.NewDecoder(&data)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		element, ok := token.(xml.StartElement)
		if !ok || element.Name.Local != "pkg-ref" {
			continue
		}
		var reference string
		if err := decoder.DecodeElement(&reference, &element); err != nil {
			return err
		}
		reference = strings.TrimSpace(reference)
		if reference == "" {
			continue
		}
		reference, err = url.PathUnescape(strings.TrimPrefix(reference, "#"))
		if err != nil || reference == "" || strings.ContainsAny(reference, ":\\\x00") || path.IsAbs(reference) || path.Clean(reference) != reference || reference == ".." || strings.HasPrefix(reference, "../") {
			return fmt.Errorf("%w: Distribution package reference %q", ErrUnsupported, reference)
		}
		component := path.Join(path.Dir(name), reference)
		file, exists := a.files[component]
		if !exists || file.Type.Value != "directory" {
			return fmt.Errorf("%w: Distribution package reference %q is not an embedded component directory", ErrUnsupported, reference)
		}
		if _, exists := a.files[path.Join(component, "PackageInfo")]; !exists {
			return fmt.Errorf("%w: Distribution component %q has no receipt", ErrUnsupported, reference)
		}
	}
}

func (a *xarArchive) packageInfo(name string) (PackageInfo, error) {
	var data bytes.Buffer
	if err := a.readEntry(name, &data, maxMetadata); err != nil {
		return PackageInfo{}, err
	}
	if err := validatePackageXML(data.Bytes()); err != nil {
		return PackageInfo{}, fmt.Errorf("PackageInfo XML: %w", err)
	}
	var document struct {
		PackageInfo

		XMLName  xml.Name `xml:"pkg-info"`
		Payloads []struct {
			Size int64 `xml:"installKBytes,attr"`
		} `xml:"payload"`
	}
	if err := xml.Unmarshal(data.Bytes(), &document); err != nil {
		return PackageInfo{}, err
	}
	metadata := document.PackageInfo
	metadata.Path = name
	if metadata.Identifier == "" || metadata.Version == "" {
		return PackageInfo{}, fmt.Errorf("PackageInfo requires identifier and version")
	}
	metadata.InstallLocation = strings.TrimRight(metadata.InstallLocation, "/")
	if metadata.InstallLocation == "" && document.InstallLocation != "" {
		metadata.InstallLocation = "/"
	}
	if metadata.InstallLocation != "" && (!path.IsAbs(metadata.InstallLocation) || strings.ContainsAny(metadata.InstallLocation, "\\\x00") || path.Clean(metadata.InstallLocation) != metadata.InstallLocation) {
		return PackageInfo{}, fmt.Errorf("unsafe PackageInfo install location %q", metadata.InstallLocation)
	}
	if len(document.Payloads) > 1 {
		return PackageInfo{}, fmt.Errorf("duplicate PackageInfo payload metadata")
	}
	payload, exists := a.files[path.Join(path.Dir(name), "Payload")]
	if exists && payload.Type.Value != "file" {
		return PackageInfo{}, fmt.Errorf("component Payload is not a regular file")
	}
	metadata.HasPayload = exists
	if len(document.Payloads) == 1 {
		metadata.InstalledSize = document.Payloads[0].Size
		if metadata.InstalledSize < 0 || metadata.InstalledSize > maxEntrySize/1024 {
			return PackageInfo{}, fmt.Errorf("invalid PackageInfo installed size")
		}
		if !metadata.HasPayload {
			return PackageInfo{}, fmt.Errorf("PackageInfo declares a missing Payload")
		}
	}
	return metadata, nil
}

type contextReaderAt struct {
	context.Context
	io.ReaderAt
}

func (r contextReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if err := r.Err(); err != nil {
		return 0, err
	}
	return r.ReaderAt.ReadAt(p, off)
}

func (a *xarArchive) payloadApps(ctx context.Context, name string, budget *payloadBudget, applicationRoot bool) ([]PackageApp, error) {
	r, w := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := a.readEntry(name, w, maxEntrySize)
		_ = w.CloseWithError(err)
		done <- err
	}()
	apps, err := readPayload(ctx, r, budget, applicationRoot)
	_ = r.CloseWithError(err)
	readErr := <-done
	if err != nil {
		return nil, err
	}
	return apps, readErr
}

func readPayload(ctx context.Context, source io.ReadCloser, budget *payloadBudget, applicationRoot bool) ([]PackageApp, error) {
	stop := context.AfterFunc(ctx, func() { _ = source.Close() })
	defer stop()
	defer func() { _ = source.Close() }()
	r := bufio.NewReader(fileio.Reader{Context: ctx, Reader: source})
	header, err := r.Peek(6)
	if err != nil {
		return nil, err
	}
	var content io.Reader
	switch {
	case string(header) == "070707":
		content = r
	case header[0] == 0x1f && header[1] == 0x8b:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		content = gz
	case string(header) == "\xfd7zXZ\x00":
		content, err = xz.NewReader(r, 64<<20)
	case string(header[:4]) == "pbzx":
		header, err := r.Peek(12)
		if err != nil {
			return nil, err
		}
		if binary.BigEndian.Uint64(header[4:]) != pbzx.DefaultBlockSize {
			return nil, fmt.Errorf("%w: PBZX chunk size", ErrUnsupported)
		}
		reader, err := pbzx.NewConcurrentReader(ctx, r, min(runtime.GOMAXPROCS(0), 4))
		if err != nil {
			return nil, err
		}
		defer func() {
			// Unblock the library's source reader before joining its workers.
			_ = source.Close()
			_ = reader.Close()
		}()
		content = reader
	default:
		return nil, fmt.Errorf("%w: PKG payload compression or archive format", ErrUnsupported)
	}
	if err != nil {
		return nil, err
	}
	limited := &io.LimitedReader{R: content, N: budget.bytes + 1}
	apps, err := readCPIO(limited, budget, applicationRoot)
	budget.bytes = limited.N - 1
	if err != nil {
		return nil, err
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("PKG payload exceeds read limit")
	}
	if _, err := r.Peek(1); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("trailing PKG payload data")
		}
		return nil, err
	}
	return apps, nil
}

func readCPIO(r io.Reader, budget *payloadBudget, applicationRoot bool) ([]PackageApp, error) {
	var apps []PackageApp
	seen := make(map[string]uint64)
	for {
		if budget.entries <= 0 {
			return nil, fmt.Errorf("PKG payload exceeds entry limit")
		}
		budget.entries--
		var header [76]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return nil, fmt.Errorf("CPIO header: %w", err)
		}
		if string(header[:6]) != "070707" {
			return nil, fmt.Errorf("%w: PKG requires ODC CPIO payload", ErrUnsupported)
		}
		var fields [10]uint64
		offset := 6
		for i, width := range []int{6, 6, 6, 6, 6, 6, 6, 11, 6, 11} {
			v, err := strconv.ParseUint(string(header[offset:offset+width]), 8, 64)
			if err != nil {
				return nil, fmt.Errorf("CPIO header: %w", err)
			}
			fields[i] = v
			offset += width
		}
		mode, links, nameSize, size := fields[2]&0170000, fields[5], fields[8], fields[9]
		if nameSize < 2 || nameSize > 4096 || size > maxEntrySize {
			return nil, fmt.Errorf("invalid or oversized CPIO entry")
		}
		nameData := make([]byte, nameSize)
		if _, err := io.ReadFull(r, nameData); err != nil {
			return nil, err
		}
		if nameData[len(nameData)-1] != 0 {
			return nil, fmt.Errorf("CPIO path lacks terminator")
		}
		name := string(nameData[:len(nameData)-1])
		if name == "TRAILER!!!" {
			if size != 0 {
				return nil, fmt.Errorf("CPIO trailer has content")
			}
			if err := readPadding(r); err != nil {
				return nil, err
			}
			break
		}
		name = strings.TrimPrefix(name, "./")
		if name == "" || path.IsAbs(name) || strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return nil, fmt.Errorf("unsafe CPIO path %q", name)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate CPIO path %q", name)
		}
		if name == "." && mode != 0040000 {
			return nil, fmt.Errorf("CPIO root is not a directory")
		}
		budget.pathBytes += int64(len(name))
		if budget.pathBytes > 128<<20 {
			return nil, fmt.Errorf("PKG payload paths exceed memory limit")
		}
		seen[name] = mode
		if strings.HasSuffix(name, ".app/Contents/Info.plist") || applicationRoot && name == "Contents/Info.plist" {
			if mode != 0100000 || links > 1 {
				return nil, fmt.Errorf("%w: application Info.plist must be a regular, unlinked file", ErrUnsupported)
			}
			if size > maxMetadata || int64(size) > budget.metadata {
				return nil, fmt.Errorf("application metadata exceeds read limit")
			}
			budget.metadata -= int64(size)
			data := make([]byte, size)
			if _, err := io.ReadFull(r, data); err != nil {
				return nil, err
			}
			facts, err := ParseAppInfo(data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			appPath := strings.TrimSuffix(name, "/Contents/Info.plist")
			if name == "Contents/Info.plist" {
				appPath = "."
			}
			apps = append(apps, PackageApp{Path: appPath, App: facts})
		} else if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
			return nil, fmt.Errorf("CPIO %s: %w", name, err)
		}
	}
	for _, app := range apps {
		for parent := path.Join(app.Path, "Contents"); parent != "."; parent = path.Dir(parent) {
			if mode, exists := seen[parent]; exists && mode != 0040000 {
				return nil, fmt.Errorf("%w: application ancestor %q is not a directory", ErrUnsupported, parent)
			}
		}
	}
	return apps, nil
}

func readPadding(r io.Reader) error {
	var data [4096]byte
	remaining := maxPayloadPadding
	for {
		n, err := r.Read(data[:])
		remaining -= n
		if remaining < 0 || bytes.Count(data[:n], []byte{0}) != n {
			return fmt.Errorf("invalid or oversized CPIO trailing padding")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func validatePackageXML(data []byte) error {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var parents []map[string]bool
	roots, nodes := 0, 0
	singletons := map[string]bool{"toc": true, "checksum": true, "type": true, "data": true, "offset": true, "size": true, "length": true, "encoding": true, "archived-checksum": true, "extracted-checksum": true, "KeyInfo": true, "X509Data": true}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("package XML: %w", err)
		}
		switch token := token.(type) {
		case xml.StartElement:
			nodes++
			if nodes > 250000 || len(parents) >= 96 {
				return fmt.Errorf("package XML exceeds structural limits")
			}
			if len(parents) == 0 {
				roots++
			} else if singletons[token.Name.Local] {
				parent := parents[len(parents)-1]
				if parent[token.Name.Local] {
					return fmt.Errorf("duplicate package element %q", token.Name.Local)
				}
				parent[token.Name.Local] = true
			}
			attributes := make(map[string]bool)
			for _, attr := range token.Attr {
				if attributes[attr.Name.Local] {
					return fmt.Errorf("duplicate package attribute %q", attr.Name.Local)
				}
				attributes[attr.Name.Local] = true
			}
			parents = append(parents, make(map[string]bool))
		case xml.EndElement:
			parents = parents[:len(parents)-1]
		case xml.CharData:
			if len(parents) == 0 && len(bytes.TrimSpace(token)) != 0 {
				return fmt.Errorf("trailing package XML content")
			}
		}
	}
	if roots != 1 {
		return fmt.Errorf("package XML requires exactly one XML root")
	}
	return nil
}
