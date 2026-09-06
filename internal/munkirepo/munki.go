// Package munkirepo reconciles native pkginfo, installers and shared Munki catalogs.
package munkirepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
)

// Handle implements read-only planning and content-first publication to a local repository.
// Apply serializes writers sharing this repository; external writers must use the same lock.
func Handle(ctx context.Context, request plugin.Request) (plugin.Response, error) {
	var connection struct {
		Path string `json:"path"`
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Config))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&connection); err != nil {
		return plugin.Response{}, err
	}
	if connection.Path == "" {
		return plugin.Response{}, errors.New("munki repository path is required")
	}
	if request.Method == "validate" {
		_, _, err := nativeInput(request)
		return plugin.Response{}, err
	}
	if request.Method != "plan" && request.Method != "apply" {
		return plugin.Response{}, fmt.Errorf("unsupported Munki method %q", request.Method)
	}
	if request.Method == "apply" {
		if err := os.MkdirAll(connection.Path, 0o755); err != nil {
			return plugin.Response{}, err
		}
		lock := flock.New(filepath.Join(connection.Path, ".stemma.lock"))
		ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
		if err != nil {
			return plugin.Response{}, err
		}
		if !ok {
			return plugin.Response{}, ctx.Err()
		}
		defer func() { _ = lock.Close() }()
	}
	return reconcile(ctx, connection.Path, request)
}

func nativeInput(request plugin.Request) (munki.Input, map[string]any, error) {
	var metadata map[string]any
	if err := json.Unmarshal(request.Metadata, &metadata); err != nil {
		return munki.Input{}, nil, err
	}
	if metadata == nil {
		return munki.Input{}, nil, errors.New("munki metadata must be an object")
	}
	input := munki.Input{Name: request.Identity.Recipe, Version: request.Artifact.Version, SHA256: request.Artifact.SHA256, Size: request.Artifact.Size}
	for key, value := range metadata {
		if key != "name" && key != "version" && key != "installer_type" {
			continue
		}
		text, ok := value.(string)
		if !ok || text == "" {
			return input, nil, fmt.Errorf("%s must be a nonempty string", key)
		}
		switch key {
		case "name":
			input.Name = text
		case "version":
			input.Version = text
		case "installer_type":
			input.InstallerType = text
		}
		delete(metadata, key)
	}
	if input.InstallerType == "" && strings.EqualFold(filepath.Ext(request.Artifact.Filename), ".pkg") {
		input.InstallerType = "pkg"
	}
	if input.InstallerType == "nopkg" {
		input.Size = 0
		input.SHA256 = ""
	} else {
		input.InstallerLocation = "stemma/" + request.Artifact.SHA256 + "/" + request.Artifact.Filename
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return input, nil, err
	}
	input.Metadata = raw
	if _, err := munki.DecodeMetadata(raw); err != nil {
		return input, nil, err
	}
	return input, metadata, nil
}

func reconcile(ctx context.Context, root string, request plugin.Request) (plugin.Response, error) {
	input, managed, err := nativeInput(request)
	if err != nil {
		return plugin.Response{}, err
	}
	identity := config.Fingerprint(request.Identity)
	pkginfoPath := filepath.Join("pkgsinfo", "stemma", identity+".plist")
	old, err := readObject(filepath.Join(root, pkginfoPath))
	if err != nil {
		return plugin.Response{}, err
	}
	if old != nil && owner(old) != identity {
		return plugin.Response{}, fmt.Errorf("pkginfo %s is not owned by this destination", pkginfoPath)
	}
	desired := config.Merge(old, nil)
	for key, value := range managed {
		if value == nil {
			delete(desired, key)
		} else {
			desired[key] = value
		}
	}
	if input.InstallerType == "" {
		input.InstallerType, _ = old["installer_type"].(string)
	}
	if input.InstallerType == "nopkg" {
		input.Size, input.SHA256, input.InstallerLocation = 0, "", ""
	}
	// Validate the effective supported fields while retaining unknown native fields.
	effective := map[string]any{}
	for field := range reflect.TypeFor[munki.Metadata]().Fields() {
		key, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if value, exists := desired[key]; key != "-" && exists {
			effective[key] = value
		}
	}
	input.Metadata, err = json.Marshal(effective)
	if err != nil {
		return plugin.Response{}, err
	}
	encoded, err := munki.Build(input)
	if err != nil {
		return plugin.Response{}, err
	}
	var generated map[string]any
	if _, err := plist.Unmarshal(encoded, &generated); err != nil {
		return plugin.Response{}, err
	}
	for key, value := range managed {
		if value != nil {
			desired[key] = generated[key]
		}
	}
	for _, key := range []string{"name", "version", "installer_item_location", "installer_item_hash", "installer_item_size", "installer_type"} {
		if value, ok := generated[key]; ok {
			desired[key] = value
		} else {
			delete(desired, key)
		}
	}
	_, copiesManaged := managed["items_to_copy"]
	_, removalManaged := managed["uninstall_method"]
	if old == nil || copiesManaged || removalManaged {
		if value, exists := generated["items_to_remove"]; exists {
			desired["items_to_remove"] = value
		} else {
			delete(desired, "items_to_remove")
		}
	}
	if _, managedCatalogs := managed["catalogs"]; !managedCatalogs && old == nil {
		desired["catalogs"] = []string{"testing"}
	}
	metadata, _ := desired["_metadata"].(map[string]any)
	metadata = config.Merge(metadata, map[string]any{"stemma": identity})
	desired["_metadata"] = metadata
	response := plugin.Response{}
	contentPath := ""
	if input.InstallerType != "nopkg" {
		contentPath = filepath.Join(root, "pkgs", filepath.FromSlash(input.InstallerLocation))
		matches, err := fileMatches(ctx, contentPath, request.Artifact)
		if err != nil {
			return response, err
		}
		if !matches {
			response.Changes = append(response.Changes, plugin.Change{Kind: "content", Field: "installer_item_hash", Action: "upload", After: raw(request.Artifact.SHA256)})
		}
	}
	for key, value := range desired {
		if config.Fingerprint(old[key]) != config.Fingerprint(value) {
			response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: key, Action: "set", Before: raw(old[key]), After: raw(value)})
		}
	}
	for key, value := range old {
		if _, exists := desired[key]; !exists {
			response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: key, Action: "clear", Before: raw(value)})
		}
	}
	catalogs, err := catalogChanges(root, old, desired)
	if err != nil {
		return response, err
	}
	for name := range catalogs {
		response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: "catalogs/" + name, Action: "reconcile"})
	}
	sort.Slice(response.Changes, func(i, j int) bool {
		a, b := response.Changes[i], response.Changes[j]
		return a.Kind+"/"+a.Field < b.Kind+"/"+b.Field
	})
	response.Binding = raw(map[string]string{"pkginfo": filepath.ToSlash(pkginfoPath), "identity": identity})
	if request.Method != "apply" {
		return response, nil
	}
	// No catalog references an installer before its complete bytes are available.
	if contentPath != "" {
		if err := publishContent(ctx, contentPath, request.Artifact); err != nil {
			return response, err
		}
	}
	names := make([]string, 0, len(catalogs))
	for name := range catalogs {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		data, err := plist.MarshalIndent(catalogs[name], plist.XMLFormat, "  ")
		if err != nil {
			return response, err
		}
		if err := fileio.Write(filepath.Join(root, "catalogs", name), data, 0o644); err != nil {
			return response, err
		}
	}
	// Keep the previous catalog membership until every catalog is published, so a
	// retry after interruption still knows which old catalogs require removal.
	if config.Fingerprint(old) != config.Fingerprint(desired) {
		data, err := plist.MarshalIndent(desired, plist.XMLFormat, "  ")
		if err != nil {
			return response, err
		}
		if err := fileio.Write(filepath.Join(root, pkginfoPath), data, 0o644); err != nil {
			return response, err
		}
	}
	return response, nil
}

func catalogChanges(root string, old, desired map[string]any) (map[string][]map[string]any, error) {
	names := map[string]bool{"all": true}
	for _, item := range []map[string]any{old, desired} {
		for _, name := range catalogNames(item) {
			names[name] = true
		}
	}
	changes := map[string][]map[string]any{}
	for name := range names {
		if name == "" || !filepath.IsLocal(name) || strings.ContainsAny(name, "/\\:") {
			return nil, fmt.Errorf("unsafe catalog name %q", name)
		}
		var existing []map[string]any
		data, err := os.ReadFile(filepath.Join(root, "catalogs", name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err == nil {
			if len(data) > 32<<20 {
				return nil, errors.New("catalog exceeds 32 MiB")
			}
			if _, err := plist.Unmarshal(data, &existing); err != nil {
				return nil, err
			}
		}
		var merged []map[string]any
		for _, entry := range existing {
			if owner(entry) == owner(desired) {
				continue
			}
			merged = append(merged, entry)
		}
		include := name == "all"
		for _, catalog := range catalogNames(desired) {
			if catalog == name {
				include = true
			}
		}
		if include {
			merged = append(merged, desired)
		}
		if merged == nil {
			merged = []map[string]any{}
		}
		sort.SliceStable(merged, func(i, j int) bool {
			return fmt.Sprint(merged[i]["name"])+"/"+fmt.Sprint(merged[i]["version"]) < fmt.Sprint(merged[j]["name"])+"/"+fmt.Sprint(merged[j]["version"])
		})
		if config.Fingerprint(existing) != config.Fingerprint(merged) {
			changes[name] = merged
		}
	}
	return changes, nil
}

func owner(item map[string]any) string {
	metadata, _ := item["_metadata"].(map[string]any)
	identity, _ := metadata["stemma"].(string)
	return identity
}
func catalogNames(item map[string]any) []string {
	var result []string
	switch values := item["catalogs"].(type) {
	case []any:
		for _, value := range values {
			if name, ok := value.(string); ok {
				result = append(result, name)
			}
		}
	case []string:
		result = values
	}
	return result
}
func readObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, errors.New("pkginfo exceeds 8 MiB")
	}
	var value map[string]any
	_, err = plist.Unmarshal(data, &value)
	return value, err
}
func raw(value any) json.RawMessage { data, _ := json.Marshal(value); return data }

func fileMatches(ctx context.Context, path string, artifact plugin.Artifact) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() || info.Size() != artifact.Size {
		return false, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, fileio.Reader{Context: ctx, Reader: f}); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == artifact.SHA256, nil
}
func publishContent(ctx context.Context, path string, artifact plugin.Artifact) error {
	if matches, err := fileMatches(ctx, path, artifact); err != nil || matches {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".stemma-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	source, err := os.Open(artifact.Path)
	if err != nil {
		_ = f.Close()
		return err
	}
	defer func() { _ = source.Close() }()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(fileio.Reader{Context: ctx, Reader: source}, artifact.Size+1))
	if err == nil && (n != artifact.Size || hex.EncodeToString(h.Sum(nil)) != artifact.SHA256) {
		err = errors.New("leased artifact changed before publication")
	}
	if err == nil {
		err = f.Chmod(0o644)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
