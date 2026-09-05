package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
)

func derivePackage(ctx context.Context, store *cas.Store, input Prepared, spec config.Artifact, policy config.Verification, work string) (Prepared, error) {
	if !input.Tree {
		return Prepared{}, errors.New("package artifacts require a directory source")
	}
	if input.Source.ResolvedAt.IsZero() {
		return Prepared{}, errors.New("package source has no locked timestamp; run stemma update")
	}
	key := config.Fingerprint(struct {
		Implementation string
		Input          cas.Ref
		Timestamp      time.Time
		Artifact       config.Artifact
		Verification   config.Verification
	}{pkgbuild.Version, input.Payload, input.Source.ResolvedAt, spec, policy})
	if descriptor, ok := store.Recall(ctx, key); ok {
		path, err := store.Path(descriptor)
		if err != nil {
			return Prepared{}, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return Prepared{}, err
		}
		var result Prepared
		if json.Unmarshal(data, &result) == nil && store.Verify(ctx, result.Payload) == nil {
			result.Source = input.Source
			if policy.Subject == "source" {
				result.Evidence = input.Evidence
			}
			result.Cached = true
			return materialize(ctx, store, result, work)
		}
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return Prepared{}, err
	}
	filename := spec.Filename
	if filename == "" {
		filename = spec.Identifier + ".pkg"
	}
	output := filepath.Join(work, filename)
	err := pkgbuild.Build(ctx, input.Path, output, pkgbuild.Options{Identifier: spec.Identifier, Version: spec.Version, InstallLocation: spec.InstallLocation, Payload: spec.Payload, Scripts: spec.Scripts, Timestamp: input.Source.ResolvedAt})
	if err != nil {
		return Prepared{}, err
	}
	result, err := inspect(output)
	if err != nil {
		return result, err
	}
	result.Source = input.Source
	if policy.Subject != "source" && requested(policy) {
		evidence, err := verify(output, policy)
		result.Evidence = &evidence
		if err != nil {
			return result, err
		}
	} else if policy.Subject == "source" {
		result.Evidence = input.Evidence
	}
	result.Payload, err = store.ImportFile(ctx, output, "")
	if err != nil {
		return result, err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	descriptor, err := store.Import(ctx, bytes.NewReader(data), "")
	if err != nil {
		return result, err
	}
	return result, store.Remember(key, descriptor)
}

func destinationMetadata(input map[string]any) map[string]any {
	result := config.Merge(input, nil)
	delete(result, "artifact")
	return result
}
