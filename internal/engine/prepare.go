// Package engine orchestrates locked preparation and independent destination reconciliation.
package engine

import (
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

// Inspect reads complete supported artifact facts without acquisition or publication.
func Inspect(ctx context.Context, path string) (result Prepared, err error) {
	done := plugin.Stage(ctx, "Inspecting artifact")
	defer func() { done(err) }()
	p, err := inspect(ctx, path)
	if err != nil {
		return p, err
	}
	p.Facts, err = inspection.Read(ctx, path)
	return p, err
}

func inspect(ctx context.Context, path string) (Prepared, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Prepared{}, err
	}
	facts, err := inspection.ReadMetadata(ctx, path)
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{Filename: filepath.Base(path), Format: strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."), Tree: info.IsDir(), Path: path, Facts: facts}
	if len(facts.Subjects) > 0 {
		switch facts.Subjects[0].Kind {
		case "app", "msi", "directory":
			p.Format = facts.Subjects[0].Kind
		}
		for _, subject := range facts.Subjects {
			if subject.Package != nil {
				p.Format = "pkg"
				break
			}
		}
	}
	if p.Format == "" {
		p.Format = "binary"
	}
	p.Version = artifactVersion(facts)
	return p, nil
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
