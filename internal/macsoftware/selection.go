package macsoftware

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// choose picks what a source publishes from its inventory: an application, or
// a container by its contents path. A container is a package ("." for a PKG
// source), or a disk image to open and choose within. A selector that matches
// nothing here applies inside the source's only container: package_path inside
// its only disk image, an application selector inside its only package or image.
func choose(spec Spec, traversable bool, inventory plugin.Facts) (string, *plugin.Subject, error) {
	if !traversable {
		for _, subject := range inventory.Subjects {
			if subject.Package != nil {
				return ".", nil, nil
			}
		}
		return "", nil, errors.New("MacSoftware requires an application or installer; use BuildMacPkg to declare a command or file layout")
	}
	var apps, containers []plugin.Subject
	for _, subject := range inventory.Subjects {
		switch {
		case subject.App != nil:
			apps = append(apps, subject)
		case subject.Kind == "container" && subject.ID != ".":
			containers = append(containers, subject)
		}
	}
	if spec.PackagePath != "" {
		var matches, images []plugin.Subject
		for _, subject := range containers {
			if matchPath(spec.PackagePath, subject.Path) {
				matches = append(matches, subject)
			}
			if diskImage(subject.Path) {
				images = append(images, subject)
			}
		}
		switch {
		case len(matches) == 1:
			return matches[0].Path, nil, nil
		case len(matches) == 0 && len(images) == 1:
			return images[0].Path, nil, nil
		}
		return "", nil, fmt.Errorf("package_path matched %d packages and disk images; require exactly one%s", len(matches), candidates(containers))
	}
	if options := spec.Application; options != nil && (options.Path != "" || options.BundleID != "") {
		matches := matchApps(apps, options)
		switch {
		case len(matches) == 1:
			return "", &matches[0], nil
		case len(matches) == 0 && len(containers) == 1:
			return containers[0].Path, nil, nil
		}
		return "", nil, fmt.Errorf("application selector matched %d applications; require exactly one%s", len(matches), candidates(apps))
	}
	found := slices.Concat(apps, containers)
	if len(found) == 0 {
		return "", nil, errors.New("source contains no application or installer; use BuildMacPkg to declare a command or file layout")
	}
	if len(found) != 1 {
		return "", nil, fmt.Errorf("found %d applications, packages and disk images; select application.path, application.bundle_id or package_path%s", len(found), candidates(found))
	}
	if found[0].App != nil {
		return "", &found[0], nil
	}
	return found[0].Path, nil, nil
}

// diskImage reports whether a contents path names a disk image, which
// preparation opens rather than publishes.
func diskImage(name string) bool {
	return strings.EqualFold(path.Ext(name), ".dmg")
}

// selectApp selects package application evidence only when explicitly requested.
func selectApp(facts plugin.Facts, options *Application) (*plugin.Subject, error) {
	if options == nil {
		return nil, nil
	}
	if options.Path == "" && options.BundleID == "" {
		return nil, errors.New("package application options require application.path or application.bundle_id")
	}
	var apps []plugin.Subject
	for _, subject := range facts.Subjects {
		if subject.App != nil {
			apps = append(apps, subject)
		}
	}
	matches := matchApps(apps, options)
	if len(matches) != 1 {
		return nil, fmt.Errorf("application selector matched %d applications; require exactly one%s", len(matches), candidates(apps))
	}
	return &matches[0], nil
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
