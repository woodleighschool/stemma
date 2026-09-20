// Package macpkg builds declared filesystem layouts as portable component PKGs.
package macpkg

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

// Version changes when the layout or package derivation changes.
const Version = "stemma.macpkg/4"

type Spec struct {
	Inputs  map[string]plugin.Input `json:"inputs,omitempty" yaml:"inputs,omitempty" jsonschema_description:"Named source artifacts leased into the build. Refer to them with $input in payload and scripts."`
	Payload map[string]Entry        `json:"payload,omitempty" yaml:"payload,omitempty" jsonschema_description:"Installed absolute paths mapped to files, trees, literal text or directory declarations."`
	Package Package                 `json:"package" yaml:"package" jsonschema_description:"Component package identity and version recorded in macOS receipts."`
	Scripts map[string]Entry        `json:"scripts,omitempty" yaml:"scripts,omitempty" jsonschema_description:"Paths in the temporary installer Scripts area. Root preinstall and postinstall files are hooks; other entries are resources used by those hooks. Never executed by Stemma."`
	Inspect map[string]Inspection   `json:"inspect,omitempty" yaml:"inspect,omitempty" jsonschema_description:"Named input selections inspected before evaluating package and layout expressions. Selected subjects are available under facts; optional signatures authenticate vendor content."`
}

type Inspection struct {
	Input     string                 `json:"$input" yaml:"$input" jsonschema_description:"Declared input to inspect."`
	Path      string                 `json:"path,omitempty" yaml:"path,omitempty" jsonschema_description:"Exact path within the input contents. Omit to inspect the original artifact."`
	Subject   plugin.SubjectSelector `json:"subject,omitzero" yaml:"subject,omitempty" jsonschema_description:"Select one observed subject. Omit when the selection contains exactly one subject."`
	Signature *signature.Policy      `json:"signature,omitempty" yaml:"signature,omitempty" jsonschema_description:"Require the selected app or PKG to be signed by this Developer ID team before building."`
}

type Package struct {
	Identifier string `json:"identifier" yaml:"identifier" jsonschema_description:"Stable reverse-DNS package identifier written into the installation receipt."`
	Version    string `json:"version" yaml:"version" jsonschema_description:"Package receipt version. Change it when the managed payload changes."`
	Filename   string `json:"filename,omitempty" yaml:"filename,omitempty" jsonschema_description:"Optional published PKG basename. Omit to derive it from the resource name and package version."`
}

// Entry maps a file or tree into an output area. No input or content declares a
// directory. Mode applies to the mapped root; ownership applies to its subtree.
type Entry struct {
	Input   string  `json:"$input,omitempty" yaml:"$input,omitempty" jsonschema_description:"Name of a declared input supplying this file or tree."`
	Path    string  `json:"path,omitempty" yaml:"path,omitempty" jsonschema_description:"Exact relative path in a tree, ZIP, TAR or DMG. A dot selects the contents root. Omit to retain the original input; PKGs remain opaque files."`
	Content *string `json:"content,omitempty" yaml:"content,omitempty" jsonschema_description:"Literal UTF-8 file contents. Mutually exclusive with $input; omit both to create a directory."`
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
	if err := s.validateInspections(); err != nil {
		return err
	}
	filename := s.Filename()
	if !validPath(filename) || path.Base(filename) != filename || !strings.HasSuffix(filename, ".pkg") {
		return errors.New("package filename must be a base filename ending in .pkg")
	}
	opts := pkgbuild.Options{Identifier: s.Package.Identifier, Version: s.Package.Version}
	if len(s.Payload) > 0 {
		opts.Payload = "Payload"
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
		if err := entry.validate(); err != nil {
			return fmt.Errorf("payload %q: %w", endpoint, err)
		}
		if entry.Input != "" {
			if err := s.validateRef(entry); err != nil {
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
			if err := s.validateRef(entry); err != nil {
				return fmt.Errorf("scripts %q: %w", name, err)
			}
		}
	}
	return pkgbuild.Validate(opts)
}

func (s Spec) validateInspections() error {
	for _, name := range slices.Sorted(maps.Keys(s.Inspect)) {
		inspection := s.Inspect[name]
		if !validPath(name) || path.Base(name) != name || name == "." {
			return fmt.Errorf("invalid inspection name %q", name)
		}
		if err := s.validateRef(Entry{Input: inspection.Input, Path: inspection.Path}); err != nil {
			return fmt.Errorf("inspect %s: %w", name, err)
		}
		if inspection.Signature != nil {
			signer, err := signature.Parse(inspection.Signature.Signer)
			if err != nil {
				return fmt.Errorf("inspect %s signature: %w", name, err)
			}
			if signer.Scheme != signature.AppleDeveloperID {
				return fmt.Errorf("inspect %s requires an Apple Developer ID signer", name)
			}
		}
	}
	return nil
}

func (s Spec) validateRef(ref Entry) error {
	if _, exists := s.Inputs[ref.Input]; ref.Input == "" || !exists {
		return fmt.Errorf("unknown input %q", ref.Input)
	}
	if ref.Path != "" && !validPath(ref.Path) {
		return errors.New("input path must be confined and relative")
	}
	return nil
}

func (e Entry) validate() error {
	if e.Input != "" && e.Content != nil {
		return errors.New("use either $input or content")
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
