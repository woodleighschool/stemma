package changes

import (
	"fmt"
	"maps"
	"slices"

	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/source"
)

// InputLines describes the committed input difference using resolver-owned values.
func InputLines(change lockfile.InputChange) []string {
	label := Text(change.Input)
	switch {
	case change.Before == nil:
		label += " (added)"
	case change.After == nil:
		label += " (removed)"
	case change.ContentChanged:
		label += " (content changed)"
	default:
		label += " (metadata refreshed; content unchanged)"
	}
	if change.Before != nil && change.After != nil && change.Before.Content.Filename == change.After.Content.Filename {
		label += ": " + Text(change.After.Content.Filename)
	}
	lines := []string{label}
	comparing := change.Before != nil && change.After != nil
	before, after := inputValue(change.Before, comparing), inputValue(change.After, comparing)
	var details []string
	if change.Before == nil || change.After == nil {
		fields := after
		if change.After == nil {
			fields = before
		}
		for _, field := range slices.Sorted(maps.Keys(fields)) {
			details = append(details, initial(Text(field), fields[field])...)
		}
	} else {
		details = difference("input", before, after)
		if change.Before.Declaration != change.After.Declaration {
			details = append(details, "source declaration changed")
		}
	}
	for _, line := range details {
		lines = append(lines, "  "+line)
	}
	return lines
}

func inputValue(entry *source.Entry, comparing bool) map[string]any {
	if entry == nil {
		return map[string]any{}
	}
	fields := map[string]any{
		"filename":   entry.Content.Filename,
		"sha256":     entry.Content.Artifact.SHA256,
		"size_bytes": entry.Content.Artifact.Size,
	}
	if entry.Content.Mode != 0 && entry.Content.Mode != 0o644 || comparing {
		fields["mode"] = fmt.Sprintf("%04o", entry.Content.Mode)
	}
	if entry.Content.Tree || comparing {
		fields["tree"] = entry.Content.Tree
	}
	if comparing {
		fields["resolver"] = entry.Resolver
		fields["resolver_version"] = entry.ResolverVersion
	}
	if len(entry.Observation) > 0 {
		if observation := decode(entry.Observation); observation != nil {
			if object, ok := observation.(map[string]any); !ok || len(object) > 0 {
				fields["observation"] = observation
			}
		}
	}
	if len(entry.Evidence) > 0 {
		evidence := map[string]any{}
		for name, data := range entry.Evidence {
			evidence[name] = decode(data)
		}
		fields["evidence"] = evidence
	}
	return fields
}
