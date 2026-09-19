package apple

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
)

const maxBundleDepth = 8
const maxBundleEntries = 200000
const maxCodeResources = 64 << 20

// bundleVerifier checks a bundle against its files2 resource envelope: every
// sealed file matches its recorded SHA-256, every sealed symlink its target,
// every nested code item its exact cdhash, and nothing else is present.
type bundleVerifier struct {
	ctx      context.Context
	buffer   []byte
	entries  int
	depth    int
	progress time.Time
}

type resourceSeal struct {
	kind     string
	digest   []byte
	target   string
	cdhash   []byte
	optional bool
	seen     bool
}

// verifyContents verifies a Contents-style bundle and returns its main code
// identity. Without CFBundleExecutable the executable is named after the bundle.
func (v *bundleVerifier) verifyContents(bundle fs.ReadLinkFS, bundleName string) (codeIdentity, error) {
	entries, err := fs.ReadDir(bundle, ".")
	if err != nil {
		return codeIdentity{}, err
	}
	if len(entries) != 1 || entries[0].Name() != "Contents" || !entries[0].IsDir() {
		return codeIdentity{}, errors.New("bundle root must contain only Contents")
	}
	contents, err := subtree(bundle, "Contents")
	if err != nil {
		return codeIdentity{}, err
	}
	info, err := readRegular(contents, "Info.plist", maxMetadata)
	if err != nil {
		return codeIdentity{}, err
	}
	executable, err := bundleExecutable(info, bundleName)
	if err != nil {
		return codeIdentity{}, err
	}
	return v.verifyCode(contents, "MacOS/"+executable, "Info.plist", info)
}

func bundleExecutable(info []byte, bundleName string) (string, error) {
	var facts AppFacts
	if _, err := plist.Unmarshal(info, &facts); err != nil {
		return "", fmt.Errorf("bundle Info.plist: %w", err)
	}
	executable := facts.Executable
	if executable == "" {
		executable = strings.TrimSuffix(bundleName, path.Ext(bundleName))
	}
	if executable == "" || executable == "." || executable == ".." || strings.ContainsAny(executable, "/\\\x00:") {
		return "", fmt.Errorf("unsafe CFBundleExecutable %q", executable)
	}
	return executable, nil
}

// verifyFramework verifies a versioned framework. Only the current version is
// code; the root may hold Versions and the conventional symlinks into it.
func (v *bundleVerifier) verifyFramework(bundle fs.ReadLinkFS, bundleName string) (codeIdentity, error) {
	entries, err := fs.ReadDir(bundle, ".")
	if err != nil {
		return codeIdentity{}, err
	}
	for _, entry := range entries {
		switch {
		case entry.Name() == "Versions" && entry.IsDir():
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := bundle.ReadLink(entry.Name())
			if err != nil {
				return codeIdentity{}, err
			}
			if target != "Versions/Current/"+entry.Name() {
				return codeIdentity{}, fmt.Errorf("%w: framework symlink %q points to %q", ErrUnsupported, entry.Name(), target)
			}
		default:
			return codeIdentity{}, fmt.Errorf("%w: framework root entry %q", ErrUnsupported, entry.Name())
		}
	}
	versions, err := fs.ReadDir(bundle, "Versions")
	if err != nil {
		return codeIdentity{}, err
	}
	current, err := bundle.ReadLink("Versions/Current")
	if err != nil {
		return codeIdentity{}, fmt.Errorf("%w: framework without a current version: %w", ErrUnsupported, err)
	}
	for _, entry := range versions {
		if entry.Name() != "Current" && (entry.Name() != current || !entry.IsDir()) {
			return codeIdentity{}, fmt.Errorf("%w: framework version entry %q is not the current version", ErrUnsupported, entry.Name())
		}
	}
	version, err := subtree(bundle, "Versions/"+current)
	if err != nil {
		return codeIdentity{}, err
	}
	infoPath := "Resources/Info.plist"
	info, err := readRegular(version, infoPath, maxMetadata)
	if errors.Is(err, fs.ErrNotExist) {
		infoPath = "Info.plist"
		info, err = readRegular(version, infoPath, maxMetadata)
	}
	if err != nil {
		return codeIdentity{}, err
	}
	executable, err := bundleExecutable(info, bundleName)
	if err != nil {
		return codeIdentity{}, err
	}
	return v.verifyCode(version, executable, infoPath, info)
}

// verifyShallow verifies a bundle whose signature, executable and resources
// share its root, such as an unversioned framework.
func (v *bundleVerifier) verifyShallow(bundle fs.ReadLinkFS, bundleName string) (codeIdentity, error) {
	infoPath := "Resources/Info.plist"
	info, err := readRegular(bundle, infoPath, maxMetadata)
	if errors.Is(err, fs.ErrNotExist) {
		infoPath = "Info.plist"
		info, err = readRegular(bundle, infoPath, maxMetadata)
	}
	if err != nil {
		return codeIdentity{}, err
	}
	executable, err := bundleExecutable(info, bundleName)
	if err != nil {
		return codeIdentity{}, err
	}
	return v.verifyCode(bundle, executable, infoPath, info)
}

// verifyCode authenticates the main executable within a resource root, whose
// CodeDirectory seals Info.plist and CodeResources, then the envelope itself.
func (v *bundleVerifier) verifyCode(root fs.ReadLinkFS, executable, infoPath string, info []byte) (codeIdentity, error) {
	if err := realDirectory(root, "_CodeSignature"); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return codeIdentity{}, err
	}
	signatureEntries, err := fs.ReadDir(root, "_CodeSignature")
	if err != nil {
		return codeIdentity{}, fmt.Errorf("bundle is not signed: %w", err)
	}
	for _, entry := range signatureEntries {
		if entry.Name() != "CodeResources" || !entry.Type().IsRegular() {
			return codeIdentity{}, fmt.Errorf("%w: legacy signature file %q", ErrUnsupported, entry.Name())
		}
	}
	resources, err := readRegular(root, "_CodeSignature/CodeResources", maxCodeResources)
	if err != nil {
		return codeIdentity{}, err
	}
	f, size, err := openRegular(root, executable)
	if err != nil {
		return codeIdentity{}, err
	}
	identity, err := verifyMachO(v.ctx, f, size, map[uint32][]byte{1: info, 3: resources})
	_ = f.Close()
	if err != nil {
		return codeIdentity{}, fmt.Errorf("%s: %w", executable, err)
	}
	if err := v.verifyResources(root, resources, executable, infoPath); err != nil {
		return codeIdentity{}, err
	}
	return identity, nil
}

func (v *bundleVerifier) verifyResources(root fs.ReadLinkFS, data []byte, executable, infoPath string) error {
	var manifest struct {
		Files map[string]any `plist:"files2"`
	}
	if _, err := plist.Unmarshal(data, &manifest); err != nil {
		return fmt.Errorf("CodeResources: %w", err)
	}
	if manifest.Files == nil {
		return fmt.Errorf("%w: CodeResources requires version-2 files2 seals", ErrUnsupported)
	}
	if len(manifest.Files) > maxBundleEntries {
		return errors.New("too many resource seals")
	}
	seals := make(map[string]*resourceSeal, len(manifest.Files))
	for name, value := range manifest.Files {
		if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
			return fmt.Errorf("unsafe resource seal path %q", name)
		}
		seal, err := parseSeal(value)
		if err != nil {
			return fmt.Errorf("resource %q: %w", name, err)
		}
		seals[name] = seal
	}
	err := fs.WalkDir(root, ".", func(name string, entry fs.DirEntry, err error) error {
		if canceled := v.ctx.Err(); canceled != nil {
			return canceled
		}
		if err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if v.entries++; v.entries > maxBundleEntries {
			return errors.New("bundle contains too many entries")
		}
		v.report()
		seal := seals[name]
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := root.ReadLink(name)
			if err != nil {
				return err
			}
			if seal == nil {
				if name == "CodeResources" && target == "_CodeSignature/CodeResources" {
					return nil
				}
				return fmt.Errorf("unsealed symlink %q", name)
			}
			seal.seen = true
			if seal.kind != "symlink" {
				return fmt.Errorf("resource %q is a symlink but sealed as a %s", name, seal.kind)
			}
			if target != seal.target {
				return fmt.Errorf("symlink %q points to %q, sealed as %q", name, target, seal.target)
			}
		case entry.IsDir():
			if seal == nil {
				return nil
			}
			seal.seen = true
			if seal.kind != "nested" {
				return fmt.Errorf("resource %q is a directory but sealed as a %s", name, seal.kind)
			}
			if err := v.verifyNested(root, name, seal.cdhash); err != nil {
				return err
			}
			return fs.SkipDir
		case entry.Type().IsRegular():
			if seal == nil {
				if name == executable || name == infoPath || name == "_CodeSignature/CodeResources" || unsealedByDefault(name) {
					return nil
				}
				return fmt.Errorf("unsealed resource %q", name)
			}
			seal.seen = true
			switch seal.kind {
			case "file":
				return v.verifySealedFile(root, name, seal.digest)
			case "nested":
				return v.verifyNested(root, name, seal.cdhash)
			default:
				return fmt.Errorf("resource %q is a regular file but sealed as a %s", name, seal.kind)
			}
		default:
			return fmt.Errorf("%w: nonregular resource %q", ErrUnsupported, name)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for name, seal := range seals {
		if !seal.seen && !seal.optional {
			return fmt.Errorf("sealed resource %q is missing", name)
		}
	}
	return nil
}

func parseSeal(value any) (*resourceSeal, error) {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: resource seal representation", ErrUnsupported)
	}
	seal := &resourceSeal{}
	for key, field := range fields {
		ok := true
		switch key {
		case "hash":
			// SHA-1 seals are superseded by hash2.
		case "hash2":
			seal.digest, ok = field.([]byte)
		case "symlink":
			seal.target, ok = field.(string)
		case "cdhash":
			seal.cdhash, ok = field.([]byte)
		case "requirement":
			// Nested code is bound by its exact cdhash; requirement language is not evaluated.
			_, ok = field.(string)
		case "optional":
			seal.optional, ok = field.(bool)
		default:
			return nil, fmt.Errorf("%w: resource seal field %q", ErrUnsupported, key)
		}
		if !ok {
			return nil, fmt.Errorf("invalid resource seal field %q", key)
		}
	}
	switch {
	case seal.cdhash != nil:
		if len(seal.cdhash) != 20 || seal.digest != nil || seal.target != "" {
			return nil, fmt.Errorf("%w: nested code seal", ErrUnsupported)
		}
		seal.kind = "nested"
	case seal.target != "":
		if seal.digest != nil {
			return nil, fmt.Errorf("%w: symlink seal", ErrUnsupported)
		}
		seal.kind = "symlink"
	case len(seal.digest) == sha256.Size:
		seal.kind = "file"
	default:
		return nil, fmt.Errorf("%w: resource seal without a SHA-256 digest", ErrUnsupported)
	}
	return seal, nil
}

// unsealedByDefault lists the files codesign leaves out of every envelope:
// PkgInfo, Finder metadata, localization version stamps and its own
// compatibility copy of the envelope at the resource root.
func unsealedByDefault(name string) bool {
	return name == "PkgInfo" || name == "CodeResources" || path.Base(name) == ".DS_Store" || strings.HasSuffix(name, ".lproj/locversion.plist")
}

func (v *bundleVerifier) verifySealedFile(root fs.ReadLinkFS, name string, expected []byte) error {
	f, _, err := openRegular(root, name)
	if err != nil {
		return err
	}
	digest, err := fileDigest(v.ctx, f, v.buffer)
	_ = f.Close()
	if err != nil {
		return err
	}
	if digest != hex.EncodeToString(expected) {
		return fmt.Errorf("resource %q does not match its seal", name)
	}
	return nil
}

// verifyNested authenticates nested code and binds it by its sealed cdhash,
// so only the exact recorded code satisfies the envelope.
func (v *bundleVerifier) verifyNested(root fs.ReadLinkFS, name string, cdhash []byte) error {
	if v.depth >= maxBundleDepth {
		return fmt.Errorf("%w: nested code deeper than %d bundles", ErrUnsupported, maxBundleDepth)
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	identity, err := v.verifyNestedCode(root, name, info.IsDir())
	if err != nil {
		return fmt.Errorf("nested code %q: %w", name, err)
	}
	if !matchCDHash(identity, cdhash) {
		return fmt.Errorf("nested code %q does not match its sealed cdhash", name)
	}
	return nil
}

func (v *bundleVerifier) verifyNestedCode(root fs.ReadLinkFS, name string, bundle bool) (codeIdentity, error) {
	if !bundle {
		f, size, err := openRegular(root, name)
		if err != nil {
			return codeIdentity{}, err
		}
		defer func() { _ = f.Close() }()
		return verifyMachO(v.ctx, f, size, nil)
	}
	nested, err := subtree(root, name)
	if err != nil {
		return codeIdentity{}, err
	}
	v.depth++
	defer func() { v.depth-- }()
	switch {
	case isDirectory(nested, "Contents"):
		return v.verifyContents(nested, path.Base(name))
	case isDirectory(nested, "Versions"):
		return v.verifyFramework(nested, path.Base(name))
	case isDirectory(nested, "_CodeSignature"):
		return v.verifyShallow(nested, path.Base(name))
	default:
		return codeIdentity{}, fmt.Errorf("%w: nested code layout", ErrUnsupported)
	}
}

func isDirectory(fsys fs.ReadLinkFS, name string) bool {
	info, err := fsys.Lstat(name)
	return err == nil && info.IsDir()
}

func (v *bundleVerifier) report() {
	if time.Since(v.progress) < 250*time.Millisecond {
		return
	}
	v.progress = time.Now()
	plugin.Logger(v.ctx).InfoContext(v.ctx, "Verifying sealed resources", "progress", true, "current", v.entries, "unit", "entries")
}
