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

// prune keeps the newest others beside the current publication and removes the
// rest of the family, newest first, unless the repository still pins them.
func prune(ctx context.Context, root string, current map[string]any, others []item, keep int, apply bool, response *plugin.ReconcileResponse) error {
	for _, old := range others[min(keep-1, len(others)):] {
		if err := ctx.Err(); err != nil {
			return err
		}
		protected, err := protectedPublication(root, old, current)
		if err != nil {
			return err
		}
		if protected {
			continue
		}
		response.Changes = append(response.Changes, plugin.Change{Kind: "retention", Field: old.path, Action: "delete"})
		if !apply {
			continue
		}
		catalogs, err := catalogChanges(root, old.document, nil)
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
		if err := os.Remove(filepath.Join(root, filepath.FromSlash(old.path))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		location, _ := old.document["installer_item_location"].(string)
		if location == "" {
			continue
		}
		if !filepath.IsLocal(filepath.FromSlash(location)) {
			return fmt.Errorf("pkginfo %s has an unsafe installer location", old.path)
		}
		referenced, err := installerReferenced(root, location)
		if err != nil {
			return err
		}
		if !referenced {
			if err := os.Remove(filepath.Join(root, "pkgs", filepath.FromSlash(location))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func protectedPublication(root string, old item, current map[string]any) (bool, error) {
	name, _ := old.document["name"].(string)
	version, _ := old.document["version"].(string)
	pins := []string{name + "-" + version, name + "--" + version}
	if !sameAvailability(old.document, current) {
		pins = append(pins, name)
	}
	protected := false
	for _, directory := range []string{"manifests", "pkgsinfo"} {
		err := walkDocuments(filepath.Join(root, directory), func(filename string, value any) {
			if directory == "pkgsinfo" && filename == filepath.Join(root, filepath.FromSlash(old.path)) {
				return
			}
			if references(value, pins) {
				protected = true
			}
		})
		if err != nil {
			return false, err
		}
	}
	return protected, nil
}

// A bare name can only replace an older item when its catalogs and installation
// constraints still make it available to the same clients.
func sameAvailability(a, b map[string]any) bool {
	for _, key := range []string{"catalogs", "supported_architectures"} {
		if !slices.Equal(sortedStrings(a[key]), sortedStrings(b[key])) {
			return false
		}
	}
	for _, key := range []string{"minimum_os_version", "maximum_os_version", "minimum_munki_version", "installable_condition"} {
		if hashValue(a[key]) != hashValue(b[key]) {
			return false
		}
	}
	return true
}

func references(value any, pins []string) bool {
	switch value := value.(type) {
	case string:
		return slices.Contains(pins, value)
	case []any:
		for _, item := range value {
			if references(item, pins) {
				return true
			}
		}
	case map[string]any:
		for key, item := range value {
			switch key {
			case "requires", "update_for", "managed_installs", "managed_uninstalls", "managed_updates", "optional_installs", "featured_items", "default_installs", "conditional_items":
				if references(item, pins) {
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
		return fmt.Errorf("refusing to read symlink %s", root)
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
		// Munki ignores dotfiles, which desktop tools scatter through a repository.
		if name != "." && strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to read symlink %s", filename)
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
			return fmt.Errorf("%s exceeds the repository read limit", filename)
		}
		var value any
		if err := munki.Unmarshal(data, &value); err != nil {
			return fmt.Errorf("%s: %w", filename, err)
		}
		visit(filename, value)
		return nil
	})
}
