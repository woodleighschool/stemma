package changes

import (
	"slices"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/plugin"
)

// PublicationRow renders the same change for plan and apply. The command's
// summary distinguishes proposed changes from completed ones.
func PublicationRow(p engine.Publication) Row {
	if p.Status == engine.StatusBlocked {
		return BlockedBy(p.Destination, p.BlockedBy)
	}
	row := Row{Subject: p.Destination, Summary: PublicationHeadline(p)}
	if p.Error != "" {
		row = Failed(p.Destination, p.Error)
	}
	row.Details = append(row.Details, publicationFields(p)...)
	for _, u := range p.Uploads {
		label := "upload " + Text(u.Name)
		upload := Line{{Label, label}}
		if u.Filename != "" && (p.Error != "" || u.Filename != p.Version) {
			upload = Line{{Label, label + ": "}, {Plain, Text(u.Filename)}}
		}
		row.Details = append(row.Details, upload)
		if u.Reason != "" {
			row.Details = append(row.Details, Line{{Old, "  " + Text(u.Reason)}})
		}
		digest := Line{{Label, "  digest: "}}
		if u.Before != "" && u.Before != u.After {
			before, after := distinct(u.Before, u.After)
			digest = append(digest, Span{Old, before + " →"}, Span{Plain, " "}, Span{New, after})
		} else {
			digest = append(digest, Span{New, value(u.After)})
		}
		row.Details = append(row.Details, digest)
	}
	for _, repair := range p.Repairs {
		row.Details = append(row.Details, Line{{New, "rebuild " + Text(repair)}})
	}
	if len(p.Removed) > 0 {
		row.Details = append(row.Details, Line{{Removed, PruneDetail(p.Removed)}})
	}
	return row
}

func publicationFields(p engine.Publication) []Line {
	if len(p.Fields) == 0 {
		return nil
	}
	// A destination can confirm fields in a different order than it plans them.
	fields := slices.Clone(p.Fields)
	slices.SortStableFunc(fields, func(a, b plugin.FieldChange) int { return strings.Compare(a.Field, b.Field) })
	metadata, _ := decode(p.Metadata).(map[string]any)
	var lines []Line
	var previous fieldMember
	var heading int
	for _, field := range fields {
		// A filename used as publication identity is already in the headline.
		if p.Error == "" && p.Version != "" && field.Field == "filename" && decode(field.After) == p.Version {
			if len(field.Before) == 0 && p.Previous == "" || decode(field.Before) == p.Previous {
				continue
			}
		}
		member, leaf := namedField(metadata, field.Field)
		indent := ""
		if member.collection != "" {
			indent, field.Field = "    ", leaf
			if member.collection != previous.collection {
				lines = append(lines, Line{{Label, Text(member.collection) + ":"}})
			}
			if member != previous {
				heading = len(lines)
				lines = append(lines, Line{{Plain, "  " + Text(member.name) + ":"}})
			}
			if leaf == "name" && len(field.Before) == 0 && decode(field.After) == member.name {
				lines[heading] = Line{{Added, "  + " + Text(member.name) + ":"}}
				previous = member
				continue
			}
		}
		previous = member
		for _, line := range fieldChange(field) {
			lines = append(lines, styled(indent+line))
		}
	}
	return lines
}

type fieldMember struct {
	collection, name string
	index            int
}

// namedField resolves a field such as policies.0.package against its declaration.
func namedField(metadata map[string]any, path string) (fieldMember, string) {
	collection, rest, _ := strings.Cut(path, ".")
	index, leaf, nested := strings.Cut(rest, ".")
	i, err := strconv.Atoi(index)
	if !nested || err != nil || i < 0 {
		return fieldMember{}, path
	}
	items, _ := metadata[collection].([]any)
	if i >= len(items) {
		return fieldMember{}, path
	}
	item, _ := items[i].(map[string]any)
	name, _ := item["name"].(string)
	if name == "" {
		return fieldMember{}, path
	}
	return fieldMember{collection, name, i}, leaf
}

// PublicationHeadline identifies the version or content being published.
func PublicationHeadline(p engine.Publication) Line {
	switch {
	case p.Version == "" && (len(p.Uploads) > 0 || len(p.Fields) > 0 || len(p.Removed) > 0 || len(p.Repairs) > 0):
		return Line{{New, "updated"}}
	case p.Version == "":
		return Line{{Old, "unchanged"}}
	case p.Previous == "":
		return Line{{New, "new " + Text(p.Version)}}
	case p.Version != p.Previous:
		return replaced(p.Previous, p.Version)
	case p.Status == engine.StatusUnchanged:
		return Line{{Old, Text(p.Version) + " unchanged"}}
	}
	for _, upload := range p.Uploads {
		if upload.Name == plugin.UploadInstaller {
			note := " (same version)"
			if upload.Filename == p.Version {
				note = " (same filename)"
			}
			return Line{{Plain, Text(p.Version)}, {Old, note}}
		}
	}
	return Line{{Old, Text(p.Version)}}
}

// fieldChange renders one field: its initial value, its removal, or the
// difference between its values.
func fieldChange(field plugin.FieldChange) []string {
	name := Text(field.Field)
	switch field.Field {
	case "pkginfo.installed_size", "pkginfo.installer_item_size":
		name += " (KiB)"
	}
	before, after := decode(field.Before), decode(field.After)
	if _, absent := before.(missing); absent {
		return initial(name, after)
	}
	lines := difference(name, before, after)
	if len(lines) == 0 {
		return []string{name + ": changed"}
	}
	return lines
}

// PruneDetail names the packages a destination prunes.
func PruneDetail(packages []string) string {
	noun := "package "
	if len(packages) != 1 {
		noun = "packages "
	}
	return "prune " + noun + Text(strings.Join(packages, ", "))
}
