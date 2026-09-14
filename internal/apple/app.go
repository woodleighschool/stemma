package apple

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"time"
	"unicode/utf8"

	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
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

// VerifyApp checks the executable and requested resource scope of a Contents-style
// bundle. Resource sealing rejects symlinks, nested code and unsealed files.
// SubjectSHA256 binds paths, modes, sizes, file hashes and relative symlink targets;
// this verifier tree digest is not a CAS reference or a resource seal.
func VerifyApp(ctx context.Context, appPath string, policy Policy) (Evidence, error) {
	if err := ctx.Err(); err != nil {
		return Evidence{}, err
	}
	policy = policy.expanded()
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return Evidence{}, err
	}
	defer func() { _ = root.Close() }()
	digest, err := appDigest(ctx, root)
	if err != nil {
		evidence := newEvidence("", policy)
		if policy.RequireIntegrity {
			evidence.Integrity = checkError(err, "")
		}
		if policy.RequireResources {
			evidence.Resources = checkError(err, "")
		}
		if policy.RequireSignature {
			evidence.Signature = Check{Status: Unsupported, Detail: "signature verification requires a supported immutable app tree"}
		}
		if policy.RequireIdentity || policy.CertificateSHA256 != "" {
			evidence.Identity = Check{Status: Unsupported, Detail: "identity verification requires a supported immutable app tree"}
		}
		if policy.RequirePlatform {
			evidence.Platform = Check{Status: Unsupported, Detail: "macOS platform assessment requires native OS policy"}
		}
		return evidence, err
	}
	evidence := newEvidence(digest, policy)
	facts, info, err := appInfo(root)
	if err != nil {
		return evidence, err
	}
	resources, resourcesErr := rootRead(root, "Contents/_CodeSignature/CodeResources", 32<<20)
	if resourcesErr != nil && !errors.Is(resourcesErr, fs.ErrNotExist) {
		return evidence, resourcesErr
	}
	external := map[uint32][]byte{1: info}
	if resourcesErr == nil {
		external[3] = resources
	}
	executable := "Contents/MacOS/" + facts.Executable
	f, err := openAppFile(root, executable)
	if err != nil {
		return evidence, err
	}
	defer func() { _ = f.Close() }()
	if err := verifyExecutable(ctx, f, policy, external, &evidence); err != nil {
		return evidence, err
	}
	if policy.RequireResources {
		switch {
		case evidence.Integrity.Status != Valid:
			evidence.Resources = Check{Status: evidence.Integrity.Status, Detail: "resource manifest binding failed: " + evidence.Integrity.Detail}
		case resourcesErr != nil:
			evidence.Resources = checkError(resourcesErr, "")
		default:
			evidence.Resources = checkError(verifyResources(ctx, root, executable, resources), "every regular resource is sealed and matches its recorded digest; no nested code or symlinks")
		}
	}
	if err := ctx.Err(); err != nil {
		return evidence, err
	}
	return evidence, evidence.required(policy)
}

func appInfo(root *os.Root) (AppFacts, []byte, error) {
	data, err := rootRead(root, "Contents/Info.plist", maxMetadata)
	if err != nil {
		return AppFacts{}, nil, err
	}
	facts, err := ParseAppInfo(data)
	return facts, data, err
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
		var err error
		facts.MinimumOS, err = minimumAppOS(requirements.Versions)
		if err != nil {
			return facts, err
		}
	}
	return facts, nil
}

func minimumAppOS(versions map[string]string) (string, error) {
	var maximum []int
	minimum := ""
	for _, architecture := range slices.Sorted(maps.Keys(versions)) {
		version := versions[architecture]
		var numbers []int
		for part := range strings.SplitSeq(version, ".") {
			number, err := strconv.Atoi(part)
			if err != nil || number < 0 || strconv.Itoa(number) != part {
				return "", fmt.Errorf("app Info.plist has invalid minimum system version %q for %s", version, architecture)
			}
			numbers = append(numbers, number)
		}
		for len(numbers) > 1 && numbers[len(numbers)-1] == 0 {
			numbers = numbers[:len(numbers)-1]
		}
		if slices.Compare(numbers, maximum) > 0 {
			maximum, minimum = numbers, version
		}
	}
	return minimum, nil
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

func appDigest(ctx context.Context, root *os.Root) (string, error) {
	type record struct {
		Path   string `json:"path"`
		Mode   uint32 `json:"mode"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256,omitempty"`
		Target string `json:"target,omitempty"`
	}
	var records []record
	buffer := make([]byte, 32<<10)
	lastProgress := time.Now()
	// Traverse from open parent directories rather than reopening the whole
	// bundle-relative path for every file. Record order remains lexical DFS.
	var walk func(*os.Root, string) error
	walk = func(directory *os.Root, prefix string) error {
		children, err := fs.ReadDir(directory.FS(), ".")
		if err != nil {
			return err
		}
		for _, child := range children {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(records) >= 100000 {
				return fmt.Errorf("app contains too many entries")
			}
			name := path.Join(prefix, child.Name())
			info, err := child.Info()
			if err != nil {
				return err
			}
			r := record{Path: name, Mode: uint32(info.Mode().Perm())}
			switch {
			case info.Mode()&fs.ModeSymlink != 0:
				target, err := directory.Readlink(child.Name())
				if err != nil {
					return err
				}
				if target == "" || len(target) > 4096 || !utf8.ValidString(target) || path.IsAbs(target) || strings.ContainsAny(target, "\\:\x00\r\n") || !fs.ValidPath(path.Join(path.Dir(name), target)) {
					return fmt.Errorf("unsafe app symlink %q", name)
				}
				component := false
				for part := range strings.SplitSeq(target, "/") {
					// A preceding named component may itself be a symlink, so lexical
					// cleanup cannot prove a later parent traversal remains confined.
					if part == ".." && component {
						return fmt.Errorf("unsafe app symlink %q", name)
					}
					component = component || part != "" && part != "." && part != ".."
				}
				r.Mode |= uint32(fs.ModeSymlink)
				r.Size, r.Target = int64(len(target)), target
			case info.IsDir():
				r.Mode |= uint32(fs.ModeDir)
			case info.Mode().IsRegular():
				if info.Size() > maxEntrySize {
					return fmt.Errorf("app file %s exceeds limit", name)
				}
				f, err := directory.Open(child.Name())
				if err != nil {
					return err
				}
				r.SHA256, err = fileDigest(ctx, f, buffer)
				_ = f.Close()
				if err != nil {
					return err
				}
				r.Size = info.Size()
			default:
				return fmt.Errorf("%w: nonregular app entry %q", ErrUnsupported, name)
			}
			records = append(records, r)
			if time.Since(lastProgress) >= 250*time.Millisecond {
				plugin.Logger(ctx).InfoContext(ctx, "Hashing application files", "progress", true, "current", len(records), "unit", "entries")
				lastProgress = time.Now()
			}
			if info.IsDir() {
				nested, err := directory.OpenRoot(child.Name())
				if err != nil {
					return err
				}
				err = walk(nested, name)
				_ = nested.Close()
				if err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(root, "."); err != nil {
		return "", err
	}
	data, err := json.Marshal(records)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func verifyResources(ctx context.Context, root *os.Root, executable string, data []byte) error {
	var manifest struct {
		Files map[string]any `plist:"files2"`
	}
	if _, err := plist.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("CodeResources: %w", err)
	}
	if manifest.Files == nil {
		return fmt.Errorf("%w: CodeResources requires version-2 files2 seals", ErrUnsupported)
	}
	if len(manifest.Files) > 100000 {
		return fmt.Errorf("too many resource seals")
	}
	sealed := make(map[string]bool, len(manifest.Files))
	buffer := make([]byte, 32<<10)
	for name, value := range manifest.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00:") {
			return fmt.Errorf("unsafe resource seal path %q", name)
		}
		if filepath.Ext(name) == ".app" || filepath.Ext(name) == ".framework" || filepath.Ext(name) == ".xpc" {
			return fmt.Errorf("%w: nested code resource %q", ErrUnsupported, name)
		}
		seals, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%w: resource seal representation for %q", ErrUnsupported, name)
		}
		if _, nested := seals["cdhash"]; nested {
			return fmt.Errorf("%w: nested code resource %q", ErrUnsupported, name)
		}
		if _, symlink := seals["symlink"]; symlink {
			return fmt.Errorf("%w: symlink resource %q", ErrUnsupported, name)
		}
		for key := range seals {
			if key != "hash" && key != "hash2" && key != "optional" {
				return fmt.Errorf("%w: resource seal field %q", ErrUnsupported, key)
			}
		}
		fullPath := "Contents/" + name
		if fullPath == executable || fullPath == "Contents/Info.plist" || strings.HasPrefix(name, "_CodeSignature/") {
			return fmt.Errorf("unsupported resource seal targets special file %q", name)
		}
		info, err := root.Lstat(fullPath)
		if err != nil {
			return fmt.Errorf("sealed resource %q: %w", name, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: nonregular resource %q", ErrUnsupported, name)
		}
		f, err := root.Open(fullPath)
		if err != nil {
			return err
		}
		sha1Hash, sha256Hash := sha1.New(), sha256.New()
		_, err = io.CopyBuffer(io.MultiWriter(sha1Hash, sha256Hash), fileio.Reader{Context: ctx, Reader: f}, buffer)
		_ = f.Close()
		if err != nil {
			return err
		}
		checked := false
		for key, actual := range map[string][]byte{"hash": sha1Hash.Sum(nil), "hash2": sha256Hash.Sum(nil)} {
			if value, present := seals[key]; present {
				expected, ok := value.([]byte)
				if !ok || !bytes.Equal(expected, actual) {
					return fmt.Errorf("resource %q %s mismatch", name, key)
				}
				checked = true
			}
		}
		if !checked {
			return fmt.Errorf("%w: resource %q has no supported digest", ErrUnsupported, name)
		}
		sealed[fullPath] = true
	}
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if canceled := ctx.Err(); canceled != nil {
			return canceled
		}
		if err != nil {
			return err
		}
		if name == "." || entry.IsDir() {
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: app symlink %q", ErrUnsupported, name)
		}
		if name == executable || name == "Contents/Info.plist" || name == "Contents/_CodeSignature/CodeResources" {
			return nil
		}
		if !strings.HasPrefix(name, "Contents/") || path.Clean(name) != name || !sealed[name] {
			return fmt.Errorf("unsealed app resource %q", name)
		}
		return nil
	})
}
