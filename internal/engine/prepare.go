// Package engine orchestrates locked preparation and independent destination reconciliation.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	inspection "github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Prepared preserves the original source alongside selected payloads and verification evidence.
type Prepared struct {
	Source        source.Entry    `json:"source"`
	Payload       cas.Ref         `json:"payload"`
	Filename      string          `json:"filename"`
	Format        string          `json:"format"`
	Version       string          `json:"version,omitempty"`
	Tree          bool            `json:"tree,omitempty"`
	Facts         plugin.Facts    `json:"facts"`
	SuppliedFacts bool            `json:"supplied_facts,omitempty"`
	Evidence      *apple.Evidence `json:"evidence,omitempty"`
	Cached        bool            `json:"cached"`
	Path          string          `json:"-"`
}

func prepare(ctx context.Context, store *cas.Store, entry source.Entry, recipe config.Recipe, work string) (Prepared, error) {
	key := config.Fingerprint(struct {
		Implementation                   string
		Source                           cas.Ref
		Filename, Select, Platform, Arch string
		Tree                             bool
		Verification                     config.Verification
	}{"prepare/2", entry.Artifact, entry.Filename, recipe.Select, recipe.Platform, recipe.Arch, entry.Tree, recipe.Verification})
	if descriptor, ok := store.Recall(ctx, key); ok {
		path, err := store.Path(descriptor)
		if err != nil {
			return Prepared{}, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Prepared{}, err
		}
		var prepared Prepared
		if json.Unmarshal(data, &prepared) == nil && store.Verify(ctx, prepared.Payload) == nil {
			prepared.Source = entry
			prepared.Cached = true
			return materialize(ctx, store, prepared, work)
		}
	}
	input := filepath.Join(work, "source", entry.Filename)
	if entry.Tree {
		input += ".tar"
	}
	if err := store.Materialize(ctx, entry.Artifact, input); err != nil {
		return Prepared{}, err
	}
	payload := input
	switch {
	case entry.Tree:
		payload = filepath.Join(work, entry.Filename)
		if err := archive.Extract(ctx, input, payload); err != nil {
			return Prepared{}, err
		}
		if recipe.Select != "" {
			selected, err := archive.Select(payload, recipe.Select)
			if err != nil {
				return Prepared{}, err
			}
			payload = selected
		}
	case isArchive(entry.Filename):
		extracted := filepath.Join(work, "expanded")
		if err := archive.Extract(ctx, input, extracted); err != nil {
			return Prepared{}, err
		}
		selected, err := archive.Select(extracted, recipe.Select)
		if err != nil {
			return Prepared{}, err
		}
		payload = selected
	case recipe.Select != "":
		return Prepared{}, errors.New("select requires a supported archive source")
	}
	prepared, err := inspect(ctx, payload)
	if err != nil {
		return prepared, err
	}
	prepared.Source = entry
	verificationPath := payload
	if recipe.Verification.Subject == "source" {
		verificationPath = input
		if entry.Tree {
			verificationPath = filepath.Join(work, entry.Filename)
		}
	}
	if requested(recipe.Verification) {
		evidence, err := verify(verificationPath, recipe.Verification)
		prepared.Evidence = &evidence
		if err != nil {
			return prepared, err
		}
	}
	if prepared.Tree {
		packed, err := os.CreateTemp(work, "payload-*.tar")
		if err != nil {
			return prepared, err
		}
		packErr := archive.Pack(ctx, payload, packed)
		closeErr := packed.Close()
		if packErr != nil {
			return prepared, packErr
		}
		if closeErr != nil {
			return prepared, closeErr
		}
		prepared.Payload, err = store.ImportFile(ctx, packed.Name(), "")
		if err != nil {
			return prepared, err
		}
	} else {
		prepared.Payload, err = store.ImportFile(ctx, payload, "")
		if err != nil {
			return prepared, err
		}
	}
	prepared.Path = payload
	data, err := json.Marshal(prepared)
	if err != nil {
		return prepared, err
	}
	descriptor, err := store.Import(ctx, bytes.NewReader(data), "")
	if err != nil {
		return prepared, err
	}
	err = store.Remember(key, descriptor)
	return prepared, err
}

func materialize(ctx context.Context, store *cas.Store, p Prepared, work string) (Prepared, error) {
	p.Path = filepath.Join(work, "payload", p.Filename)
	if !p.Tree {
		return p, store.Materialize(ctx, p.Payload, p.Path)
	}
	packed := filepath.Join(work, "payload.tar")
	if err := store.Materialize(ctx, p.Payload, packed); err != nil {
		return p, err
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o700); err != nil {
		return p, err
	}
	return p, archive.Extract(ctx, packed, p.Path)
}

// Inspect reads complete supported artifact facts without acquisition or publication.
func Inspect(ctx context.Context, path string) (Prepared, error) {
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

func isArchive(name string) bool {
	lower := strings.ToLower(name)
	return strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".tar") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tgz")
}
func requested(v config.Verification) bool {
	return v.Integrity || v.Signature || v.Resources || v.Identity || v.Platform || v.CertificateSHA256 != ""
}

func verify(path string, v config.Verification) (apple.Evidence, error) {
	policy := apple.Policy{RequireIntegrity: v.Integrity || v.Resources || v.Signature || v.Identity || v.CertificateSHA256 != "", RequireSignature: v.Signature || v.Identity || v.CertificateSHA256 != "", RequireResources: v.Resources, RequireIdentity: v.Identity, CertificateSHA256: v.CertificateSHA256, RequirePlatform: v.Platform}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pkg":
		return apple.VerifyPackage(path, policy)
	case ".app":
		return apple.VerifyApp(path, policy)
	case ".exe", ".msi", ".dmg", ".zip", ".tar", ".gz":
		return apple.Evidence{}, fmt.Errorf("required verification is unsupported for %s", filepath.Ext(path))
	default:
		return apple.VerifyMachO(path, policy)
	}
}
