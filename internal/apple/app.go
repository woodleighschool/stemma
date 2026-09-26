package apple

import (
	"context"
	"errors"
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
	IconFile   string `json:"icon_file,omitempty" plist:"CFBundleIconFile"`
	IconName   string `json:"icon_name,omitempty" plist:"CFBundleIconName"`
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
	bundle := rootFS(root)
	return InspectAppFS(context.Background(), bundle, ".")
}

// InspectAppFS reads conventional application metadata where the bundle lies
// in a filesystem. Its Info.plist must be a regular file without symlink parents.
func InspectAppFS(ctx context.Context, fsys fs.ReadLinkFS, appPath string) (AppFacts, error) {
	if err := ctx.Err(); err != nil {
		return AppFacts{}, err
	}
	data, err := readRegular(fsys, path.Join(appPath, "Contents/Info.plist"), maxMetadata)
	if err != nil {
		return AppFacts{}, err
	}
	return ParseAppInfo(data)
}

// VerifyApp verifies a Contents-style bundle on disk: its complete Developer ID
// signature, reporting the signer. A zero want derives the signer; otherwise it
// must match.
func VerifyApp(ctx context.Context, appPath string, want signature.Signer) (signature.Result, error) {
	if err := ctx.Err(); err != nil {
		return signature.Result{}, err
	}
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return signature.Result{}, err
	}
	defer func() { _ = root.Close() }()
	bundle := rootFS(root)
	v := &bundleVerifier{ctx: ctx, buffer: make([]byte, 256<<10)}
	return v.verifyApp(bundle, filepath.Base(appPath), want)
}

// VerifyAppFS verifies a Contents-style bundle where it lies in a filesystem,
// such as an open disk image.
func VerifyAppFS(ctx context.Context, fsys fs.ReadLinkFS, appPath string, want signature.Signer) (signature.Result, error) {
	if err := ctx.Err(); err != nil {
		return signature.Result{}, err
	}
	bundle, err := subtree(fsys, appPath)
	if err != nil {
		return signature.Result{}, err
	}
	v := &bundleVerifier{ctx: ctx, buffer: make([]byte, 256<<10)}
	return v.verifyApp(bundle, path.Base(appPath), want)
}

// VerifyAppsFS verifies each application where it lies in a filesystem. They
// must share one signer, and the result names every target.
func VerifyAppsFS(ctx context.Context, fsys fs.ReadLinkFS, apps []string, want signature.Signer) (signature.Result, error) {
	if len(apps) == 0 {
		return signature.Result{}, errors.New("no application to verify")
	}
	var result signature.Result
	var targets []string
	for _, app := range apps {
		observed, err := VerifyAppFS(ctx, fsys, app, want)
		if err != nil {
			return signature.Result{}, fmt.Errorf("%s: %w", app, err)
		}
		if len(targets) == 0 {
			result = observed
			if want, err = signature.Parse(observed.Signer); err != nil {
				return signature.Result{}, err
			}
		}
		targets = append(targets, observed.Target)
	}
	result.Target = strings.Join(targets, ", ")
	return result, nil
}

func (v *bundleVerifier) verifyApp(bundle fs.ReadLinkFS, name string, want signature.Signer) (signature.Result, error) {
	identity, err := v.verifyContents(bundle, name)
	if err != nil {
		return signature.Result{}, err
	}
	if !identity.application {
		return signature.Result{}, fmt.Errorf("%w: application is not signed with a Developer ID Application certificate", ErrUnsupported)
	}
	result := signature.Result{Signer: identity.signer().String(), Name: identity.name, Authority: "Developer ID Application", Target: name, Verifier: signature.Verifier}
	if err := signature.Check(identity.signer(), want); err != nil {
		return result, err
	}
	return result, v.ctx.Err()
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
	// Script applets may omit CFBundleIdentifier. Destinations require one
	// only of the application they detect.
	if facts.Executable == "" {
		return facts, fmt.Errorf("app Info.plist lacks CFBundleExecutable")
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

// bundleFile is a regular file opened from a bundle: read in sequence for
// digests and at offsets for Mach-O code pages.
type bundleFile interface {
	io.ReadCloser
	io.ReaderAt
}

// rootFS exposes a directory on disk as a bundle filesystem. The root confines
// every path to that directory.
func rootFS(root *os.Root) fs.ReadLinkFS {
	return hostLinks{root.FS().(fs.ReadLinkFS)}
}

// hostLinks reports symlink targets with forward slashes, the form signatures
// seal, where the host's filesystem returns its own separator.
type hostLinks struct{ fs.ReadLinkFS }

func (h hostLinks) ReadLink(name string) (string, error) {
	target, err := h.ReadLinkFS.ReadLink(name)
	return filepath.ToSlash(target), err
}

// subtree roots a bundle directory by name. The rooting is only logical, so the
// directory and its parents are established as real here, and confinement
// stays with the filesystem underneath.
func subtree(fsys fs.ReadLinkFS, dir string) (fs.ReadLinkFS, error) {
	if dir == "." {
		return fsys, nil
	}
	if err := realDirectory(fsys, dir); err != nil {
		return nil, err
	}
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return nil, err
	}
	links, ok := sub.(fs.ReadLinkFS)
	if !ok {
		return nil, fmt.Errorf("%w: filesystem subtree does not report symlinks", ErrUnsupported)
	}
	return links, nil
}

// realDirectory requires dir and each of its parents to be a directory itself,
// not a symlink to one.
func realDirectory(fsys fs.ReadLinkFS, dir string) error {
	for prefix := dir; prefix != "."; prefix = path.Dir(prefix) {
		info, err := fsys.Lstat(prefix)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: app path %q traverses a symlink or nondirectory parent", ErrUnsupported, dir)
		}
	}
	return nil
}

// openRegular opens a regular file reached through real directories only. No
// open relies on the filesystem to refuse a symlinked path.
func openRegular(fsys fs.ReadLinkFS, name string) (bundleFile, int64, error) {
	if err := realDirectory(fsys, path.Dir(name)); err != nil {
		return nil, 0, err
	}
	info, err := fsys.Lstat(name)
	if err != nil {
		return nil, 0, err
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, 0, fmt.Errorf("%w: app file %q is a symlink", ErrUnsupported, name)
	}
	if !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s is not a regular file", name)
	}
	f, err := fsys.Open(name)
	if err != nil {
		return nil, 0, err
	}
	file, ok := f.(bundleFile)
	if !ok {
		_ = f.Close()
		return nil, 0, fmt.Errorf("%w: filesystem does not read %s at offsets", ErrUnsupported, name)
	}
	if info, err = f.Stat(); err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", name)
	}
	if err != nil {
		_ = file.Close()
		return nil, 0, err
	}
	return file, info.Size(), nil
}

func readRegular(fsys fs.ReadLinkFS, name string, limit int64) ([]byte, error) {
	f, size, err := openRegular(fsys, name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	if size > limit {
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
