package apple

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/signature"
	"howett.net/plist"
)

// AppFacts contains conventional Info.plist metadata, which is inspection only.
type AppFacts struct {
	BundleID   string `json:"bundle_id" plist:"CFBundleIdentifier"`
	Name       string `json:"name" plist:"CFBundleName"`
	Version    string `json:"version" plist:"CFBundleShortVersionString"`
	Build      string `json:"build" plist:"CFBundleVersion"`
	Executable string `json:"executable" plist:"CFBundleExecutable"`
	// MinimumOS is the scalar requirement, or the highest declared architecture
	// requirement when LSMinimumSystemVersion is absent.
	MinimumOS string `json:"minimum_os" plist:"LSMinimumSystemVersion"`
}

// InspectApp reads XML or binary Info.plist metadata in a Contents-style bundle.
func InspectApp(appPath string) (AppFacts, error) {
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return AppFacts{}, err
	}
	defer func() { _ = root.Close() }()
	return InspectAppFS(context.Background(), root.FS(), ".")
}

// InspectAppFS reads conventional application metadata from a filesystem.
// Metadata paths must be regular files without symlink parents.
func InspectAppFS(ctx context.Context, fsys fs.FS, appPath string) (AppFacts, error) {
	name := path.Join(appPath, "Contents/Info.plist")
	for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
		info, err := fs.Lstat(fsys, prefix)
		if err != nil {
			return AppFacts{}, err
		}
		if info.Mode()&fs.ModeSymlink != 0 || prefix != name && !info.IsDir() {
			return AppFacts{}, fmt.Errorf("%w: app metadata traverses a symlink or nondirectory parent", ErrUnsupported)
		}
		if prefix == name && (!info.Mode().IsRegular() || info.Size() > maxMetadata) {
			return AppFacts{}, fmt.Errorf("app Info.plist must be a regular file within the metadata limit")
		}
	}
	f, err := fsys.Open(name)
	if err != nil {
		return AppFacts{}, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(fileio.Reader{Context: ctx, Reader: f}, maxMetadata+1))
	if err != nil {
		return AppFacts{}, err
	}
	return ParseAppInfo(data)
}

// VerifyApp verifies a Contents-style bundle's complete Developer ID signature
// and reports its signer. A zero want derives the signer; otherwise it must match.
func VerifyApp(ctx context.Context, appPath string, want signature.Signer) (signature.Result, error) {
	if err := ctx.Err(); err != nil {
		return signature.Result{}, err
	}
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return signature.Result{}, err
	}
	defer func() { _ = root.Close() }()
	v := &bundleVerifier{ctx: ctx, buffer: make([]byte, 256<<10)}
	if nativeBundleValidity != nil {
		v.skipEnvelopes = nativeBundleValidity(ctx, appPath)
	}
	identity, err := v.verifyContents(root, filepath.Base(appPath))
	if err != nil {
		return signature.Result{}, err
	}
	if !identity.application {
		return signature.Result{}, fmt.Errorf("%w: application is not signed with a Developer ID Application certificate", ErrUnsupported)
	}
	result := signature.Result{Signer: identity.signer().String(), Name: identity.name, Authority: "Developer ID Application", Target: filepath.Base(appPath), Verifier: signature.Verifier}
	if err := signature.Check(identity.signer(), want); err != nil {
		return result, err
	}
	return result, ctx.Err()
}

// ParseAppInfo reads conventional application metadata without accessing an
// installed application or evaluating installer scripts.
func ParseAppInfo(data []byte) (AppFacts, error) {
	if len(data) > maxMetadata {
		return AppFacts{}, fmt.Errorf("app Info.plist exceeds read limit")
	}
	var facts AppFacts
	if _, err := plist.Unmarshal(data, &facts); err != nil {
		return facts, fmt.Errorf("app Info.plist: %w", err)
	}
	if facts.BundleID == "" || facts.Executable == "" {
		return facts, fmt.Errorf("app Info.plist lacks CFBundleIdentifier or CFBundleExecutable")
	}
	if facts.Executable == "." || facts.Executable == ".." || strings.ContainsAny(facts.Executable, "/\\\x00:") {
		return facts, fmt.Errorf("unsafe CFBundleExecutable %q", facts.Executable)
	}
	if facts.MinimumOS == "" {
		var requirements struct {
			Versions map[string]string `plist:"LSMinimumSystemVersionByArchitecture"`
		}
		if _, err := plist.Unmarshal(data, &requirements); err != nil {
			return facts, fmt.Errorf("app Info.plist minimum system versions: %w", err)
		}
		var versions []string
		for _, architecture := range slices.Sorted(maps.Keys(requirements.Versions)) {
			versions = append(versions, requirements.Versions[architecture])
		}
		var err error
		if facts.MinimumOS, err = highestOSVersion(versions...); err != nil {
			return facts, fmt.Errorf("app Info.plist: %w", err)
		}
	}
	return facts, nil
}

// highestOSVersion returns the latest dotted numeric macOS version, ignoring
// absent values. Trailing zero components do not affect ordering.
func highestOSVersion(versions ...string) (string, error) {
	var maximum []int
	highest := ""
	for _, version := range versions {
		if version == "" {
			continue
		}
		var numbers []int
		for part := range strings.SplitSeq(version, ".") {
			number, err := strconv.Atoi(part)
			if err != nil || number < 0 || strconv.Itoa(number) != part {
				return "", fmt.Errorf("invalid minimum system version %q", version)
			}
			numbers = append(numbers, number)
		}
		for len(numbers) > 1 && numbers[len(numbers)-1] == 0 {
			numbers = numbers[:len(numbers)-1]
		}
		if slices.Compare(numbers, maximum) > 0 {
			maximum, highest = numbers, version
		}
	}
	return highest, nil
}

func rootRead(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := openAppFile(root, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", name, limit)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", name, limit)
	}
	return data, nil
}

func openAppFile(root *os.Root, name string) (*os.File, error) {
	for prefix := name; prefix != "."; prefix = path.Dir(prefix) {
		info, err := root.Lstat(prefix)
		if err != nil {
			return nil, err
		}
		if info.Mode()&fs.ModeSymlink != 0 || prefix != name && !info.IsDir() {
			return nil, fmt.Errorf("%w: app file %q traverses a symlink or nondirectory parent", ErrUnsupported, name)
		}
		if prefix == name && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file", name)
		}
	}
	return root.Open(name)
}
