package munki

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"

	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

// DocumentInput reads an explicitly rendered pkginfo artifact and binds it to
// its installer input before native destination derivation.
func DocumentInput(ctx context.Context, request plugin.ReconcileRequest) (plugin.ReconcileRequest, error) {
	installer, exists := request.Inputs["installer"]
	if !exists && request.Artifact.Format != "json" {
		return request, nil
	}
	if request.Artifact.Size < 0 || request.Artifact.Size > 4<<20 {
		return request, errors.New("pkginfo JSON exceeds size limit")
	}
	file, err := os.Open(request.Artifact.Path)
	if err != nil {
		return request, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(fileio.Reader{Context: ctx, Reader: file}, (4<<20)+1))
	if err != nil {
		return request, err
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != request.Artifact.Size || hex.EncodeToString(digest[:]) != request.Artifact.SHA256 {
		return request, errors.New("pkginfo JSON differs from its immutable descriptor")
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return request, err
	}
	if document == nil {
		return request, errors.New("pkginfo JSON must be an object")
	}
	if document["installer_type"] == "nopkg" {
		if exists {
			return request, errors.New("nopkg must not include an installer input")
		}
		for _, key := range []string{"installer_item_location", "installer_item_hash", "installer_item_size"} {
			if _, supplied := document[key]; supplied {
				return request, errors.New("nopkg must not include installer content")
			}
		}
	} else {
		if !exists {
			return request, errors.New("pkginfo requires an installer input")
		}
		if document["installer_item_hash"] != installer.SHA256 {
			return request, errors.New("pkginfo installer hash does not match installer input")
		}
		size, ok := document["installer_item_size"].(float64)
		if !ok || size != float64((installer.Size+1023)/1024) {
			return request, errors.New("pkginfo installer size does not match installer input")
		}
	}
	// Content locations and computed removals belong to this repository's native
	// renderer. The document's editable fields retain their presence semantics.
	for _, key := range []string{"installer_item_location", "installer_item_hash", "installer_item_size", "items_to_remove"} {
		delete(document, key)
	}
	var explicit map[string]any
	if len(request.Metadata) != 0 {
		if err := json.Unmarshal(request.Metadata, &explicit); err != nil {
			return request, err
		}
	}
	native, _ := explicit["pkginfo"].(map[string]any)
	if explicit == nil {
		explicit = map[string]any{}
	}
	maps.Copy(document, native)
	explicit["pkginfo"] = document
	request.Metadata, err = json.Marshal(explicit)
	request.Artifact, request.Facts = installer, installer.Facts
	return request, err
}
