// Package artifactname names published installers independently of their input paths.
package artifactname

import (
	"regexp"
	"strings"
)

var separators = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Filename combines a resource name and selected version with the final format.
// Unversioned content uses its digest prefix. Components are bounded and made
// portable without changing the version used for installation or detection.
func Filename(name, version, digest, extension string) string {
	name = separators.ReplaceAllString(name, "-")
	version = strings.Trim(separators.ReplaceAllString(version, "-"), ".-_")
	if version == "" {
		version = digest[:min(len(digest), 12)]
	}
	return name[:min(len(name), 128)] + "-" + version[:min(len(version), 80)] + "." + extension
}
