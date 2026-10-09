package apple

import (
	"cmp"
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

	"github.com/deploymenttheory/go-macos-pkg/pkg/pkgsign"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/treefs"
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
func InspectApp(ctx context.Context, appPath string) (AppFacts, error) {
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return AppFacts{}, err
	}
	defer func() { _ = root.Close() }()
	return InspectAppFS(ctx, rootFS(root), ".")
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

// VerifyApp verifies the complete signature of a Contents-style bundle on disk
// and reports the publisher it was signed for, under Developer ID or the App
// Store. A zero want derives the signer; otherwise it must match.
func VerifyApp(ctx context.Context, appPath string, want signature.Signer) (signature.Result, error) {
	if err := ctx.Err(); err != nil {
		return signature.Result{}, err
	}
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return signature.Result{}, err
	}
	defer func() { _ = root.Close() }()
	bundle, err := treefs.OpenDir(rootFS(root), ".")
	if err != nil {
		return signature.Result{}, err
	}
	v := &bundleVerifier{ctx: ctx, roots: pkgsign.AppleRootCertificates()}
	result, err := v.verifyApp(bundle, filepath.Base(appPath), want)
	closeRead(bundle, &err)
	return result, err
}

// VerifyAppFS verifies a Contents-style bundle where it lies in a filesystem,
// such as an open disk image.
func VerifyAppFS(ctx context.Context, fsys fs.ReadLinkFS, appPath string, want signature.Signer) (result signature.Result, err error) {
	if err := ctx.Err(); err != nil {
		return signature.Result{}, err
	}
	bundle, err := subtree(fsys, appPath)
	if err != nil {
		return signature.Result{}, err
	}
	defer closeRead(bundle, &err)
	v := &bundleVerifier{ctx: ctx, roots: pkgsign.AppleRootCertificates(), base: appPath}
	v.attributes, _ = fsys.(xattrFS)
	return v.verifyApp(bundle, path.Base(appPath), want)
}

func (v *bundleVerifier) verifyApp(bundle fs.ReadLinkFS, name string, want signature.Signer) (signature.Result, error) {
	identity, err := v.verifyContents(bundle, "", name)
	if err != nil {
		return signature.Result{}, err
	}
	signer, authority := identity.publisher()
	if signer.IsZero() {
		return signature.Result{}, fmt.Errorf("%w: application is signed with neither a Developer ID Application nor an App Store certificate", ErrUnsupported)
	}
	slices.SortFunc(v.replaced, func(a, b signature.Replacement) int { return cmp.Compare(a.Path, b.Path) })
	result := signature.Result{Signer: signer.String(), Name: identity.name, Authority: authority, Target: name, Verifier: signature.Verifier, Timestamped: identity.timestamped, Replaced: v.replaced}
	if err := signature.Check(signer, want); err != nil {
		return result, err
	}
	return result, v.ctx.Err()
}

// ParseAppInfo reads conventional application metadata without accessing an
// installed application or evaluating installer scripts. An absent executable
// declaration stays absent; resolving it requires the bundle filename.
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
	if facts.Executable != "" {
		if err := validateExecutable(facts.Executable); err != nil {
			return facts, err
		}
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

func rootFS(root *os.Root) fs.ReadLinkFS { return treefs.Local(root) }

func subtree(fsys fs.ReadLinkFS, dir string) (*treefs.Directory, error) {
	if err := realDirectory(fsys, dir); err != nil {
		return nil, err
	}
	return treefs.OpenDir(fsys, dir)
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
func openRegular(fsys fs.ReadLinkFS, name string) (*treefs.File, int64, error) {
	f, err := treefs.OpenFile(fsys, name)
	if err != nil {
		return nil, 0, err
	}
	if _, ok := f.File.(io.ReaderAt); !ok {
		return nil, 0, errors.Join(fmt.Errorf("%w: filesystem does not read %s at offsets", ErrUnsupported, name), f.Close())
	}
	info, err := f.Stat()
	if err != nil {
		return nil, 0, errors.Join(err, f.Close())
	}
	return f, info.Size(), nil
}

func readRegular(fsys fs.ReadLinkFS, name string, limit int64) (data []byte, err error) {
	f, size, err := openRegular(fsys, name)
	if err != nil {
		return nil, err
	}
	defer closeRead(f, &err)
	if size > limit {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", name, limit)
	}
	data, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s exceeds %d-byte limit", name, limit)
	}
	return data, nil
}

// A failed final identity check invalidates even an unsigned verdict. Joining
// ErrUnsigned with the failure would let callers mistake damage for absence.
func closeRead(c io.Closer, err *error) {
	if closeErr := c.Close(); closeErr != nil {
		*err = closeErr
	}
}
