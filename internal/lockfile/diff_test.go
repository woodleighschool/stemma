package lockfile

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/internal/source"
)

func TestDiffInputsIncludesRemovedAndMetadataOnlyEntries(t *testing.T) {
	old := source.Entry{Version: 1, Content: source.Content{Filename: "one.pkg", SHA256: "old"}, Observation: json.RawMessage(`{"version":"1"}`)}
	metadata := old
	metadata.Observation = json.RawMessage(`{"version":"1","etag":"new"}`)
	content := old
	content.Content.SHA256 = "new"
	changes := DiffInputs(map[string]map[string]source.Entry{"app": {"source": old, "icon": old}, "removed": {"source": old}}, map[string]map[string]source.Entry{"app": {"source": metadata, "icon": content}, "new": {"source": content}})
	if len(changes) != 4 {
		t.Fatalf("changes=%+v", changes)
	}
	if changes[0].Input != "icon" || !changes[0].ContentChanged || changes[1].Input != "source" || changes[1].ContentChanged {
		t.Fatalf("content vs metadata: %+v", changes)
	}
	if changes[2].Resource != "new" || changes[2].Before != nil || changes[2].After == nil || changes[3].After != nil {
		t.Fatalf("add/remove: %+v", changes)
	}
	if got := DiffInputs(map[string]map[string]source.Entry{"app": {"source": old}}, map[string]map[string]source.Entry{"app": {"source": old}}); len(got) != 0 {
		t.Fatalf("unchanged inputs: %+v", got)
	}
}
