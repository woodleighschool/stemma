package apple

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/deploymenttheory/go-macos-pkg/pkg/pbzx"
	"github.com/klauspost/compress/gzip"
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
	distribution := false
	for _, entry := range archive.entries {
		if path.Base(entry.Path) == "Distribution" {
			if entry.Size > budget.metadata {
				return PackageFacts{}, fmt.Errorf("package metadata exceeds read limit")
			}
			budget.metadata -= entry.Size
			declared, err := archive.distribution(entry.Path, contents)
			if err != nil {
				return PackageFacts{}, err
			}
			if !distribution {
				distribution = true
				facts.Version, facts.MinimumOS, facts.RestartAction = declared.Version, declared.MinimumOS, declared.RestartAction
			}
		}
		if !contents {
			continue
		}
		if path.Base(entry.Path) == "Payload" && !payloads[entry.Path] {
			return PackageFacts{}, fmt.Errorf("%w: payload %q has no component receipt", ErrUnsupported, entry.Path)
		}
		if entry.Type == "file" && strings.HasSuffix(strings.ToLower(entry.Path), ".pkg") {
			return PackageFacts{}, fmt.Errorf("%w: nested package file %q", ErrUnsupported, entry.Path)
		}
	}
	var minimums []string
	for _, pkg := range facts.Packages {
		// Installer ignores component postinstall actions within a Distribution.
		if !distribution {
			facts.RestartAction = strongerRestartAction(facts.RestartAction, pkg.RestartAction)
		}
		// Without Distribution requirements, Munki uses receipt declarations;
		// payload-free components record no receipt.
		if pkg.HasPayload {
			minimums = append(minimums, pkg.MinimumOS)
		}
	}
	if facts.MinimumOS == "" {
		if facts.MinimumOS, err = highestOSVersion(minimums...); err != nil {
			return PackageFacts{}, fmt.Errorf("PackageInfo: %w", err)
		}
	}
	return facts, ctx.Err()
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

		XMLName           xml.Name `xml:"pkg-info"`
		PostinstallAction string   `xml:"postinstall-action,attr"`
		Payloads          []struct {
			Size int64 `xml:"installKBytes,attr"`
		} `xml:"payload"`
	}
	if err := xml.Unmarshal(data.Bytes(), &document); err != nil {
		return PackageInfo{}, err
	}
	metadata := document.PackageInfo
	metadata.Path = name
	switch strings.ToLower(document.PostinstallAction) {
	case "logout":
		metadata.RestartAction = "RequireLogout"
	case "restart":
		metadata.RestartAction = "RequireRestart"
	case "shutdown":
		metadata.RestartAction = "RequireShutdown"
	}
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

// Installer's restart query accepts these static values case-insensitively, in
// ascending precedence. None and unknown values impose no requirement.
var restartActions = []string{"RecommendRestart", "RequireLogout", "RequireRestart", "RequireShutdown"}

func strongerRestartAction(current, declared string) string {
	for i, action := range restartActions {
		if strings.EqualFold(action, declared) && i > slices.Index(restartActions, current) {
			return action
		}
	}
	return current
}

// distribution reads a Distribution's static product declarations. Installer's
// restart query uses every pkg-ref onConclusion, including deselected choices,
// and ignores onConclusionScript.
func (a *xarArchive) distribution(name string, confine bool) (PackageFacts, error) {
	var data bytes.Buffer
	if err := a.readEntry(name, &data, maxMetadata); err != nil {
		return PackageFacts{}, err
	}
	if err := validatePackageXML(data.Bytes()); err != nil {
		return PackageFacts{}, fmt.Errorf("distribution XML: %w", err)
	}
	var document struct {
		Products []struct {
			Version string `xml:"version,attr"`
		} `xml:"product"`
		VolumeChecks []struct {
			AllowedOSVersions []struct {
				OSVersions []struct {
					Minimum string `xml:"min,attr"`
				} `xml:"os-version"`
			} `xml:"allowed-os-versions"`
		} `xml:"volume-check"`
	}
	if err := xml.Unmarshal(data.Bytes(), &document); err != nil {
		return PackageFacts{}, fmt.Errorf("distribution XML: %w", err)
	}
	var declared PackageFacts
	if len(document.Products) > 0 {
		declared.Version = document.Products[0].Version
	}
	// Munki uses the highest minimum within the first allowed version set.
	if len(document.VolumeChecks) > 0 && len(document.VolumeChecks[0].AllowedOSVersions) > 0 {
		var minimums []string
		for _, version := range document.VolumeChecks[0].AllowedOSVersions[0].OSVersions {
			minimums = append(minimums, version.Minimum)
		}
		var err error
		if declared.MinimumOS, err = highestOSVersion(minimums...); err != nil {
			return PackageFacts{}, fmt.Errorf("distribution: %w", err)
		}
	}
	decoder := xml.NewDecoder(&data)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return declared, nil
		}
		if err != nil {
			return PackageFacts{}, err
		}
		element, ok := token.(xml.StartElement)
		if !ok || element.Name.Local != "pkg-ref" {
			continue
		}
		for _, attr := range element.Attr {
			if attr.Name.Local == "onConclusion" {
				declared.RestartAction = strongerRestartAction(declared.RestartAction, attr.Value)
			}
		}
		var reference string
		if err := decoder.DecodeElement(&reference, &element); err != nil {
			return PackageFacts{}, err
		}
		if reference = strings.TrimSpace(reference); confine && reference != "" {
			if err := a.confineReference(name, reference); err != nil {
				return PackageFacts{}, err
			}
		}
	}
}

func (a *xarArchive) confineReference(distribution, reference string) error {
	reference, err := url.PathUnescape(strings.TrimPrefix(reference, "#"))
	if err != nil || reference == "" || strings.ContainsAny(reference, ":\\\x00") || path.IsAbs(reference) || path.Clean(reference) != reference || reference == ".." || strings.HasPrefix(reference, "../") {
		return fmt.Errorf("%w: Distribution package reference %q", ErrUnsupported, reference)
	}
	component := path.Join(path.Dir(distribution), reference)
	file, exists := a.files[component]
	if !exists || file.Type.Value != "directory" {
		return fmt.Errorf("%w: Distribution package reference %q is not an embedded component directory", ErrUnsupported, reference)
	}
	if _, exists := a.files[path.Join(component, "PackageInfo")]; !exists {
		return fmt.Errorf("%w: Distribution component %q has no receipt", ErrUnsupported, reference)
	}
	return nil
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
	entry, err := a.openEntry(name, maxEntrySize)
	if err != nil {
		return nil, err
	}
	defer func() { _ = entry.Close() }()
	// Entry reads observe cancellation, so the decoder is released after reading
	// stops rather than closed concurrently with a read.
	return readPayload(ctx, io.NopCloser(entry), budget, applicationRoot)
}

func readPayload(ctx context.Context, source io.ReadCloser, budget *payloadBudget, applicationRoot bool) ([]PackageApp, error) {
	var apps []PackageApp
	err := streamPayload(ctx, source, budget, func(content io.Reader) (err error) {
		apps, err = readCPIO(content, budget, applicationRoot)
		return err
	})
	return apps, err
}

// streamPayload decodes a component payload, hands the CPIO stream to consume
// and then verifies the stream ended within budget without trailing data.
func streamPayload(ctx context.Context, source io.ReadCloser, budget *payloadBudget, consume func(io.Reader) error) (result error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result = err
		}
	}()
	stop := context.AfterFunc(ctx, func() { _ = source.Close() })
	defer stop()
	defer func() { _ = source.Close() }()
	r := bufio.NewReader(fileio.Reader{Context: ctx, Reader: source})
	header, err := r.Peek(6)
	if err != nil {
		return err
	}
	var content io.Reader
	switch {
	case string(header) == "070707":
		content = r
	case header[0] == 0x1f && header[1] == 0x8b:
		gz, err := gzip.NewReader(r)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		content = gz
	case string(header) == "\xfd7zXZ\x00":
		content, err = xz.NewReader(r, 64<<20)
	case string(header[:4]) == "pbzx":
		header, err := r.Peek(12)
		if err != nil {
			return err
		}
		if binary.BigEndian.Uint64(header[4:]) != pbzx.DefaultBlockSize {
			return fmt.Errorf("%w: PBZX chunk size", ErrUnsupported)
		}
		reader, err := pbzx.NewConcurrentReader(ctx, r, min(runtime.GOMAXPROCS(0), 4))
		if err != nil {
			return err
		}
		defer func() {
			// Unblock the library's source reader before joining its workers.
			_ = source.Close()
			_ = reader.Close()
		}()
		content = reader
	default:
		return fmt.Errorf("%w: PKG payload compression or archive format", ErrUnsupported)
	}
	if err != nil {
		return err
	}
	limited := &io.LimitedReader{R: content, N: budget.bytes + 1}
	err = consume(limited)
	budget.bytes = limited.N - 1
	if err != nil {
		return err
	}
	if limited.N == 0 {
		return fmt.Errorf("PKG payload exceeds read limit")
	}
	if _, err := r.Peek(1); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing PKG payload data")
		}
		return err
	}
	return nil
}

const (
	cpioDirectory = 0040000
	cpioRegular   = 0100000
	cpioSymlink   = 0120000
)

// cpioEntry is one ODC header whose payload-relative name has been validated.
type cpioEntry struct {
	name  string
	mode  uint64
	links uint64
	size  int64
}

func (e cpioEntry) kind() uint64 { return e.mode & 0170000 }

func (e cpioEntry) perm() fs.FileMode { return fs.FileMode(e.mode & 0o777) }

// cpioReader walks an ODC stream within the payload budget, remembering each
// path's type so callers can check bundle ancestry.
type cpioReader struct {
	r      io.Reader
	budget *payloadBudget
	seen   map[string]uint64
}

func newCPIOReader(r io.Reader, budget *payloadBudget) *cpioReader {
	return &cpioReader{r: r, budget: budget, seen: map[string]uint64{}}
}

// next returns the following entry header; the trailer returns [io.EOF].
func (c *cpioReader) next() (cpioEntry, error) {
	if c.budget.entries <= 0 {
		return cpioEntry{}, fmt.Errorf("PKG payload exceeds entry limit")
	}
	c.budget.entries--
	var header [76]byte
	if _, err := io.ReadFull(c.r, header[:]); err != nil {
		return cpioEntry{}, fmt.Errorf("CPIO header: %w", err)
	}
	if string(header[:6]) != "070707" {
		return cpioEntry{}, fmt.Errorf("%w: PKG requires ODC CPIO payload", ErrUnsupported)
	}
	var fields [10]uint64
	offset := 6
	for i, width := range []int{6, 6, 6, 6, 6, 6, 6, 11, 6, 11} {
		v, err := strconv.ParseUint(string(header[offset:offset+width]), 8, 64)
		if err != nil {
			return cpioEntry{}, fmt.Errorf("CPIO header: %w", err)
		}
		fields[i] = v
		offset += width
	}
	nameSize, size := fields[8], fields[9]
	if nameSize < 2 || nameSize > 4096 || size > maxEntrySize {
		return cpioEntry{}, fmt.Errorf("invalid or oversized CPIO entry")
	}
	entry := cpioEntry{mode: fields[2], links: fields[5], size: int64(size)}
	nameData := make([]byte, nameSize)
	if _, err := io.ReadFull(c.r, nameData); err != nil {
		return cpioEntry{}, err
	}
	if nameData[len(nameData)-1] != 0 {
		return cpioEntry{}, fmt.Errorf("CPIO path lacks terminator")
	}
	name := string(nameData[:len(nameData)-1])
	if name == "TRAILER!!!" {
		if entry.size != 0 {
			return cpioEntry{}, fmt.Errorf("CPIO trailer has content")
		}
		if err := readPadding(c.r); err != nil {
			return cpioEntry{}, err
		}
		return cpioEntry{}, io.EOF
	}
	name = strings.TrimPrefix(name, "./")
	// Some packagers, Mozilla's among them, name the payload root "./".
	if name == "" {
		name = "."
	}
	if path.IsAbs(name) || strings.ContainsAny(name, "\\\x00") || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
		return cpioEntry{}, fmt.Errorf("unsafe CPIO path %q", name)
	}
	if _, exists := c.seen[name]; exists {
		return cpioEntry{}, fmt.Errorf("duplicate CPIO path %q", name)
	}
	if name == "." && entry.kind() != cpioDirectory {
		return cpioEntry{}, fmt.Errorf("CPIO root is not a directory")
	}
	c.budget.pathBytes += int64(len(name))
	if c.budget.pathBytes > 128<<20 {
		return cpioEntry{}, fmt.Errorf("PKG payload paths exceed memory limit")
	}
	c.seen[name] = entry.kind()
	entry.name = name
	return entry, nil
}

// skip discards an entry's content.
func (c *cpioReader) skip(entry cpioEntry) error {
	if _, err := io.CopyN(io.Discard, c.r, entry.size); err != nil {
		return fmt.Errorf("CPIO %s: %w", entry.name, err)
	}
	return nil
}

func readCPIO(r io.Reader, budget *payloadBudget, applicationRoot bool) ([]PackageApp, error) {
	var apps []PackageApp
	entries := newCPIOReader(r, budget)
	for {
		entry, err := entries.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		name := entry.name
		if metadata := strings.HasSuffix(name, ".app/Contents/Info.plist") || applicationRoot && name == "Contents/Info.plist"; !metadata {
			if err := entries.skip(entry); err != nil {
				return nil, err
			}
			continue
		}
		if entry.kind() != cpioRegular || entry.links > 1 {
			return nil, fmt.Errorf("%w: application Info.plist must be a regular, unlinked file", ErrUnsupported)
		}
		if entry.size > maxMetadata || entry.size > budget.metadata {
			return nil, fmt.Errorf("application metadata exceeds read limit")
		}
		budget.metadata -= entry.size
		data := make([]byte, entry.size)
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
	}
	for _, app := range apps {
		for parent := path.Join(app.Path, "Contents"); parent != "."; parent = path.Dir(parent) {
			if kind, exists := entries.seen[parent]; exists && kind != cpioDirectory {
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
