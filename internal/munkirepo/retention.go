package munkirepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/munki"
	"github.com/woodleighschool/stemma/plugin"
)

type repositoryBinding struct {
	Identity     string                 `json:"identity"`
	Pkginfo      string                 `json:"pkginfo"`
	Publications plugin.Publications    `json:"publications"`
	Entries      map[string]publication `json:"entries"`
	Latest       map[string]string      `json:"latest"`
}

type publication struct {
	Payload  string   `json:"payload"`
	Pkginfo  string   `json:"pkginfo"`
	Location string   `json:"location,omitempty"`
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	SHA256   string   `json:"sha256,omitempty"`
	Derived  []string `json:"derived,omitempty"`
}

func samePublication(a, b map[string]any) bool {
	return a != nil && b != nil && owner(a) != "" && owner(a) == owner(b) && a["name"] == b["name"] && a["version"] == b["version"] && a["installer_item_hash"] == b["installer_item_hash"] && hashValue(a["supported_architectures"]) == hashValue(b["supported_architectures"])
}

func prune(ctx context.Context, root string, binding *repositoryBinding, keep int, apply bool, response *plugin.ReconcileResponse) error {
	retained := binding.Publications.Retained(keep)
	fingerprints := make([]string, 0, len(binding.Entries))
	for fingerprint := range binding.Entries {
		fingerprints = append(fingerprints, fingerprint)
	}
	slices.Sort(fingerprints)
	for _, fingerprint := range fingerprints {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := binding.Entries[fingerprint]
		payload := entry.Payload
		if entry.Pkginfo == binding.Pkginfo || payload == "" || binding.Publications.Order[payload] == 0 || binding.Latest[payload] == "" {
			continue
		}
		if retained[payload] && binding.Latest[payload] == fingerprint {
			continue
		}
		expected := filepath.ToSlash(filepath.Join("pkgsinfo", "stemma", binding.Identity, fingerprint+".plist"))
		if entry.Pkginfo != expected {
			return errors.New("retention binding contains an invalid pkginfo path")
		}
		document, err := readObject(filepath.Join(root, filepath.FromSlash(entry.Pkginfo)))
		if err != nil {
			return err
		}
		if document == nil {
			continue
		}
		if owner(document) != binding.Identity || document["name"] != entry.Name || document["version"] != entry.Version {
			return errors.New("retention pkginfo no longer matches owned publication")
		}
		if entry.SHA256 != "" && (document["installer_item_hash"] != entry.SHA256 || document["installer_item_location"] != entry.Location) {
			return errors.New("retention installer no longer matches owned publication")
		}
		protected, err := protectedPublication(root, entry)
		if err != nil {
			return err
		}
		if protected {
			continue
		}
		if !apply {
			response.Changes = append(response.Changes, plugin.Change{Kind: "retention", Field: entry.Pkginfo, Action: "delete"})
			continue
		}
		catalogs, err := catalogChanges(root, document, nil)
		if err != nil {
			return err
		}
		for name, entries := range catalogs {
			data, err := munki.Marshal(entries)
			if err != nil {
				return err
			}
			if err := fileio.Write(filepath.Join(root, "catalogs", name), data, 0o644); err != nil {
				return err
			}
		}
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(entry.Pkginfo))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if entry.Location != "" {
			referenced, err := installerReferenced(root, entry.Location)
			if err != nil {
				return err
			}
			if !referenced {
				if !strings.HasPrefix(entry.Location, "stemma/"+entry.SHA256+"/") || !filepath.IsLocal(entry.Location) {
					return errors.New("retention binding contains an invalid installer location")
				}
				if err := os.Remove(filepath.Join(root, "pkgs", filepath.FromSlash(entry.Location))); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
		delete(binding.Entries, fingerprint)
		referenced := false
		for _, other := range binding.Entries {
			if other.Payload == payload {
				referenced = true
				break
			}
		}
		if !referenced {
			delete(binding.Publications.Order, payload)
			delete(binding.Latest, payload)
		}
		response.Binding = raw(binding)
		response.Changes = append(response.Changes, plugin.Change{Kind: "retention", Field: entry.Pkginfo, Action: "delete"})
	}
	return nil
}

func protectedPublication(root string, entry publication) (bool, error) {
	protected := false
	for _, directory := range []string{"manifests", "pkgsinfo"} {
		err := walkDocuments(filepath.Join(root, directory), func(filename string, value any) {
			if directory == "pkgsinfo" && filename == filepath.Join(root, filepath.FromSlash(entry.Pkginfo)) {
				return
			}
			if references(value, entry.Name, entry.Version) {
				protected = true
			}
		})
		if err != nil {
			return false, err
		}
	}
	return protected, nil
}

func references(value any, name, version string) bool {
	switch value := value.(type) {
	case string:
		return value == name || value == name+"-"+version || value == name+"--"+version
	case []any:
		for _, item := range value {
			if references(item, name, version) {
				return true
			}
		}
	case map[string]any:
		for key, item := range value {
			switch key {
			case "requires", "update_for", "managed_installs", "managed_uninstalls", "managed_updates", "optional_installs", "featured_items", "default_installs", "conditional_items":
				if references(item, name, version) {
					return true
				}
			}
		}
	}
	return false
}

func installerReferenced(root, location string) (bool, error) {
	referenced := false
	for _, directory := range []string{"pkgsinfo", "catalogs"} {
		if err := walkDocuments(filepath.Join(root, directory), func(_ string, value any) {
			if containsLocation(value, location) {
				referenced = true
			}
		}); err != nil {
			return false, err
		}
	}
	return referenced, nil
}

func containsLocation(value any, location string) bool {
	switch value := value.(type) {
	case map[string]any:
		if value["installer_item_location"] == location {
			return true
		}
		for _, item := range value {
			if containsLocation(item, location) {
				return true
			}
		}
	case []any:
		for _, item := range value {
			if containsLocation(item, location) {
				return true
			}
		}
	}
	return false
}

func walkDocuments(root string, visit func(string, any)) error {
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing retention with symlink %s", root)
	}
	scoped, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer func() { _ = scoped.Close() }()
	return fs.WalkDir(scoped.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing retention with symlink %s", filename)
		}
		file, err := scoped.Open(filepath.FromSlash(name))
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(file, (32<<20)+1))
		closeErr := file.Close()
		if err := errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if len(data) > 32<<20 {
			return errors.New("repository document exceeds retention read limit")
		}
		var value any
		if err := munki.Unmarshal(data, &value); err != nil {
			return err
		}
		visit(filename, value)
		return nil
	})
}
