package macsoftware

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// choose picks what a source publishes from its inventory: a package, by its
// contents path ("." for a PKG source), or an application for a disk image.
// An application selector that matches nothing beside a single package selects
// within that package instead.
func choose(spec Spec, traversable bool, inventory plugin.Facts) (string, *plugin.Subject, error) {
	if !traversable {
		for _, subject := range inventory.Subjects {
			if subject.Package != nil {
				return ".", nil, nil
			}
		}
		return "", nil, errors.New("macOS software requires an application, PKG or DMG installer")
	}
	var apps, packages []plugin.Subject
	for _, subject := range inventory.Subjects {
		switch {
		case subject.App != nil:
			apps = append(apps, subject)
		case subject.Kind == "container" && subject.ID != ".":
			packages = append(packages, subject)
		}
	}
	if spec.PackagePath != "" {
		var matches []plugin.Subject
		for _, subject := range packages {
			if matchPath(spec.PackagePath, subject.Path) {
				matches = append(matches, subject)
			}
		}
		if len(matches) != 1 {
			return "", nil, fmt.Errorf("package_path matched %d packages; require exactly one%s", len(matches), candidates(packages))
		}
		return matches[0].Path, nil, nil
	}
	if options := spec.Application; options != nil && (options.Path != "" || options.BundleID != "") {
		matches := matchApps(apps, options)
		switch {
		case len(matches) == 1:
			return "", &matches[0], nil
		case len(matches) == 0 && len(packages) == 1:
			return packages[0].Path, nil, nil
		}
		return "", nil, fmt.Errorf("application selector matched %d applications; require exactly one%s", len(matches), candidates(apps))
	}
	found := slices.Concat(apps, packages)
	if len(found) != 1 {
		return "", nil, fmt.Errorf("found %d applications and packages; select application.path, application.bundle_id or package_path%s", len(found), candidates(found))
	}
	if found[0].App != nil {
		return "", &found[0], nil
	}
	return found[0].Path, nil, nil
}

// selectApp finds the application a selector names in a package inventory, or
// infers its only application outside another application. Without application
// options, packages with no unique application use installer evidence alone.
func selectApp(facts plugin.Facts, options *Application) (*plugin.Subject, error) {
	var apps []plugin.Subject
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			apps = append(apps, subject)
		}
	}
	if options != nil && (options.Path != "" || options.BundleID != "") {
		matches := matchApps(apps, options)
		if len(matches) != 1 {
			return nil, fmt.Errorf("application selector matched %d applications; require exactly one%s", len(matches), candidates(apps))
		}
		return &matches[0], nil
	}
	top := topLevel(facts)
	switch len(top) {
	case 0:
		if options != nil {
			return nil, errors.New("application options require an application")
		}
		return nil, nil
	case 1:
		return &top[0], nil
	}
	if options == nil {
		return nil, nil
	}
	return nil, fmt.Errorf("multiple applications observed; select application.path or application.bundle_id%s", candidates(top))
}

// topLevel lists the applications that are not inside another application.
func topLevel(facts plugin.Facts) []plugin.Subject {
	apps := map[string]bool{}
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			apps[subject.ID] = true
		}
	}
	var top []plugin.Subject
	for _, subject := range facts.Subjects {
		if subject.App != nil && !apps[subject.Parent] {
			top = append(top, subject)
		}
	}
	return top
}

func matchApps(apps []plugin.Subject, options *Application) []plugin.Subject {
	var matches []plugin.Subject
	for _, app := range apps {
		if options.Path != "" && !matchPath(options.Path, app.Path) || options.BundleID != "" && options.BundleID != app.App.BundleID {
			continue
		}
		matches = append(matches, app)
	}
	return matches
}

// matchPath accepts a glob or the exact path, whose name may itself contain
// pattern characters.
func matchPath(pattern, name string) bool {
	matched, _ := path.Match(pattern, name)
	return matched || pattern == name
}

func candidates(subjects []plugin.Subject) string {
	if len(subjects) == 0 {
		return ""
	}
	paths := make([]string, len(subjects))
	for i, subject := range subjects {
		paths[i] = subject.Path
	}
	return ": " + strings.Join(paths, ", ")
}
