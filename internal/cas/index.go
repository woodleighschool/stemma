package cas

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/fileio"
)

// References describe invalidation, not reachability: indexes never pin objects.
type index struct {
	Data    json.RawMessage `json:"data"`
	Objects []string        `json:"objects,omitempty"`
}

func (s *Store) recall(ctx context.Context, dir, key string, value any) bool {
	if !validDigest(key) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(s.Dir, dir, key))
	var entry index
	if err != nil || json.Unmarshal(data, &entry) != nil || len(entry.Data) == 0 {
		return false
	}
	for _, digest := range entry.Objects {
		if !s.HasDigest(digest) {
			return false
		}
	}
	if json.Unmarshal(entry.Data, value) != nil {
		return false
	}
	s.touch(ctx, dir, key)
	return true
}

func (s *Store) remember(ctx context.Context, dir, key string, value any, objects []string) error {
	if !validDigest(key) {
		return os.ErrInvalid
	}
	for _, digest := range objects {
		if !validDigest(digest) {
			return os.ErrInvalid
		}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data, err = json.Marshal(index{Data: data, Objects: objects})
	if err != nil {
		return err
	}
	if err := fileio.Write(filepath.Join(s.Dir, dir, key), data, 0o600); err != nil {
		return err
	}
	s.touch(ctx, dir, key)
	return nil
}

// Recall reads a derivation result only if the referenced object still verifies.
func (s *Store) Recall(ctx context.Context, key string) (Ref, bool) {
	var ref Ref
	if !s.recall(ctx, "derivations", key, &ref) || s.Verify(ctx, ref) != nil {
		return Ref{}, false
	}
	return ref, true
}

// Remember indexes a completed derivation and all objects needed to reuse it.
func (s *Store) Remember(ctx context.Context, key string, ref Ref, dependencies ...Ref) error {
	objects := []string{ref.SHA256}
	for _, dependency := range dependencies {
		objects = append(objects, dependency.SHA256)
	}
	return s.remember(ctx, "derivations", key, ref, objects)
}

// RecallSource reads a disposable source record without certifying content bytes.
func (s *Store) RecallSource(ctx context.Context, key string, record any) bool {
	return s.recall(ctx, "sources", key, record)
}

// RememberSource records a source response and any cached content it describes.
func (s *Store) RememberSource(ctx context.Context, key string, record any, objects ...string) error {
	return s.remember(ctx, "sources", key, record, objects)
}
