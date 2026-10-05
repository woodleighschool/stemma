package reconcile

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
)

// change is one resource whose lock differs from its current inputs.
type change struct {
	kind, name string
	entries    map[string]source.Entry
	removed    bool
	// refresh marks a change that resolved the same bytes with new metadata.
	refresh bool
}

// diff finds resources whose resolved inputs differ from the reviewed lock and
// locked resources the catalog no longer declares. A resource the resolution
// skipped keeps whatever the lock holds for it.
func diff(candidate engine.Candidate) map[string]change {
	changes := map[string]change{}
	for key, resource := range candidate.Resources {
		if resource.Skipped || resource.Error != "" || equalEntries(candidate.Lock.Inputs[key], resource.Inputs) {
			continue
		}
		changes[key] = change{kind: resource.Kind, name: resource.Name, entries: resource.Inputs, refresh: sameArtifacts(candidate.Lock.Inputs[key], resource.Inputs)}
	}
	for key := range candidate.Lock.Inputs {
		if _, declared := candidate.Resources[key]; !declared {
			kind, name := splitKey(key)
			changes[key] = change{kind: kind, name: name, removed: true}
		}
	}
	return changes
}

// sameArtifacts reports whether every input resolved to the bytes already
// locked, so only observations, filenames or declarations changed.
func sameArtifacts(before, after map[string]source.Entry) bool {
	if len(before) != len(after) {
		return false
	}
	for name, entry := range after {
		previous, ok := before[name]
		if !ok || previous.Content.SHA256 != entry.Content.SHA256 {
			return false
		}
	}
	return true
}

func equalEntries(before, after map[string]source.Entry) bool {
	if len(before) == 0 && len(after) == 0 {
		return true
	}
	left, _ := json.Marshal(before)
	right, _ := json.Marshal(after)
	return bytes.Equal(left, right)
}

func splitKey(key string) (kind, name string) {
	parts := strings.Split(key, "/")
	if len(parts) < 2 {
		return key, key
	}
	return parts[len(parts)-2], parts[len(parts)-1]
}

var (
	unsafeRef = regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	dotRuns   = regexp.MustCompile(`\.{2,}`)
)

// branchName derives a valid branch name from a resource identity.
func branchName(kind, name string) string {
	for strings.HasSuffix(name, ".lock") {
		name = strings.TrimSuffix(name, ".lock")
	}
	name = strings.Trim(dotRuns.ReplaceAllString(unsafeRef.ReplaceAllString(name, "-"), "."), "-.")
	if name == "" {
		name = "resource"
	}
	return prefix + kind + "/" + name
}

// proposes reports whether a published proposal already records these
// entries for the resource; an absent lockfile proposes nothing.
func proposes(published []byte, key string, entries map[string]source.Entry) bool {
	if len(published) == 0 {
		return len(entries) == 0
	}
	file, err := lockfile.Parse(published)
	if err != nil {
		return false
	}
	return equalEntries(file.Inputs[key], entries)
}
