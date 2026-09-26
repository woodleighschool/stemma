package engine

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

var macOSVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*$`)

// minimumOS returns the declared minimum_os, or else the latest macOS
// requirement of the installer and the primary application. It runs after
// preparation, so a changed declaration reuses prepared outputs.
func minimumOS(artifact plugin.Artifact, declared string) (*plugin.MinimumOS, error) {
	if declared != "" {
		return &plugin.MinimumOS{Version: declared, Origin: "software.minimum_os"}, nil
	}
	var candidates []plugin.MinimumOS
	for _, subject := range artifact.Facts.Subjects {
		if subject.ID == "." && subject.Installer != nil && subject.Installer.MinimumOS != "" {
			candidates = append(candidates, plugin.MinimumOS{Version: subject.Installer.MinimumOS, Origin: "installer.minimum_os"})
		}
	}
	if data, ok := artifact.Evidence["macos.application"]; ok {
		var app plugin.Subject
		if err := json.Unmarshal(data, &app); err != nil || app.App == nil {
			return nil, errors.New("macos.application evidence requires an application subject")
		}
		if app.App.MinimumOS != "" {
			candidates = append(candidates, plugin.MinimumOS{Version: app.App.MinimumOS, Origin: "app.minimum_os"})
		}
	}
	var result *plugin.MinimumOS
	for _, candidate := range candidates {
		if !macOSVersion.MatchString(candidate.Version) {
			return nil, fmt.Errorf("%s %q is not a macOS version; set minimum_os", candidate.Origin, candidate.Version)
		}
		if result == nil || compareVersions(candidate.Version, result.Version) > 0 {
			result = &candidate
		}
	}
	return result, nil
}

// compareVersions orders dotted numeric versions; missing components compare
// as zero, so 14 and 14.0 are equal.
func compareVersions(a, b string) int {
	left, right := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(left), len(right)) {
		var x, y uint64
		if i < len(left) {
			x, _ = strconv.ParseUint(left[i], 10, 64)
		}
		if i < len(right) {
			y, _ = strconv.ParseUint(right[i], 10, 64)
		}
		if order := cmp.Compare(x, y); order != 0 {
			return order
		}
	}
	return 0
}
