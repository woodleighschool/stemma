package apple

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"howett.net/plist"
)

// AppFacts contains conventional Info.plist metadata, which is inspection only.
type AppFacts struct {
	BundleID   string `json:"bundle_id" plist:"CFBundleIdentifier"`
	Name       string `json:"name" plist:"CFBundleName"`
	Version    string `json:"version" plist:"CFBundleShortVersionString"`
	Build      string `json:"build" plist:"CFBundleVersion"`
	Executable string `json:"executable" plist:"CFBundleExecutable"`
	MinimumOS  string `json:"minimum_os" plist:"LSMinimumSystemVersion"`
}

// InspectApp reads XML or binary Info.plist metadata in a Contents-style bundle.
func InspectApp(appPath string) (AppFacts, error) {
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return AppFacts{}, err
	}
	defer func() { _ = root.Close() }()
	facts, _, err := appInfo(root)
	return facts, err
}

// VerifyApp checks a strict Contents-style regular-file bundle subset. It rejects
// symlinks, nested code and unsealed files, and never authenticates an ad-hoc seal.
// SubjectSHA256 hashes sorted JSON records of path, mode, size and SHA-256 for all
// files and directories; this is an apple verifier tree digest, not a CAS reference.
func VerifyApp(appPath string, policy Policy) (Evidence, error) {
	policy = policy.expanded()
	root, err := os.OpenRoot(appPath)
	if err != nil {
		return Evidence{}, err
	}
	defer func() { _ = root.Close() }()
	digest, err := appDigest(root)
	if err != nil {
		evidence := newEvidence("", policy)
		if policy.RequireIntegrity {
			evidence.Integrity = checkError(err, "")
		}
		if policy.RequireResources {
			evidence.Resources = checkError(err, "")
		}
		if policy.RequireSignature {
			evidence.Signature = Check{Status: Unsupported, Detail: "Mach-O CMS cryptographic signature verification is unsupported"}
		}
		if policy.RequireIdentity || policy.CertificateSHA256 != "" {
			evidence.Identity = Check{Status: Unsupported, Detail: "Mach-O identity verification is unsupported"}
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
	resources, resourcesErr := rootRead(root, "Contents/_CodeSignature/CodeResources", maxMetadata)
	external := map[uint32][]byte{1: info}
	if resourcesErr == nil {
		external[3] = resources
	}
	executable := "Contents/MacOS/" + facts.Executable
	f, err := root.Open(executable)
	if err != nil {
		return evidence, err
	}
	defer func() { _ = f.Close() }()
	if err := verifyExecutable(f, policy, external, &evidence); err != nil {
		return evidence, err
	}
	if policy.RequireResources {
		switch {
		case evidence.Integrity.Status != Valid:
			evidence.Resources = Check{Status: evidence.Integrity.Status, Detail: "resource manifest binding failed: " + evidence.Integrity.Detail}
		case resourcesErr != nil:
			evidence.Resources = checkError(resourcesErr, "")
		default:
			evidence.Resources = checkError(verifyResources(root, executable, resources), "every regular resource is sealed and matches its recorded digest; no nested code or symlinks")
		}
	}
	return evidence, evidence.required(policy)
}

func appInfo(root *os.Root) (AppFacts, []byte, error) {
	data, err := rootRead(root, "Contents/Info.plist", maxMetadata)
	if err != nil {
		return AppFacts{}, nil, err
	}
	var facts AppFacts
	if _, err := plist.Unmarshal(data, &facts); err != nil {
		return facts, nil, fmt.Errorf("app Info.plist: %w", err)
	}
	if facts.BundleID == "" || facts.Executable == "" {
		return facts, nil, fmt.Errorf("app Info.plist lacks CFBundleIdentifier or CFBundleExecutable")
	}
	if facts.Executable == "." || facts.Executable == ".." || strings.ContainsAny(facts.Executable, "/\\\x00:") {
		return facts, nil, fmt.Errorf("unsafe CFBundleExecutable %q", facts.Executable)
	}
	return facts, data, nil
}

func rootRead(root *os.Root, name string, limit int64) ([]byte, error) {
	f, err := root.Open(name)
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

func appDigest(root *os.Root) (string, error) {
	type record struct {
		Path   string `json:"path"`
		Mode   uint32 `json:"mode"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256,omitempty"`
	}
	var records []record
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if len(records) >= 100000 {
			return fmt.Errorf("app contains too many entries")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%w: app symlink %q", ErrUnsupported, name)
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			return fmt.Errorf("%w: nonregular app entry %q", ErrUnsupported, name)
		}
		r := record{Path: name, Mode: uint32(info.Mode().Perm())}
		if info.IsDir() {
			r.Mode |= uint32(fs.ModeDir)
		} else {
			if info.Size() > maxEntrySize {
				return fmt.Errorf("app file %s exceeds limit", name)
			}
			f, err := root.Open(name)
			if err != nil {
				return err
			}
			r.SHA256, err = fileDigest(f)
			_ = f.Close()
			if err != nil {
				return err
			}
			r.Size = info.Size()
		}
		records = append(records, r)
		return nil
	})
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(records)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func verifyResources(root *os.Root, executable string, data []byte) error {
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
	for name, value := range manifest.Files {
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
		_, err = io.Copy(io.MultiWriter(sha1Hash, sha256Hash), f)
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
