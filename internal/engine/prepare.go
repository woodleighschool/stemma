// Package engine orchestrates locked preparation and independent destination reconciliation.
package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	inspection "github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/plugin"
)

// Prepared preserves the original source alongside selected payloads and verification evidence.
type Prepared struct {
	InputsHash    string                     `json:"inputs_hash,omitempty"`
	Payload       cas.Ref                    `json:"payload"`
	Filename      string                     `json:"filename"`
	Format        string                     `json:"format"`
	Version       string                     `json:"version,omitempty"`
	Tree          bool                       `json:"tree,omitempty"`
	Facts         plugin.Facts               `json:"facts"`
	SuppliedFacts bool                       `json:"supplied_facts,omitempty"`
	Evidence      map[string]json.RawMessage `json:"evidence,omitempty"`
	EntryPoint    string                     `json:"entry_point,omitempty"`
	Mode          uint32                     `json:"mode,omitempty"`
	Cached        bool                       `json:"cached"`
	Path          string                     `json:"-"`
}

func materialize(ctx context.Context, store *cas.Store, p Prepared, work string) (Prepared, error) {
	p.Path = filepath.Join(work, "payload", p.Filename)
	if !p.Tree {
		if err := store.Materialize(ctx, p.Payload, p.Path); err != nil {
			return p, err
		}
		return p, os.Chmod(p.Path, os.FileMode(p.Mode))
	}
	packed := filepath.Join(work, "payload.tar")
	if err := store.Materialize(ctx, p.Payload, packed); err != nil {
		return p, err
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil {
		return p, err
	}
	if err := archive.Extract(ctx, packed, p.Path); err != nil {
		return p, err
	}
	return p, os.Chmod(p.Path, os.FileMode(p.Mode))
}

// expose materializes a prepared artifact for the operator at a cache path its
// digest and filename name. The copy is writable and nothing verifies it later,
// so each call replaces it from the verified object.
func expose(ctx context.Context, store *cas.Store, p Prepared, work string) (path string, err error) {
	done := plugin.Stage(ctx, "Materializing artifact", plugin.Detail(p.Filename))
	defer func() { done(err) }()
	p, err = materialize(ctx, store, p, work)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(store.Dir, "materialized", p.Payload.SHA256)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path = filepath.Join(dir, p.Filename)
	if err := os.RemoveAll(path); err != nil {
		return "", err
	}
	if err := os.Rename(p.Path, path); err != nil {
		// Another project sharing the cache materialized the same bytes.
		if _, statErr := os.Lstat(path); statErr != nil {
			return "", err
		}
	}
	return path, nil
}

// Inspection describes a local file or directory by its own metadata.
type Inspection struct {
	Filename string       `json:"filename"`
	Format   string       `json:"format"`
	Version  string       `json:"version,omitempty"`
	Facts    plugin.Facts `json:"facts"`
}

// Inspect reads a local artifact's complete facts without executing it.
func Inspect(ctx context.Context, path string) (result Inspection, err error) {
	done := plugin.Stage(ctx, "Inspecting artifact", plugin.Detail(filepath.Base(path)))
	defer func() { done(err) }()
	facts, err := inspection.Read(ctx, path)
	if err != nil {
		return Inspection{}, err
	}
	return Inspection{Filename: filepath.Base(path), Format: artifactFormat(path, facts), Version: artifactVersion(facts), Facts: facts}, nil
}

// artifactFormat names what the facts show an artifact is, falling back to its
// extension.
func artifactFormat(path string, facts plugin.Facts) string {
	for _, subject := range facts.Subjects {
		if subject.Package != nil {
			return "pkg"
		}
	}
	if len(facts.Subjects) > 0 {
		switch kind := facts.Subjects[0].Kind; kind {
		case "app", "msi", "directory":
			return kind
		}
	}
	return cmp.Or(strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."), "binary")
}

func artifactVersion(facts plugin.Facts) string {
	var version string
	for _, subject := range facts.Subjects {
		var candidate string
		switch {
		case subject.App != nil && subject.Parent == "":
			candidate = subject.App.Version
			if candidate == "" {
				candidate = subject.App.Build
			}
		case subject.Package != nil:
			candidate = subject.Package.Version
		case subject.MSI != nil:
			candidate = subject.MSI.ProductVersion
		default:
			continue
		}
		if candidate == "" {
			return ""
		}
		if version != "" && version != candidate {
			return ""
		}
		version = candidate
	}
	return version
}
