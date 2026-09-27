// Package munkirepo reconciles native pkginfo, installers and shared Munki catalogs.
package munkirepo

import (
	"cmp"
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
func Handle(ctx context.Context, request plugin.ReconcileRequest[Config]) (plugin.ReconcileResponse, error) {
	var inputErr error
	request, inputErr = munki.DocumentInput(ctx, request)
	if inputErr != nil {
		return plugin.ReconcileResponse{}, inputErr
	}
	request, inputErr = munki.ResolveReferences(request)
	if inputErr != nil {
		return plugin.ReconcileResponse{}, inputErr
	}
	if request.Method == "validate" && !request.Prepared && request.Artifact.Path == "" {
		_, err := munki.DecodeDestination(request.Metadata)
		return plugin.ReconcileResponse{}, err
	}
	if request.Method == "validate" {
		input, _, _, err := nativeInput(request)
		if err == nil {
			_, err = munki.Build(input)
		}
		return plugin.ReconcileResponse{}, err
	}
	if request.Method != "plan" && request.Method != "apply" {
		return plugin.ReconcileResponse{}, fmt.Errorf("unsupported Munki method %q", request.Method)
	}
	if err := request.Config.Validate(); err != nil {
		return plugin.ReconcileResponse{}, err
	}
	root := request.Config.Path
	if !filepath.IsAbs(root) && request.Root != "" {
		root = filepath.Join(request.Root, root)
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
	return reconcile(ctx, root, request)
}

// Config selects a local Munki repository.
type Config struct {
	Path string `json:"path" jsonschema:"minLength=1" jsonschema_description:"Repository directory, relative to the project root or absolute. Apply serializes writers and updates native catalogs."`
}

// Validate requires a repository location.
func (config Config) Validate() error {
	if config.Path == "" {
		return errors.New("munki repository path is required")
	}
	return nil
}

func nativeInput(request plugin.ReconcileRequest[Config]) (munki.Input, map[string]any, munki.Derived, error) {
	input := munki.Input{Name: request.Identity.Resource.Name, Version: request.Artifact.Version, SHA256: request.Artifact.SHA256, Size: request.Artifact.Size, InstallerLocation: request.Artifact.Filename}
	if request.Artifact.Format == "pkg" || strings.EqualFold(filepath.Ext(request.Artifact.Filename), ".pkg") {
		input.InstallerType = "pkg"
	}
	derived, err := munki.Derive(request)
	if err != nil {
		return input, nil, derived, err
	}
	data, err := json.Marshal(derived.Values)
	if err != nil {
		return input, nil, derived, err
	}
	input, managed, err := munki.Compose(input, data)
	return input, managed, derived, err
}

// item is one pkginfo document, located relative to the repository root.
type item struct {
	path     string
	document map[string]any
}

// target is the repository's own identity for a publication: Munki name and
// version, and the architectures when the declaration sets that variant.
type target struct {
	name, version string
	architectures []string
	scoped        bool
}

func (t target) family(document map[string]any) bool {
	return document["name"] == t.name && (!t.scoped || slices.Equal(sortedStrings(document["supported_architectures"]), t.architectures))
}

func sortedStrings(value any) []string {
	var result []string
	switch values := value.(type) {
	case []any:
		for _, value := range values {
			if name, ok := value.(string); ok {
				result = append(result, name)
			}
		}
	case []string:
		result = slices.Clone(values)
	}
	slices.Sort(result)
	return result
}

func pkgsinfo(root string) ([]item, error) {
	var items []item
	err := walkDocuments(filepath.Join(root, "pkgsinfo"), func(filename string, value any) {
		if document, ok := value.(map[string]any); ok {
			relative, _ := filepath.Rel(root, filename)
			items = append(items, item{filepath.ToSlash(relative), document})
		}
	})
	slices.SortFunc(items, func(a, b item) int { return strings.Compare(a.path, b.path) })
	return items, err
}

// newer orders pkginfo by descending Munki version, then by path.
func newer(a, b item) int {
	left, _ := a.document["version"].(string)
	right, _ := b.document["version"].(string)
	return cmp.Or(munki.CompareVersions(right, left), strings.Compare(a.path, b.path))
}

// New files are named as munkiimport names them, numbered when the name is taken.
func freeName(directory, base, extension string, reusable func(string) (bool, error)) (string, error) {
	if base == "" || base+extension != filepath.Base(base+extension) || !filepath.IsLocal(base+extension) {
		return "", fmt.Errorf("unsafe repository filename %q", base+extension)
	}
	for index := 0; ; index++ {
		name := base + extension
		if index > 0 {
			name = fmt.Sprintf("%s__%d%s", base, index, extension)
		}
		if _, err := os.Lstat(filepath.Join(directory, name)); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
		if reuse, err := reusable(name); err != nil || reuse {
			return name, err
		}
	}
}

func reconcile(ctx context.Context, root string, request plugin.ReconcileRequest[Config]) (plugin.ReconcileResponse, error) {
	input, managed, derived, err := nativeInput(request)
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	destination, err := munki.DecodeDestination(request.Metadata)
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	items, err := pkgsinfo(root)
	if err != nil {
		return plugin.ReconcileResponse{}, err
	}
	want := target{name: input.Name, version: input.Version}
	if value, declared := managed["supported_architectures"]; declared {
		want.scoped, want.architectures = true, sortedStrings(value)
	}
	var current *item
	var others []item
	for i, candidate := range items {
		switch {
		case !want.family(candidate.document):
		case candidate.document["version"] != want.version:
			others = append(others, candidate)
		case current != nil:
			return plugin.ReconcileResponse{}, fmt.Errorf("pkginfo %s and %s both publish %s %s; set supported_architectures or remove one", current.path, candidate.path, want.name, want.version)
		default:
			current = &items[i]
		}
	}
	slices.SortFunc(others, newer)
	var old map[string]any
	var pkginfoPath string
	if current != nil {
		old, pkginfoPath = current.document, current.path
	} else {
		base := want.name + "-" + want.version
		if len(want.architectures) == 1 {
			base += "-" + want.architectures[0]
		}
		name, err := freeName(filepath.Join(root, "pkgsinfo"), base, ".plist", func(string) (bool, error) { return false, nil })
		if err != nil {
			return plugin.ReconcileResponse{}, err
		}
		pkginfoPath = "pkgsinfo/" + name
	}
	desired := mergeFields(old, nil)
	origins := derived.Origins
	// Artwork belongs to the software, so a new installer can retain the newest reference.
	previous := old
	if previous == nil && len(others) > 0 {
		previous = others[0].document
	}
	_, managedName := managed["icon_name"]
	_, managedHash := managed["icon_hash"]
	// A declared icon publishes its exact bytes; without one the previous
	// reference stays.
	if origins["pkginfo.icon_name"] != "input.icon" && !managedName && !managedHash {
		name, _ := previous["icon_name"].(string)
		if name != "" && filepath.IsLocal(name) {
			if info, err := os.Stat(filepath.Join(root, "icons", filepath.FromSlash(name))); err == nil && info.Mode().IsRegular() {
				managed["icon_name"], managed["icon_hash"] = name, previous["icon_hash"]
				origins["pkginfo.icon_name"], origins["pkginfo.icon_hash"] = "retained", "retained"
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return plugin.ReconcileResponse{}, err
			}
		}
	}
	for _, field := range derived.Cleared {
		delete(desired, field)
	}
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
	} else if location, _ := old["installer_item_location"].(string); location != "" {
		// An adopted item keeps the place its repository gave the installer.
		input.InstallerLocation = location
	} else {
		extension := filepath.Ext(request.Artifact.Filename)
		input.InstallerLocation, err = freeName(filepath.Join(root, "pkgs"), strings.TrimSuffix(request.Artifact.Filename, extension), extension, func(name string) (bool, error) {
			return fileMatches(ctx, filepath.Join(root, "pkgs", name), request.Artifact)
		})
		if err != nil {
			return plugin.ReconcileResponse{}, err
		}
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
	removalManaged := old == nil
	for _, key := range []string{"items_to_copy", "uninstall_method"} {
		_, declared := managed[key]
		removalManaged = removalManaged || declared || slices.Contains(derived.Cleared, key)
	}
	if removalManaged {
		if value, exists := generated["items_to_remove"]; exists {
			desired["items_to_remove"] = value
		} else {
			delete(desired, "items_to_remove")
		}
	}
	if _, managedCatalogs := managed["catalogs"]; !managedCatalogs && old == nil {
		desired["catalogs"] = []string{"testing"}
	}
	response := plugin.ReconcileResponse{Origins: origins}
	contentPath := ""
	if input.InstallerType != "nopkg" {
		contentPath = filepath.Join(root, "pkgs", filepath.FromSlash(input.InstallerLocation))
		matches, err := fileMatches(ctx, contentPath, request.Artifact)
		if err != nil {
			return response, err
		}
		if !matches {
			// Replacing bytes in place must not break another item that installs them.
			for _, other := range items {
				if other.path != pkginfoPath && other.document["installer_item_location"] == input.InstallerLocation {
					return response, fmt.Errorf("installer %s is shared with %s, so its bytes cannot be replaced", input.InstallerLocation, other.path)
				}
			}
			response.Changes = append(response.Changes, plugin.Change{Kind: "content", Field: "installer_item_hash", Action: "upload", Before: raw(old["installer_item_hash"]), After: raw(request.Artifact.SHA256)})
		}
	}
	iconPath := ""
	if origins["pkginfo.icon_name"] == "input.icon" {
		name, _ := managed["icon_name"].(string)
		iconPath = filepath.Join(root, "icons", filepath.FromSlash(name))
		matches, err := fileMatches(ctx, iconPath, request.Inputs["icon"])
		if err != nil {
			return response, err
		}
		if !matches {
			response.Changes = append(response.Changes, plugin.Change{Kind: "content", Field: "icon_hash", Action: "upload", After: raw(request.Inputs["icon"].SHA256)})
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
	if request.Method != "apply" {
		if destination.Retention != nil {
			err = prune(ctx, root, desired, others, destination.Retention.Keep, false, &response)
		}
		return response, err
	}
	// No catalog references an installer before its complete bytes are available.
	if contentPath != "" {
		if err := publishContent(ctx, contentPath, request.Artifact); err != nil {
			return response, err
		}
	}
	if iconPath != "" {
		if err := publishContent(ctx, iconPath, request.Inputs["icon"]); err != nil {
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
		if err := fileio.Write(filepath.Join(root, filepath.FromSlash(pkginfoPath)), data, 0o644); err != nil {
			return response, err
		}
	}
	if destination.Retention != nil {
		err = prune(ctx, root, desired, others, destination.Retention.Keep, true, &response)
	}
	return response, err
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
			if sameItem(entry, old) || sameItem(entry, desired) {
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
			merged = append(merged, catalogEntry(desired))
		}
		if merged == nil {
			merged = []map[string]any{}
		}
		// Variants share a name and version, so their architectures settle the order.
		key := func(entry map[string]any) string {
			return fmt.Sprint(entry["name"]) + "/" + fmt.Sprint(entry["version"]) + "/" + strings.Join(sortedStrings(entry["supported_architectures"]), ",")
		}
		sort.SliceStable(merged, func(i, j int) bool { return key(merged[i]) < key(merged[j]) })
		if hashValue(existing) != hashValue(merged) {
			changes[name] = merged
		}
	}
	return changes, nil
}

// sameItem compares the identity Munki itself gives an item.
func sameItem(a, b map[string]any) bool {
	return a != nil && b != nil && a["name"] == b["name"] && a["version"] == b["version"] && slices.Equal(sortedStrings(a["supported_architectures"]), sortedStrings(b["supported_architectures"]))
}

// catalogEntry omits what makecatalogs omits: administrator notes and private keys.
func catalogEntry(pkginfo map[string]any) map[string]any {
	entry := make(map[string]any, len(pkginfo))
	for key, value := range pkginfo {
		if key != "notes" && !strings.HasPrefix(key, "_") {
			entry[key] = value
		}
	}
	return entry
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
func publishContent(ctx context.Context, path string, artifact plugin.Artifact) (err error) {
	done := plugin.Stage(ctx, "Publishing Munki installer", plugin.Detail(filepath.Base(path)))
	defer func() { done(err) }()
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
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(plugin.ProgressReader(ctx, fileio.Reader{Context: ctx, Reader: source}, artifact.Size), artifact.Size+1))
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
