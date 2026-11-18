package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"strconv"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

// Content identifies bytes and their filesystem representation. Tree bytes are a
// canonical TAR retaining file and directory modes and confined symlink targets.
// Root names the directory inside the content that the input uses.
type Content struct {
	Filename string
	SHA256   string
	Mode     uint32
	Tree     bool
	Root     string
}

// contentJSON is how lock documents and the source index record content: the
// mode as an octal string, written only when it differs from its kind's
// default.
type contentJSON struct {
	Filename string `json:"filename"`
	SHA256   string `json:"sha256"`
	Mode     string `json:"mode,omitempty"`
	Tree     bool   `json:"tree,omitempty"`
	Root     string `json:"root,omitempty"`
}

// defaultMode is the mode content of its kind has unless the lock says otherwise.
func defaultMode(tree bool) uint32 {
	if tree {
		return 0o755
	}
	return 0o644
}

func (content Content) MarshalJSON() ([]byte, error) {
	recorded := contentJSON{Filename: content.Filename, SHA256: content.SHA256, Tree: content.Tree, Root: content.Root}
	if content.Mode != defaultMode(content.Tree) {
		recorded.Mode = fmt.Sprintf("%04o", content.Mode)
	}
	return json.Marshal(recorded)
}

func (content *Content) UnmarshalJSON(data []byte) error {
	var recorded contentJSON
	if err := decode(data, &recorded); err != nil {
		return err
	}
	*content = Content{Filename: recorded.Filename, SHA256: recorded.SHA256, Mode: defaultMode(recorded.Tree), Tree: recorded.Tree, Root: recorded.Root}
	if recorded.Mode != "" {
		mode, err := strconv.ParseUint(recorded.Mode, 8, 32)
		if err != nil || len(recorded.Mode) != 4 {
			return fmt.Errorf("content mode %q is not a four-digit octal mode", recorded.Mode)
		}
		content.Mode = uint32(mode)
	}
	return nil
}

// Entry is one input's lock entry. Version is the version the resolver states
// for the content, if any. Origin belongs to the named resolver: what it needs
// to fetch the same bytes again, never the version or content identity the
// entry already records. Evidence is reviewed JSON passed to consumers. Both
// must be nonsecret.
type Entry struct {
	Version         string                     `json:"version,omitempty"`
	Content         Content                    `json:"content"`
	Origin          json.RawMessage            `json:"origin,omitempty"`
	Resolver        string                     `json:"resolver"`
	ResolverVersion string                     `json:"resolver_version"`
	Declaration     string                     `json:"declaration"`
	Evidence        map[string]json.RawMessage `json:"evidence,omitempty"`
}

// Validate checks only the shared entry; resolver-owned origins are
// interpreted by their resolver when content must be fetched again.
func (entry Entry) Validate() error {
	if !plugin.ValidOperationName(entry.Resolver) || entry.ResolverVersion == "" || !validDigest(entry.Declaration) {
		return errors.New("unsupported or incomplete input lock entry")
	}
	if !entry.Content.valid() {
		return errors.New("invalid locked input content")
	}
	if root := entry.Content.Root; root != "" && (!fs.ValidPath(root) || strings.ContainsAny(root, "\\\x00\r\n\t")) {
		return errors.New("invalid locked input content root")
	}
	if len(entry.Origin) > 0 {
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry.Origin, &fields) != nil || len(fields) == 0 {
			return errors.New("resolver origin must be a nonempty JSON object")
		}
	}
	for name, value := range entry.Evidence {
		if !json.Valid(value) {
			return fmt.Errorf("invalid resolver evidence %q", name)
		}
	}
	return nil
}

// expected is the content an entry says its origin fetches, or nil when it
// does not know yet.
func (entry Entry) expected() *Content {
	if entry.Content.SHA256 == "" {
		return nil
	}
	content := entry.Content
	return &content
}

func (content Content) valid() bool {
	return validFilename(content.Filename) && validDigest(content.SHA256) && content.Mode <= 0o777
}

// Equal compares semantic lock state, including JSON origins and evidence.
func (entry Entry) Equal(other Entry) bool {
	if entry.Version != other.Version || entry.Content != other.Content || entry.Resolver != other.Resolver || entry.ResolverVersion != other.ResolverVersion || entry.Declaration != other.Declaration || !equalOrigin(entry.Origin, other.Origin) {
		return false
	}
	leftEvidence, err := canonicalEvidence(entry.Evidence)
	if err != nil {
		return false
	}
	rightEvidence, err := canonicalEvidence(other.Evidence)
	return err == nil && maps.EqualFunc(leftEvidence, rightEvidence, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) })
}

// canonicalOrigin is a resolver origin as the lock records it: canonical JSON,
// or nothing when the resolver needs nothing to fetch the bytes again.
func canonicalOrigin(origin json.RawMessage) (json.RawMessage, error) {
	if len(origin) == 0 {
		return nil, nil
	}
	canonical, err := canonicalJSON(origin)
	if err != nil || string(canonical) == "{}" {
		return nil, err
	}
	return canonical, nil
}

func equalOrigin(a, b json.RawMessage) bool {
	left, err := canonicalOrigin(a)
	if err != nil {
		return false
	}
	right, err := canonicalOrigin(b)
	return err == nil && bytes.Equal(left, right)
}

// MarshalYAML preserves resolver JSON as ordinary YAML objects, including exact
// integer values, rather than serializing RawMessage as a byte array.
func (entry Entry) MarshalYAML() (any, error) {
	type plain Entry
	data, err := json.Marshal(plain(entry))
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	var blockStyle func(*yaml.Node)
	blockStyle = func(node *yaml.Node) {
		node.Style = 0
		for _, child := range node.Content {
			blockStyle(child)
		}
	}
	blockStyle(&node)
	return node.Content[0], nil
}

func (entry *Entry) UnmarshalYAML(node *yaml.Node) error {
	value, err := yamlJSON(node)
	if err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	type plain Entry
	if err := decode(data, (*plain)(entry)); err != nil {
		return err
	}
	entry.Evidence, err = canonicalEvidence(entry.Evidence)
	return err
}

// canonicalEvidence is resolver evidence as the lock records it. Each name is
// one namespace a resolver owns; the host's namespaces are not its to write.
func canonicalEvidence(evidence map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if len(evidence) == 0 {
		return nil, nil
	}
	result := make(map[string]json.RawMessage, len(evidence))
	for name, value := range evidence {
		switch {
		case !plugin.ValidEvidenceName(name):
			return nil, fmt.Errorf("resolver evidence %q must be one namespace name; nest what it holds", name)
		case plugin.HostEvidence(name):
			return nil, fmt.Errorf("resolver evidence %q is a namespace Stemma writes", name)
		}
		canonical, err := canonicalJSON(value)
		if err != nil {
			return nil, fmt.Errorf("resolver evidence %q: %w", name, err)
		}
		result[name] = canonical
	}
	return result, nil
}

func yamlJSON(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.MappingNode:
		values := map[string]any{}
		for i := 0; i < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Tag != "!!str" || key.Value == "<<" {
				return nil, errors.New("lock mapping keys must be strings")
			}
			if _, ok := values[key.Value]; ok {
				return nil, fmt.Errorf("duplicate lock field %q", key.Value)
			}
			value, err := yamlJSON(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			values[key.Value] = value
		}
		return values, nil
	case yaml.SequenceNode:
		values := make([]any, 0, len(node.Content))
		for _, child := range node.Content {
			value, err := yamlJSON(child)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	case yaml.DocumentNode, yaml.AliasNode, yaml.StreamNode:
		return nil, errors.New("input locks do not support nested documents or aliases")
	case yaml.ScalarNode:
		switch node.Tag {
		case "!!null":
			return nil, nil
		case "!!bool":
			var value bool
			err := node.Decode(&value)
			return value, err
		case "!!int", "!!float":
			return json.Number(node.Value), nil
		case "!!str", "!!timestamp":
			return node.Value, nil
		}
	}
	return nil, errors.New("unsupported YAML value in input lock")
}

func canonicalJSON(data json.RawMessage) (json.RawMessage, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected one JSON value")
	}
	return json.Marshal(value)
}
