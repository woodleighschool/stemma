// Package macpkg builds declared filesystem layouts as portable component PKGs.
package macpkg

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

// Version changes when the layout or package derivation changes.
const Version = "stemma.macpkg/8"

type Spec struct {
	Inputs     map[string]plugin.Input      `json:"inputs,omitempty" yaml:"inputs,omitempty" jsonschema_description:"Named source artifacts leased into the build. Refer to them with $input in payload and scripts."`
	Payload    map[string]Entry             `json:"payload,omitempty" yaml:"payload,omitempty" jsonschema_description:"Installed absolute paths mapped to files, trees, literal text, relative symlinks or directory declarations."`
	Package    Package                      `json:"package" yaml:"package" jsonschema_description:"Component package identity and version recorded in macOS receipts, with the published filename and payload compression."`
	Scripts    map[string]Script            `json:"scripts,omitempty" yaml:"scripts,omitempty" jsonschema_description:"Literal script text or input selections in the temporary installer Scripts area. Root preinstall and postinstall files are hooks; other entries are resources used by those hooks. Never executed by Stemma."`
	Signatures []signature.InputExpectation `json:"signatures,omitempty" yaml:"signatures,omitempty" jsonschema:"minItems=1" jsonschema_description:"Signing expectations for every physical signing subject consumed by the resolved payload and scripts layout. Unused siblings are excluded. The built package itself is unsigned."`
}

type Package struct {
	Identifier string `json:"identifier" yaml:"identifier" jsonschema_description:"Stable reverse-DNS package identifier written into the installation receipt."`
	Version    string `json:"version" yaml:"version" jsonschema_description:"Package receipt version. Change it when the managed payload changes."`
	Filename   string `json:"filename,omitempty" yaml:"filename,omitempty" jsonschema_description:"Optional published PKG basename. Omit to derive it from the resource name and package version."`
	// Compression applies to the payload. The Scripts area is always gzip.
	Compression pkgbuild.Compression `json:"compression,omitempty" yaml:"compression,omitempty" jsonschema:"enum=gzip,enum=xz" jsonschema_description:"Payload compression: gzip or xz. Omit for gzip. xz builds a smaller package, costs far more CPU to build and read, and installs on macOS 10.10 or later."`
}

// compression returns the declared payload compression, or gzip.
func (p Package) compression() pkgbuild.Compression {
	if p.Compression == "" {
		return pkgbuild.Gzip
	}
	return p.Compression
}

// Entry maps a file or tree into the installed payload. No input or content declares a
// directory. Mode applies to the mapped root; ownership applies to its subtree.
type Entry struct {
	Input   string  `json:"$input,omitempty" yaml:"$input,omitempty" jsonschema_description:"Name of a declared input supplying this file or tree."`
	Path    string  `json:"path,omitempty" yaml:"path,omitempty" jsonschema_description:"Exact relative path in a tree, ZIP, TAR or DMG. A dot selects the contents root. Omit to retain the original input; PKGs remain opaque files."`
	Content *string `json:"content,omitempty" yaml:"content,omitempty" jsonschema_description:"Literal UTF-8 file contents. Mutually exclusive with $input; omit both to create a directory."`
	Symlink string  `json:"symlink,omitempty" yaml:"symlink,omitempty" jsonschema_description:"Relative link target in the installed filesystem, confined to the package root. Mutually exclusive with $input and content."`
	Mode    string  `json:"mode,omitempty" yaml:"mode,omitempty" jsonschema:"pattern=^0?[0-7]{3}$" jsonschema_description:"Octal permissions for the mapped root, for example 0644 for a file or 0755 for a directory."`
	UID     uint32  `json:"uid,omitempty" yaml:"uid,omitempty" jsonschema:"maximum=262143" jsonschema_description:"Numeric owner applied to the mapped subtree. Defaults to root (0)."`
	GID     uint32  `json:"gid,omitempty" yaml:"gid,omitempty" jsonschema:"maximum=262143" jsonschema_description:"Numeric group applied to the mapped subtree. Defaults to wheel (0)."`
}

func (s Spec) Filename() string {
	if s.Package.Filename != "" {
		return s.Package.Filename
	}
	return s.Package.Identifier + "-" + s.Package.Version + ".pkg"
}

func (s Spec) Validate() error {
	filename := s.Filename()
	if !validPath(filename) || path.Base(filename) != filename || !strings.HasSuffix(filename, ".pkg") {
		return errors.New("package filename must be a base filename ending in .pkg")
	}
	switch s.Package.Compression {
	case "", pkgbuild.Gzip, pkgbuild.XZ:
	default:
		return errors.New("package.compression must be gzip or xz")
	}
	opts := pkgbuild.Options{Identifier: s.Package.Identifier, Version: s.Package.Version}
	if len(s.Payload) > 0 {
		opts.Payload, opts.Compression = "Payload", s.Package.compression()
	} else if s.Package.Compression != "" {
		return errors.New("package.compression requires a payload")
	}
	seen := map[string]bool{}
	for endpoint, entry := range s.Payload {
		name, err := payloadPath(endpoint)
		if err != nil {
			return fmt.Errorf("payload %q: %w", endpoint, err)
		}
		if seen[name] {
			return fmt.Errorf("payload %q duplicates another normalized destination", endpoint)
		}
		seen[name] = true
		if entry.Symlink != "" && !validSymlink(name, entry.Symlink) {
			return fmt.Errorf("payload %q: symlink must be relative and confined to the package root", endpoint)
		}
		if err := entry.validate(); err != nil {
			return fmt.Errorf("payload %q: %w", endpoint, err)
		}
		if entry.Input != "" {
			if err := s.validateRef(entry.Input, entry.Path); err != nil {
				return fmt.Errorf("payload %q: %w", endpoint, err)
			}
		}
	}
	if len(s.Scripts) > 0 {
		opts.Scripts = "Scripts"
	}
	for name, entry := range s.Scripts {
		if !validPath(name) {
			return fmt.Errorf("scripts %q: destination must be confined and relative", name)
		}
		if err := entry.validate(); err != nil {
			return fmt.Errorf("scripts %q: %w", name, err)
		}
		if entry.Input != "" {
			if err := s.validateRef(entry.Input, entry.Path); err != nil {
				return fmt.Errorf("scripts %q: %w", name, err)
			}
		}
	}
	for i, expected := range s.Signatures {
		if err := s.validateRef(expected.Input, ""); err != nil {
			return fmt.Errorf("signatures[%d]: %w", i, err)
		}
		if err := expected.Validate(signature.AppleDeveloperID); err != nil {
			return fmt.Errorf("signatures[%d]: %w", i, err)
		}
	}
	return pkgbuild.Validate(opts)
}

func (s Spec) validateRef(input, name string) error {
	if _, exists := s.Inputs[input]; input == "" || !exists {
		return fmt.Errorf("unknown input %q", input)
	}
	if name != "" && !validPath(name) {
		return errors.New("input path must be confined and relative")
	}
	return nil
}

func (e Entry) validate() error {
	choices := 0
	if e.Input != "" {
		choices++
	}
	if e.Content != nil {
		choices++
	}
	if e.Symlink != "" {
		choices++
	}
	if choices > 1 {
		return errors.New("use only one of $input, content or symlink")
	}
	if e.Input == "" && e.Path != "" {
		return errors.New("path requires $input")
	}
	if _, err := e.mode(); err != nil {
		return err
	}
	if e.UID > 0o777777 || e.GID > 0o777777 {
		return errors.New("UID/GID exceeds the PKG archive limit of 262143")
	}
	return nil
}

func (e Entry) mode() (*uint32, error) {
	if e.Mode == "" {
		return nil, nil
	}
	value, err := strconv.ParseUint(e.Mode, 8, 9)
	if err != nil || len(e.Mode) < 3 || len(e.Mode) > 4 {
		return nil, errors.New("mode must be three or four octal digits without special permission bits")
	}
	mode := uint32(value)
	return &mode, nil
}

func payloadPath(value string) (string, error) {
	if value == "" || strings.ContainsAny(value, "\\\x00\r\n\t") || !utf8.ValidString(value) {
		return "", errors.New("destination must be a POSIX payload path")
	}
	for segment := range strings.SplitSeq(value, "/") {
		if segment == ".." {
			return "", errors.New("destination must not traverse parent directories")
		}
	}
	name := path.Clean(strings.TrimPrefix(value, "/"))
	if !validPath(name) {
		return "", errors.New("destination must remain within the payload root")
	}
	return name, nil
}

func validPath(value string) bool {
	return fs.ValidPath(value) && len(value) <= 4096 && utf8.ValidString(value) && !strings.ContainsAny(value, "\\\x00\r\n\t")
}
