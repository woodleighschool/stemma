package jamf

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	"github.com/woodleighschool/stemma/plugin"
)

// A classic is a Classic API object collection: policies or patch policies.
type classic struct {
	path string
	root string
	noun string
}

var (
	policies      = classic{constants.EndpointClassicPolicies, "policy", "policy"}
	patchPolicies = classic{policyPath, "patch_policy", "patch policy"}
)

// read returns the object with id, or nil when Jamf has none.
func (c *client) read(ctx context.Context, kind classic, id string) (*xmlNode, error) {
	if !validID(id) {
		return nil, fmt.Errorf("invalid Jamf %s ID", kind.noun)
	}
	result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).GetBytes(kind.path + "/id/" + id)
	if result != nil && result.StatusCode() == http.StatusNotFound {
		return nil, nil
	}
	if err := requestError(ctx, result, err); err != nil {
		return nil, err
	}
	doc, err := parseXML(data, kind.root)
	if err != nil {
		return nil, err
	}
	actual := doc.value("general", "id")
	if actual == "" {
		actual = doc.value("id")
	}
	if actual != id {
		return nil, fmt.Errorf("jamf %s response ID does not match request", kind.noun)
	}
	return doc, nil
}

func (c *client) readXML(ctx context.Context, path, root string) (*xmlNode, error) {
	result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).GetBytes(path)
	if err := requestError(ctx, result, err); err != nil {
		return nil, err
	}
	return parseXML(data, root)
}

// A setting is one value Stemma owns in a Classic API document. Plans name it
// by its declaration field and show value; fragment holds the elements that
// set it, rooted at the document element.
type setting struct {
	field    string
	value    any
	fragment *xmlNode
	// current reads the value a document holds, in declaration terms.
	current func(*xmlNode) any
	// write supplies the fragment to write when it differs from the one
	// compared, such as an icon that must be uploaded first.
	write func(context.Context) (*xmlNode, error)
}

// fragment builds the elements that set value at path under the document root.
func fragment(root string, value any, path ...string) *xmlNode {
	for _, name := range slices.Backward(path) {
		value = map[string]any{name: value}
	}
	return jsonXML(root, raw(value))
}

// text reads the element at path as plans show it.
func text(path ...string) func(*xmlNode) any {
	return func(doc *xmlNode) any { return doc.value(path...) }
}

// flag reads the boolean at path as plans show it.
func flag(path ...string) func(*xmlNode) any {
	return func(doc *xmlNode) any { return doc.value(path...) == "true" }
}

// number reads the integer at path as plans show it.
func number(path ...string) func(*xmlNode) any {
	return func(doc *xmlNode) any {
		value, err := strconv.Atoi(doc.value(path...))
		if err != nil {
			return doc.value(path...)
		}
		return value
	}
}

// choice reads the element at path as the declared name of its native value,
// or the native value when no declared name maps to it.
func choice(values map[string]string, path ...string) func(*xmlNode) any {
	return func(doc *xmlNode) any {
		native := doc.value(path...)
		for declared, value := range values {
			if value == native {
				return declared
			}
		}
		return native
	}
}

// names reads the names of the objects a list at path holds.
func names(path ...string) func(*xmlNode) any {
	return func(doc *xmlNode) any {
		list := doc
		for _, name := range path {
			list = list.child(name)
		}
		found := []string{}
		if list == nil {
			return found
		}
		item := itemName(path[len(path)-1])
		for _, child := range list.Children {
			if child.XMLName.Local == item {
				found = append(found, child.value("name"))
			}
		}
		slices.Sort(found)
		return found
	}
}

// pending returns the settings a document does not hold; a nil document holds none.
func pending(doc *xmlNode, settings []setting) []setting {
	return slices.DeleteFunc(slices.Clone(settings), func(s setting) bool { return doc != nil && containsXML(doc, s.fragment) })
}

// changes reports the pending settings as plan changes under prefix.
func changes(prefix string, doc *xmlNode, settings []setting) []plugin.Change {
	var found []plugin.Change
	for _, s := range pending(doc, settings) {
		change := plugin.Change{Kind: "metadata", Field: prefix + "." + s.field, Action: "set", After: raw(s.value)}
		if doc != nil && s.current != nil {
			change.Before = raw(s.current(doc))
		}
		found = append(found, change)
	}
	return found
}

// update writes the settings a document lacks over its latest read and
// verifies every setting by readback.
func (c *client) update(ctx context.Context, kind classic, id string, doc *xmlNode, settings []setting) error {
	writes := pending(doc, settings)
	if len(writes) == 0 {
		return nil
	}
	for _, s := range writes {
		wanted := s.fragment
		if s.write != nil {
			var err error
			if wanted, err = s.write(ctx); err != nil {
				return err
			}
		}
		mergeXML(doc, wanted)
	}
	body, err := xml.Marshal(doc)
	if err != nil {
		return err
	}
	result, putErr := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).SetHeader("Content-Type", constants.ApplicationXML).SetBody(body).DisableRetry().Put(kind.path + "/id/" + id)
	putErr = requestError(ctx, result, putErr)
	actual, err := c.read(ctx, kind, id)
	if err != nil || actual == nil {
		if putErr != nil {
			return putErr
		}
		return fmt.Errorf("jamf %s update could not be read back", kind.noun)
	}
	for _, s := range settings {
		if !containsXML(actual, s.fragment) {
			if putErr != nil {
				return putErr
			}
			return fmt.Errorf("jamf %s %s did not match readback", kind.noun, s.field)
		}
	}
	return nil
}

// xmlNode retains unowned Classic API fields during partial native reconciliation.
// Replace marks a collection whose items a declaration replaces; Whole marks
// an element a merge replaces rather than merging into, as an object
// reference, whose old name would otherwise outlive a new ID.
type xmlNode struct {
	XMLName  xml.Name   `xml:""`
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []xmlNode  `xml:",any"`
	Replace  bool       `xml:"-"`
	Whole    bool       `xml:"-"`
}

// whole marks the element at path under doc as replaced whole by merges.
func whole(doc *xmlNode, path ...string) *xmlNode {
	node := doc
	for _, name := range path {
		node = node.child(name)
	}
	node.Whole = true
	return doc
}

func parseXML(data []byte, root string) (*xmlNode, error) {
	var node xmlNode
	if err := xml.Unmarshal(data, &node); err != nil {
		return nil, errors.New("invalid Jamf XML response")
	}
	if node.XMLName.Local != root {
		return nil, errors.New("unexpected Jamf XML response root")
	}
	return &node, nil
}
func (n *xmlNode) child(name string) *xmlNode {
	if n == nil {
		return nil
	}
	for i := range n.Children {
		if n.Children[i].XMLName.Local == name {
			return &n.Children[i]
		}
	}
	return nil
}
func (n *xmlNode) value(path ...string) string {
	for _, name := range path {
		n = n.child(name)
		if n == nil {
			return ""
		}
	}
	return n.Text
}
func (n *xmlNode) set(value xmlNode) {
	if child := n.child(value.XMLName.Local); child != nil {
		*child = value
		return
	}
	n.Children = append(n.Children, value)
}
func containsXML(actual, desired *xmlNode) bool {
	if actual == nil {
		return false
	}
	if desired.Replace {
		return sameItems(actual, desired)
	}
	if len(desired.Children) == 0 {
		return actual.Text == desired.Text
	}
	for i := range desired.Children {
		child := &desired.Children[i]
		if !containsXML(actual.child(child.XMLName.Local), child) {
			return false
		}
	}
	return true
}

// itemName names the elements a Classic API list holds.
func itemName(list string) string {
	if list == "self_service_categories" {
		return "category"
	}
	return strings.TrimSuffix(list, "s")
}

// sameItems reports whether a replaced collection holds exactly the desired
// items in any order. Jamf returns more of each object than a declaration
// sets, such as its name beside its ID, and can add a size element.
func sameItems(actual, desired *xmlNode) bool {
	item := itemName(desired.XMLName.Local)
	var items []*xmlNode
	for i := range actual.Children {
		if actual.Children[i].XMLName.Local == item {
			items = append(items, &actual.Children[i])
		}
	}
	if len(items) != len(desired.Children) {
		return false
	}
	used := make([]bool, len(items))
	for i := range desired.Children {
		found := false
		for j, candidate := range items {
			if !used[j] && containsXML(candidate, &desired.Children[i]) {
				used[j], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func mergeXML(current, desired *xmlNode) {
	for _, wanted := range desired.Children {
		old := current.child(wanted.XMLName.Local)
		if old == nil || wanted.Replace || wanted.Whole || len(wanted.Children) == 0 {
			current.set(wanted)
		} else {
			mergeXML(old, &wanted)
		}
	}
}

func jsonXML(name string, data json.RawMessage) *xmlNode {
	n := &xmlNode{XMLName: xml.Name{Local: name}}
	switch data[0] {
	case '{':
		fields, _ := decodeObject(data)
		for _, key := range slices.Sorted(maps.Keys(fields)) {
			n.Children = append(n.Children, *jsonXML(key, fields[key]))
		}
	case '[':
		n.Replace = true
		var entries []json.RawMessage
		_ = json.Unmarshal(data, &entries)
		for _, entry := range entries {
			n.Children = append(n.Children, *jsonXML(itemName(name), entry))
		}
	case '"':
		_ = json.Unmarshal(data, &n.Text)
	default:
		n.Text = string(data)
	}
	return n
}
