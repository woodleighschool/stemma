package apple

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/internal/treefs"
	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
)

const maxBundleDepth = 8
const maxBundleEntries = 200000
const maxCodeResources = 64 << 20

// bundleVerifier checks a bundle against its files2 resource envelope: every
// sealed file matches its recorded SHA-256, every sealed symlink its target,
// every nested code item its exact cdhash or recorded requirement, and nothing
// else is present. Locations are paths relative to the verified bundle.
type bundleVerifier struct {
	ctx     context.Context
	buffers sync.Pool
	mu      sync.Mutex
	// roots anchors every signing certificate and timestamp in the bundle.
	roots    []*x509.Certificate
	entries  int
	depth    int
	progress time.Time
	replaced []signature.Replacement
	// attributes reads extended attributes from the filesystem that holds the
	// verified bundle at base, when that filesystem has them.
	attributes xattrFS
	base       string
}

type resourceSeal struct {
	kind        string
	digest      []byte
	target      string
	cdhash      []byte
	requirement string
	optional    bool
	seen        bool
}

// verifyContents returns the verified identity of a Contents-style bundle.
func (v *bundleVerifier) verifyContents(bundle fs.ReadLinkFS, location, bundleName string) (identity codeIdentity, err error) {
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
	defer closeRead(contents, &err)
	info, err := readRegular(contents, "Info.plist", maxMetadata)
	if err != nil {
		return codeIdentity{}, err
	}
	return v.verifyCode(contents, path.Join(location, "Contents"), bundleName, "MacOS", "Info.plist", info)
}

// ResolveExecutable returns the declared executable name, or the bundle filename
// without its extension when the declaration is absent. It validates the name
// but does not check whether the executable exists.
func ResolveExecutable(declared, bundleName string) (string, error) {
	executable := declared
	if executable == "" {
		executable = strings.TrimSuffix(bundleName, path.Ext(bundleName))
	}
	if err := validateExecutable(executable); err != nil {
		return "", err
	}
	return executable, nil
}

func validateExecutable(executable string) error {
	if executable == "" || executable == "." || executable == ".." || strings.ContainsAny(executable, "/\\\x00:") {
		return fmt.Errorf("unsafe CFBundleExecutable %q", executable)
	}
	return nil
}

// verifyFramework verifies every version against the enclosing resource seal.
// Root links are aliases, not sealed resources; each must name the same entry
// in the version selected by Current, directly or through Current itself.
// Direct links preserve the literal Current target, including an optional "./".
func (v *bundleVerifier) verifyFramework(bundle fs.ReadLinkFS, location, bundleName string, seal *resourceSeal) (codeIdentity, error) {
	current, err := bundle.ReadLink("Versions/Current")
	if err != nil {
		return codeIdentity{}, fmt.Errorf("%w: framework without a current version: %w", ErrUnsupported, err)
	}
	versionName := strings.TrimPrefix(current, "./")
	if !fs.ValidPath(versionName) || versionName == "." || versionName == "Current" || strings.ContainsAny(versionName, "/\\\x00") {
		return codeIdentity{}, fmt.Errorf("%w: unsafe framework current version %q", ErrUnsupported, current)
	}
	if err := realDirectory(bundle, "Versions/"+versionName); err != nil {
		return codeIdentity{}, fmt.Errorf("%w: framework current version: %w", ErrUnsupported, err)
	}
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
			target = strings.TrimPrefix(target, "./")
			if target != "Versions/Current/"+entry.Name() && target != "Versions/"+current+"/"+entry.Name() {
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
	var selected codeIdentity
	for _, entry := range versions {
		if entry.Name() == "Current" {
			continue
		}
		if !entry.IsDir() {
			return codeIdentity{}, fmt.Errorf("%w: framework version entry %q is not a directory", ErrUnsupported, entry.Name())
		}
		version, err := subtree(bundle, "Versions/"+entry.Name())
		if err != nil {
			return codeIdentity{}, err
		}
		versionLocation := path.Join(location, "Versions", entry.Name())
		identity, err := v.verifyShallow(version, versionLocation, bundleName)
		err = errors.Join(err, version.Close())
		if err != nil {
			return codeIdentity{}, fmt.Errorf("framework version %q: %w", entry.Name(), err)
		}
		if entry.Name() == versionName {
			selected = identity
		} else if err := verifyNestedIdentity(identity, seal); err != nil {
			return codeIdentity{}, fmt.Errorf("framework version %q: %w", entry.Name(), err)
		}
	}
	return selected, nil
}

// verifyShallow verifies a bundle whose signature, executable and resources
// share its root, such as an unversioned framework.
func (v *bundleVerifier) verifyShallow(bundle fs.ReadLinkFS, location, bundleName string) (codeIdentity, error) {
	infoPath := "Resources/Info.plist"
	info, err := readRegular(bundle, infoPath, maxMetadata)
	if errors.Is(err, fs.ErrNotExist) {
		infoPath = "Info.plist"
		info, err = readRegular(bundle, infoPath, maxMetadata)
	}
	if err != nil {
		return codeIdentity{}, err
	}
	return v.verifyCode(bundle, location, bundleName, "", infoPath, info)
}

// verifyCode authenticates the bundle's main code and its resource envelope.
func (v *bundleVerifier) verifyCode(root fs.ReadLinkFS, location, bundleName, executableDir, infoPath string, info []byte) (identity codeIdentity, err error) {
	var declaration struct {
		AppFacts

		MainHTML         any `plist:"MainHTML"`
		InstallerVersion any `plist:"IFMajorVersion"`
	}
	if _, err := plist.Unmarshal(info, &declaration); err != nil {
		return codeIdentity{}, fmt.Errorf("bundle Info.plist: %w", err)
	}
	if declaration.MainHTML != nil {
		return codeIdentity{}, fmt.Errorf("%w: widget bundle", ErrUnsupported)
	}
	name, err := ResolveExecutable(declaration.Executable, bundleName)
	if err != nil {
		return codeIdentity{}, err
	}
	mainPath := path.Join(executableDir, name)
	f, size, err := openRegular(root, mainPath)
	if errors.Is(err, fs.ErrNotExist) && declaration.Executable == "" {
		if declaration.InstallerVersion != nil {
			return codeIdentity{}, fmt.Errorf("%w: installer bundle", ErrUnsupported)
		}
		// Without a declared or namesake executable, codesign uses Info.plist as code.
		mainPath = infoPath
		f, size, err = openRegular(root, mainPath)
	}
	if err != nil {
		return codeIdentity{}, fmt.Errorf("bundle main file %q: %w", mainPath, err)
	}
	defer closeRead(f, &err)
	metadata, err := subtree(root, "_CodeSignature")
	if errors.Is(err, fs.ErrNotExist) && mainPath != infoPath {
		return codeIdentity{}, v.withoutEnvelope(f, size, path.Join(location, mainPath), info)
	}
	if err != nil {
		return codeIdentity{}, err
	}
	defer closeRead(metadata, &err)
	resources, err := readRegular(metadata, "CodeResources", maxCodeResources)
	if err != nil {
		return codeIdentity{}, err
	}
	external := map[uint32][]byte{1: info, 3: resources}
	identity, err = v.verifyBundleMain(metadata, path.Join(location, mainPath), f, size, external)
	if errors.Is(err, signature.ErrUnsigned) {
		return codeIdentity{}, fmt.Errorf("%s: resource envelope exists but main file is unsigned", mainPath)
	}
	if err != nil {
		return codeIdentity{}, fmt.Errorf("%s: %w", mainPath, err)
	}
	if err := v.verifyResources(root, location, resources, mainPath, infoPath); err != nil {
		if errors.Is(err, signature.ErrUnsigned) {
			return codeIdentity{}, errors.New("resource envelope contains unsigned nested code")
		}
		return codeIdentity{}, err
	}
	return identity, nil
}

// Mach-O code carries its signature in the executable. For generic code,
// including a script or the nominal Info.plist, the bundle holds the components.
func (v *bundleVerifier) verifyBundleMain(metadata fs.ReadLinkFS, location string, code *treefs.File, size int64, external map[uint32][]byte) (codeIdentity, error) {
	entries, err := fs.ReadDir(metadata, ".")
	if err != nil {
		return codeIdentity{}, err
	}
	machO, err := isMachO(code, size)
	if err != nil {
		return codeIdentity{}, err
	}
	if machO {
		if len(entries) != 1 || entries[0].Name() != "CodeResources" {
			return codeIdentity{}, fmt.Errorf("%w: unexpected signature files alongside a Mach-O signature", ErrUnsupported)
		}
		return verifyMachO(v.ctx, code, size, external, v.roots)
	}
	signed, err := v.hasSignatureAttributes(location, code)
	if err != nil {
		return codeIdentity{}, err
	}
	if signed {
		return codeIdentity{}, errors.New("bundle main file has signature attributes as well as signature files")
	}
	blobs := map[uint32][]byte{}
	remaining := int64(maxSignature)
	for _, entry := range entries {
		name := entry.Name()
		slot, ok := signatureSlots[name]
		if !ok || !entry.Type().IsRegular() {
			return codeIdentity{}, fmt.Errorf("%w: unexpected signature file %q", ErrUnsupported, name)
		}
		if slot == 3 {
			continue // CodeResources is a plist, already read as an external slot.
		}
		// ReadDir lists the numbered alternates in order. codesign stops at the first gap.
		if slot > 0x1000 && slot <= 0x1004 && blobs[slot-1] == nil {
			return codeIdentity{}, fmt.Errorf("missing alternate CodeDirectory before %s", name)
		}
		blob, err := readRegular(metadata, name, remaining)
		if err != nil {
			return codeIdentity{}, err
		}
		remaining -= int64(len(blob))
		blobs[slot] = blob
		if slot > 0 && slot < 0x1000 {
			// Each metadata file must actually be sealed by the CodeDirectory.
			external[slot] = blob
		}
	}
	return v.verifyComponents(code, size, blobs, external)
}

// An absent envelope is unsigned only when the executable also has no signature.
func (v *bundleVerifier) withoutEnvelope(f *treefs.File, size int64, location string, info []byte) error {
	machO, err := isMachO(f, size)
	if err != nil {
		return err
	}
	if !machO {
		signed, err := v.hasSignatureAttributes(location, f)
		if err != nil {
			return err
		}
		if signed {
			return errors.New("generic executable has signature attributes but no resource envelope")
		}
		return signature.ErrUnsigned
	}
	_, err = verifyMachO(v.ctx, f, size, map[uint32][]byte{1: info}, v.roots)
	if errors.Is(err, signature.ErrUnsigned) {
		return signature.ErrUnsigned
	}
	if err := v.ctx.Err(); err != nil {
		return err
	}
	return errors.New("bundle is missing its signature resource envelope")
}

func (v *bundleVerifier) verifyResources(root fs.ReadLinkFS, location string, data []byte, mainPath, infoPath string) error {
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
		// Seal paths are only compared with the names the bundle holds, which
		// may be any POSIX name.
		if !fs.ValidPath(name) || strings.ContainsRune(name, 0) {
			return fmt.Errorf("unsafe resource seal path %q", name)
		}
		seal, err := parseSeal(value)
		if err != nil {
			return fmt.Errorf("resource %q: %w", name, err)
		}
		seals[name] = seal
	}
	err := treefs.WalkConcurrent(root, func(name string, parent fs.ReadLinkFS, entry fs.DirEntry) error {
		if err := v.ctx.Err(); err != nil {
			return err
		}
		leaf := entry.Name()
		if err := v.recordEntry(); err != nil {
			return err
		}
		seal := seals[name]
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			target, err := parent.ReadLink(leaf)
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
			if name == "_CodeSignature" {
				return fs.SkipDir // Its entries were checked as signature components.
			}
			if seal == nil {
				return nil
			}
			seal.seen = true
			if seal.kind != "nested" {
				return fmt.Errorf("resource %q is a directory but sealed as a %s", name, seal.kind)
			}
			if err := v.verifyNested(parent, path.Join(location, path.Dir(name)), leaf, seal); err != nil {
				return err
			}
			return fs.SkipDir
		case entry.Type().IsRegular():
			if seal == nil {
				if name == mainPath || name == infoPath || unsealedByDefault(name) {
					return nil
				}
				return fmt.Errorf("unsealed resource %q", name)
			}
			seal.seen = true
			switch seal.kind {
			case "file":
				return v.verifySealedFile(parent, leaf, seal.digest)
			case "nested":
				return v.verifyNested(parent, path.Join(location, path.Dir(name)), leaf, seal)
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
			seal.requirement, ok = field.(string)
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
	case seal.requirement != "":
		return nil, fmt.Errorf("%w: requirement without nested code", ErrUnsupported)
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
// PkgInfo, Finder metadata, localization version stamps, its own compatibility
// copy of the envelope at the resource root, and the receipt the App Store
// adds after Apple signs.
func unsealedByDefault(name string) bool {
	return name == "PkgInfo" || name == "CodeResources" || path.Base(name) == ".DS_Store" || strings.HasSuffix(name, ".lproj/locversion.plist") || strings.HasPrefix(name, "_MASReceipt/")
}

func (v *bundleVerifier) verifySealedFile(root fs.ReadLinkFS, name string, expected []byte) error {
	f, size, err := openRegular(root, name)
	if err != nil {
		return err
	}
	buffer, _ := v.buffers.Get().(*[256 << 10]byte)
	if buffer == nil {
		buffer = new([256 << 10]byte)
	}
	defer v.buffers.Put(buffer)
	digest, err := fileDigest(v.ctx, io.LimitReader(f, size), buffer[:])
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if digest != hex.EncodeToString(expected) {
		return fmt.Errorf("resource %q does not match its seal", name)
	}
	return nil
}

// verifyNested authenticates nested code, then binds it to the envelope by its
// sealed cdhash or, for code that replaced the sealed code, by the requirement
// the envelope recorded for it. Invalid nested code fails either way.
func (v *bundleVerifier) verifyNested(root fs.ReadLinkFS, location, name string, seal *resourceSeal) error {
	if v.depth >= maxBundleDepth {
		return fmt.Errorf("%w: nested code deeper than %d bundles", ErrUnsupported, maxBundleDepth)
	}
	info, err := root.Lstat(name)
	if err != nil {
		return err
	}
	location = path.Join(location, name)
	identity, err := v.verifyNestedCode(root, location, name, info.IsDir(), seal)
	if err == nil {
		err = verifyNestedIdentity(identity, seal)
	}
	if err != nil {
		return fmt.Errorf("nested code %q: %w", name, err)
	}
	if !matchCDHash(identity, seal.cdhash) {
		replacement := signature.Replacement{Path: location, Sealed: hex.EncodeToString(seal.cdhash)}
		for _, cdhash := range identity.cdhashes {
			replacement.CDHashes = append(replacement.CDHashes, hex.EncodeToString(cdhash))
		}
		v.mu.Lock()
		v.replaced = append(v.replaced, replacement)
		v.mu.Unlock()
	}
	return nil
}

func verifyNestedIdentity(identity codeIdentity, seal *resourceSeal) error {
	if matchCDHash(identity, seal.cdhash) {
		return nil
	}
	if seal.requirement == "" {
		return errors.New("does not match its sealed cdhash")
	}
	required, err := parseRequirement(seal.requirement)
	if err != nil {
		return fmt.Errorf("does not match its sealed cdhash: %w", err)
	}
	if !required.satisfiedBy(identity) {
		return errors.New("matches neither its sealed cdhash nor its sealed requirement")
	}
	return nil
}

func (v *bundleVerifier) verifyNestedCode(root fs.ReadLinkFS, location, name string, bundle bool, seal *resourceSeal) (identity codeIdentity, err error) {
	if !bundle {
		f, size, openErr := openRegular(root, name)
		if openErr != nil {
			return codeIdentity{}, openErr
		}
		defer closeRead(f, &err)
		machO, err := isMachO(f, size)
		if err != nil {
			return codeIdentity{}, err
		}
		if !machO {
			return v.verifyGeneric(location, f, size)
		}
		return verifyMachO(v.ctx, f, size, nil, v.roots)
	}
	nested, err := subtree(root, name)
	if err != nil {
		return codeIdentity{}, err
	}
	defer closeRead(nested, &err)
	v.depth++
	defer func() { v.depth-- }()
	switch {
	case isDirectory(nested, "Contents"):
		return v.verifyContents(nested, location, path.Base(name))
	case isDirectory(nested, "Versions"):
		return v.verifyFramework(nested, location, path.Base(name), seal)
	case isDirectory(nested, "_CodeSignature"):
		return v.verifyShallow(nested, location, path.Base(name))
	default:
		return codeIdentity{}, fmt.Errorf("%w: nested code layout", ErrUnsupported)
	}
}

func isDirectory(fsys fs.ReadLinkFS, name string) bool {
	info, err := fsys.Lstat(name)
	return err == nil && info.IsDir()
}

func (v *bundleVerifier) recordEntry() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.entries++; v.entries > maxBundleEntries {
		return errors.New("bundle contains too many entries")
	}
	if time.Since(v.progress) < 250*time.Millisecond {
		return nil
	}
	v.progress = time.Now()
	plugin.Logger(v.ctx).InfoContext(v.ctx, "Verifying sealed resources", "progress", true, "current", v.entries, "unit", "entries")
	return nil
}
