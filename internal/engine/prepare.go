// Package engine orchestrates locked preparation and independent destination reconciliation.
package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

// Prepared preserves the original source alongside selected payloads and verification evidence.
type Prepared struct {
	InputsHash    string                     `json:"inputs_hash,omitempty"`
	Payload       cas.Ref                    `json:"payload"`
	Filename      string                     `json:"filename"`
	Format        string                     `json:"format"`
	Version       string                     `json:"version,omitempty"`
	ContentRoot   string                     `json:"content_root,omitempty"`
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

// expose replaces the operator's writable copy from the verified cache object.
func expose(ctx context.Context, store *cas.Store, p Prepared, work, outputFile string) (path string, err error) {
	done := plugin.Stage(ctx, "Materializing artifact", plugin.Detail(p.Filename))
	defer func() { done(err) }()
	p, err = materialize(ctx, store, p, work)
	if err != nil {
		return "", err
	}
	if outputFile != "" {
		return export(ctx, store, p, work, outputFile)
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
		return "", err
	}
	return path, nil
}

func export(ctx context.Context, store *cas.Store, p Prepared, work, target string) (string, error) {
	target, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	// Resolve the parent so an alias of the cache cannot make an export disposable.
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(parent, filepath.Base(target))
	cache, err := filepath.EvalSymlinks(store.Dir)
	if err != nil {
		return "", err
	}
	if rel, err := filepath.Rel(cache, resolved); err == nil && filepath.IsLocal(rel) {
		return "", errors.New("output-file must be outside the disposable cache")
	}
	if p.Tree {
		if _, statErr := os.Lstat(target); !errors.Is(statErr, os.ErrNotExist) {
			return "", fmt.Errorf("output-file already exists or is inaccessible: %s", target)
		}
		if err := archive.Extract(ctx, filepath.Join(work, "payload.tar"), target); err != nil {
			return "", err
		}
	} else {
		var in, out *os.File
		in, err = os.Open(p.Path)
		if err != nil {
			return "", err
		}
		defer func() { _ = in.Close() }()
		out, err = os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(out, fileio.Reader{Context: ctx, Reader: in})
		err = errors.Join(err, out.Close())
	}
	if err == nil {
		err = os.Chmod(target, os.FileMode(p.Mode))
	}
	if err != nil {
		_ = os.RemoveAll(target)
		return "", fmt.Errorf("export artifact: %w", err)
	}
	return target, nil
}

// Inspection describes a local file or directory by its own metadata.
type Inspection struct {
	Filename string       `json:"filename"`
	Format   string       `json:"format"`
	Version  string       `json:"version,omitempty"`
	Facts    plugin.Facts `json:"facts"`
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
