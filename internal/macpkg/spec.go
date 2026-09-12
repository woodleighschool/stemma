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
	"github.com/woodleighschool/stemma/plugin"
)

// Version changes when the layout or package derivation changes.
const Version = "stemma.macpkg/1"

type Spec struct {
	Inputs  map[string]plugin.Input `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Payload map[string]Entry        `json:"payload,omitempty" yaml:"payload,omitempty"`
	Package Package                 `json:"package" yaml:"package"`
	Scripts map[string]InputFileRef `json:"scripts,omitempty" yaml:"scripts,omitempty"`
}

type Package struct {
	Identifier string `json:"identifier" yaml:"identifier"`
	Version    string `json:"version" yaml:"version"`
	Filename   string `json:"filename,omitempty" yaml:"filename,omitempty"`
}

// Entry maps a file or tree to its payload key. No input or content declares a
// directory. Mode applies to the mapped root; ownership applies to its subtree.
type Entry struct {
	Input   string  `json:"$input,omitempty" yaml:"$input,omitempty"`
	Path    string  `json:"path,omitempty" yaml:"path,omitempty"`
	Content *string `json:"content,omitempty" yaml:"content,omitempty"`
	Mode    string  `json:"mode,omitempty" yaml:"mode,omitempty" jsonschema:"pattern=^0?[0-7]{3}$"`
	UID     uint32  `json:"uid,omitempty" yaml:"uid,omitempty" jsonschema:"maximum=262143"`
	GID     uint32  `json:"gid,omitempty" yaml:"gid,omitempty" jsonschema:"maximum=262143"`
}

type InputFileRef struct {
	Input string `json:"$input" yaml:"$input"`
	Path  string `json:"path,omitempty" yaml:"path,omitempty"`
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
	opts := pkgbuild.Options{Identifier: s.Package.Identifier, Version: s.Package.Version, Scripts: map[string]string{}}
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
			if err := s.validateRef(InputFileRef{Input: entry.Input, Path: entry.Path}); err != nil {
				return fmt.Errorf("payload %q: %w", endpoint, err)
			}
		}
	}
	for role, ref := range s.Scripts {
		if err := s.validateRef(ref); err != nil {
			return fmt.Errorf("scripts.%s: %w", role, err)
		}
		opts.Scripts[role] = "Scripts/" + role
	}
	return pkgbuild.Validate(opts)
}

func (s Spec) validateRef(ref InputFileRef) error {
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
