package reconcile

import (
	"bytes"
	"encoding/json"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
)

// refreshBranch proposes the lock refresh. Resource branches carry a kind and
// a name, so none shares its name.
const refreshBranch = prefix + "refresh-locks"

// change is one resource whose lock differs from its current inputs.
type change struct {
	kind, name string
	// before holds the reviewed entries and after the current ones.
	before, after map[string]source.Entry
	// removed marks a locked resource the catalog no longer declares.
	removed bool
}

// refresh reports whether the change leaves content as reviewed: every input
// resolved to the bytes already locked, or the resource is no longer declared.
func (c change) refresh() bool {
	return c.removed || sameContent(c.before, c.after)
}

// changeset is what one managed branch proposes: lock changes by resource key.
type changeset struct {
	branch string
	// key is the resource an update proposes; the lock refresh has none.
	key     string
	changes map[string]change
}

func (s changeset) refresh() bool { return s.key == "" }

// name identifies the set in reports.
func (s changeset) name() string {
	if s.refresh() {
		return refreshName
	}
	c := s.changes[s.key]
	return c.kind + "/" + c.name
}

// counts splits the set into resources whose entries change and resources
// whose entries go.
func (s changeset) counts() (kept, removed int) {
	for _, c := range s.changes {
		if c.removed {
			removed++
		} else {
			kept++
		}
	}
	return kept, removed
}

// changesets splits the lock differences into branches. A resource whose
// content changes is reviewed on its own; every difference that leaves content
// as reviewed shares the lock refresh, which comes last.
func changesets(candidate engine.Candidate) []changeset {
	changes := diff(candidate)
	refresh := changeset{branch: refreshBranch, changes: map[string]change{}}
	var sets []changeset
	for _, key := range slices.Sorted(maps.Keys(changes)) {
		c := changes[key]
		if c.refresh() {
			refresh.changes[key] = c
			continue
		}
		sets = append(sets, changeset{branch: branchName(c.kind, c.name), key: key, changes: map[string]change{key: c}})
	}
	if len(refresh.changes) > 0 {
		sets = append(sets, refresh)
	}
	return sets
}

// diff finds resources whose resolved inputs differ from the reviewed lock and
// locked resources the catalog no longer declares. A resource the resolution
// skipped keeps whatever the lock holds for it.
func diff(candidate engine.Candidate) map[string]change {
	changes := map[string]change{}
	for key, resource := range candidate.Resources {
		before := candidate.Lock.Inputs[key]
		if resource.Skipped || resource.Error != "" || equalEntries(before, resource.Inputs) {
			continue
		}
		changes[key] = change{kind: resource.Kind, name: resource.Name, before: before, after: resource.Inputs}
	}
	for key, before := range candidate.Lock.Inputs {
		if _, declared := candidate.Resources[key]; !declared {
			kind, name := splitKey(key)
			changes[key] = change{kind: kind, name: name, before: before, removed: true}
		}
	}
	return changes
}

// sameContent reports whether every input resolved to the bytes already
// locked, so only observations, filenames or declarations changed.
func sameContent(before, after map[string]source.Entry) bool {
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

// proposes reports whether a published lockfile already records every change
// of the set; an absent lockfile records no entries.
func proposes(published []byte, set changeset) bool {
	var file lockfile.File
	if len(published) > 0 {
		var err error
		if file, err = lockfile.Parse(published); err != nil {
			return false
		}
	}
	for key, c := range set.changes {
		if !equalEntries(file.Inputs[key], c.after) {
			return false
		}
	}
	return true
}
