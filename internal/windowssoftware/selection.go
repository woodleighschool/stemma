package windowssoftware

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// maxCandidates bounds the installers an ambiguity error lists; a vendor
// setup directory can hold dozens of helper executables.
const maxCandidates = 10

// sourceSetup is the entry point the source itself selects: the installer of a
// single-file source or the entry point of a resource output. An archive
// selects none.
func sourceSetup(source plugin.Artifact, archived bool) string {
	switch {
	case archived:
		return ""
	case source.Tree:
		return source.EntryPoint
	}
	return source.Filename
}

// selectSetup picks the setup entry point of the prepared setup content, as
// MacSoftware picks its installer: setup_file selects any file by path or
// glob, otherwise the source's own entry point applies, otherwise the only
// MSI or EXE in the setup directory. Selection must be unambiguous.
func selectSetup(ctx context.Context, pattern string, source plugin.Artifact, archived bool, setup plugin.Artifact) (string, error) {
	selected := sourceSetup(source, archived)
	if !setup.Tree {
		if pattern != "" && !matchPath(pattern, setup.Filename) {
			return "", fmt.Errorf("setup_file %q does not match the installer %s", pattern, setup.Filename)
		}
		return setup.Filename, nil
	}
	if pattern == "" && selected != "" {
		return selected, nil
	}
	var files, installers []string
	err := filepath.WalkDir(setup.Path, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relativeName, err := filepath.Rel(setup.Path, name)
		if err != nil {
			return err
		}
		relativeName = filepath.ToSlash(relativeName)
		files = append(files, relativeName)
		if extension := strings.ToLower(path.Ext(relativeName)); extension == ".msi" || extension == ".exe" {
			installers = append(installers, relativeName)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if pattern != "" {
		var matches []string
		for _, name := range files {
			if matchPath(pattern, name) {
				matches = append(matches, name)
			}
		}
		if len(matches) != 1 {
			if len(matches) == 0 {
				matches = installers
			}
			return "", fmt.Errorf("setup_file matched %d files; require exactly one%s", len(matches), candidates(matches))
		}
		return matches[0], nil
	}
	if len(installers) != 1 {
		return "", fmt.Errorf("found %d MSI or EXE installers; select setup_file%s", len(installers), candidates(installers))
	}
	return installers[0], nil
}

// validPattern reports whether pattern is a confined setup-relative path or
// glob.
func validPattern(pattern string) bool {
	if !fs.ValidPath(pattern) || pattern == "." || strings.ContainsAny(pattern, "\\\x00\r\n\t") {
		return false
	}
	_, err := path.Match(pattern, "")
	return !errors.Is(err, path.ErrBadPattern)
}

// matchPath accepts a glob or the exact path, whose name may itself contain
// pattern characters.
func matchPath(pattern, name string) bool {
	matched, _ := path.Match(pattern, name)
	return matched || pattern == name
}

func candidates(names []string) string {
	if len(names) == 0 {
		return ""
	}
	listed := names[:min(len(names), maxCandidates)]
	text := ": " + strings.Join(listed, ", ")
	if more := len(names) - len(listed); more > 0 {
		text += fmt.Sprintf(" and %d more", more)
	}
	return text
}
