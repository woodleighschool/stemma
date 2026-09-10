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
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/plugin"
	"howett.net/plist"
)

// Handle implements read-only planning and content-first publication to a local repository.
// Apply serializes writers sharing this repository; external writers must use the same lock.
func Handle(ctx context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
	var inputErr error
	request, inputErr = munki.DocumentInput(ctx, request)
	if inputErr != nil {
		return plugin.ReconcileResponse{}, inputErr
	}
	root, err := repositoryPath(request.Config)
	if err == nil && !filepath.IsAbs(root) && request.Root != "" {
		root = filepath.Join(request.Root, root)
	}
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	if request.Method == "validate" && !request.Prepared && request.Artifact.Path == "" {
		_, err := munki.DecodeDestination(request.Metadata)
		return plugin.ReconcileResponse{}, err
	}
	if request.Method == "validate" {
		input, _, err := nativeInput(request)
		if err == nil {
			_, err = munki.Build(input)
		}
		return plugin.ReconcileResponse{}, err
	}
	if request.Method != "plan" && request.Method != "apply" {
		return plugin.ReconcileResponse{}, fmt.Errorf("unsupported Munki method %q", request.Method)
	}
	if request.Method == "apply" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return plugin.ReconcileResponse{}, err
		}
		lock := flock.New(filepath.Join(root, ".stemma.lock"))
		ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
		if err != nil {
			return plugin.ReconcileResponse{}, err
		}
		if !ok {
			return plugin.ReconcileResponse{}, ctx.Err()
		}
		defer func() { _ = lock.Close() }()
	}
	response, err := reconcile(ctx, root, request)
	_, response.Origins, _ = munki.Derive(request)
	return response, err
}

func repositoryPath(configuration json.RawMessage) (string, error) {
	var connection struct {
		Path string `json:"path"`
	}
	decoder := json.NewDecoder(bytes.NewReader(configuration))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&connection); err != nil {
		return "", err
	}
	if connection.Path == "" {
		return "", errors.New("munki repository path is required")
	}
	return connection.Path, nil
}

func nativeInput(request plugin.ReconcileRequest) (munki.Input, map[string]any, error) {
	input := munki.Input{Name: request.Identity.Software, Version: request.Artifact.Version, SHA256: request.Artifact.SHA256, Size: request.Artifact.Size, InstallerLocation: "stemma/" + request.Artifact.SHA256 + "/" + request.Artifact.Filename}
	if request.Artifact.Format == "pkg" || strings.EqualFold(filepath.Ext(request.Artifact.Filename), ".pkg") {
		input.InstallerType = "pkg"
	}
	values, _, err := munki.Derive(request)
	if err != nil {
		return input, nil, err
	}
	data, err := json.Marshal(values)
	if err != nil {
		return input, nil, err
	}
	return munki.Compose(input, data)
}

func reconcile(ctx context.Context, root string, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
	input, managed, err := nativeInput(request)
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	identity := hashValue(request.Identity)
	var binding repositoryBinding
	if len(request.Binding) > 0 && string(request.Binding) != "null" {
		if err := json.Unmarshal(request.Binding, &binding); err != nil {
			return plugin.ReconcileResponse{}, err
		}
	}
	if binding.Identity != "" && binding.Identity != identity {
		return plugin.ReconcileResponse{}, errors.New("munki binding belongs to a different destination")
	}
	if binding.Entries == nil {
		binding.Entries = map[string]publication{}
	}
	fingerprint := hashValue([]any{input.Name, input.Version, input.SHA256, managed["supported_architectures"]})
	payload := input.SHA256
	if input.InstallerType == "nopkg" {
		payload = hashValue([]any{"nopkg", input.Version})
	}
	if binding.Latest == nil {
		binding.Latest = map[string]string{}
	}
	pkginfoPath := filepath.Join("pkgsinfo", "stemma", identity, fingerprint+".plist")
	entry, known := binding.Entries[fingerprint]
	if known && (entry.Pkginfo != filepath.ToSlash(pkginfoPath) || entry.Name != input.Name || entry.Version != input.Version || entry.Location != input.InstallerLocation) {
		return plugin.ReconcileResponse{}, errors.New("munki publication binding differs from installer tuple")
	}
	old, err := readObject(filepath.Join(root, pkginfoPath))
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	if old != nil && (!known || owner(old) != identity) {
		return plugin.ReconcileResponse{}, fmt.Errorf("pkginfo %s is not owned by this destination", pkginfoPath)
	}
	desired := mergeFields(old, nil)
	_, origins, _ := munki.Derive(request)
	destinationMetadata, _ := munki.DecodeDestination(request.Metadata)
	for _, field := range entry.Derived {
		if _, exists := managed[field]; !exists && !slices.Contains(destinationMetadata.Unmanaged, "pkginfo."+field) {
			delete(desired, field)
		}
	}
	derived := []string{}
	for field, origin := range origins {
		if origin != "authored" {
			derived = append(derived, strings.TrimPrefix(field, "pkginfo."))
		}
	}
	slices.Sort(derived)
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
		return plugin.ReconcileResponse{}, err
	}
	encoded, err := munki.Build(input)
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	var generated map[string]any
	if _, err := plist.Unmarshal(encoded, &generated); err != nil {
		return plugin.ReconcileResponse{}, err
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
	metadata = mergeFields(metadata, map[string]any{"stemma": identity})
	desired["_metadata"] = metadata
	response := plugin.ReconcileResponse{}
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
		if hashValue(old[key]) != hashValue(value) {
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
	binding.Pkginfo, binding.Identity = filepath.ToSlash(pkginfoPath), identity
	binding.Entries[fingerprint] = publication{Payload: payload, Pkginfo: filepath.ToSlash(pkginfoPath), Location: input.InstallerLocation, Name: input.Name, Version: input.Version, SHA256: input.SHA256, Derived: derived}
	response.Binding = raw(binding)
	if request.Method != "apply" {
		destination, err := munki.DecodeDestination(request.Metadata)
		if err != nil {
			return response, err
		}
		if destination.Retention != nil {
			binding.Publications.Record(payload)
			binding.Latest[payload] = fingerprint
			if err := prune(ctx, root, &binding, destination.Retention.Keep, false, &response); err != nil {
				return response, err
			}
		}
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
	if hashValue(old) != hashValue(desired) {
		data, err := plist.MarshalIndent(desired, plist.XMLFormat, "  ")
		if err != nil {
			return response, err
		}
		if err := fileio.Write(filepath.Join(root, pkginfoPath), data, 0o644); err != nil {
			return response, err
		}
	}
	binding.Publications.Record(payload)
	binding.Latest[payload] = fingerprint
	response.Binding = raw(binding)
	destination, err := munki.DecodeDestination(request.Metadata)
	if err != nil {
		return response, err
	}
	if destination.Retention != nil {
		if err := prune(ctx, root, &binding, destination.Retention.Keep, true, &response); err != nil {
			return response, err
		}
	}
	response.Binding = raw(binding)
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
			if samePublication(entry, old) || samePublication(entry, desired) {
				continue
			}
			merged = append(merged, entry)
		}
		include := desired != nil && name == "all"
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
		if hashValue(existing) != hashValue(merged) {
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
