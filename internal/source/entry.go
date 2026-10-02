package source

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

// Content identifies bytes and their filesystem representation. Tree bytes are a
// canonical TAR retaining file and directory modes and confined symlink targets.
type Content struct {
	SHA256   string `json:"sha256" yaml:"sha256"`
	Filename string `json:"filename" yaml:"filename"`
	Tree     bool   `json:"tree,omitempty" yaml:"tree,omitempty"`
	Mode     uint32 `json:"mode" yaml:"mode"`
}

// Entry is the common lock envelope. Observation belongs to the named resolver;
// Evidence is reviewed JSON passed to consumers. Both must be nonsecret.
type Entry struct {
	Version         int                        `json:"version" yaml:"version"`
	Resolver        string                     `json:"resolver" yaml:"resolver"`
	ResolverVersion string                     `json:"resolver_version" yaml:"resolver_version"`
	Declaration     string                     `json:"declaration" yaml:"declaration"`
	Observation     json.RawMessage            `json:"observation" yaml:"observation"`
	Download        *plugin.Download           `json:"download,omitempty" yaml:"download,omitempty"`
	Content         Content                    `json:"content" yaml:"content"`
	InputVersion    string                     `json:"input_version,omitempty" yaml:"input_version,omitempty"`
	ContentRoot     string                     `json:"content_root,omitempty" yaml:"content_root,omitempty"`
	Evidence        map[string]json.RawMessage `json:"evidence,omitempty" yaml:"evidence,omitempty"`
}

// Validate checks only the shared lock envelope; resolver-owned observations are
// interpreted by their resolver when content must be fetched again.
func (entry Entry) Validate() error {
	if entry.Version != 1 || !plugin.ValidOperationName(entry.Resolver) || entry.ResolverVersion == "" || !validDigest(entry.Declaration) {
		return errors.New("unsupported or incomplete input lock envelope")
	}
	if err := validateDownload(entry.Download); err != nil {
		return err
	}
	if entry.Download != nil && entry.Content.Tree {
		return errors.New("HTTP downloads must identify file content")
	}
	if !entry.Content.valid() {
		return errors.New("invalid locked input content")
	}
	if entry.ContentRoot != "" && (!fs.ValidPath(entry.ContentRoot) || strings.ContainsAny(entry.ContentRoot, "\\\x00\r\n\t")) {
		return errors.New("invalid locked input content root")
	}
	if len(entry.Observation) == 0 {
		return errors.New("missing resolver observation")
	}
	if !json.Valid(entry.Observation) {
		return errors.New("invalid resolver observation")
	}
	for name, value := range entry.Evidence {
		if !json.Valid(value) {
			return fmt.Errorf("invalid resolver evidence %q", name)
		}
	}
	return nil
}

func (content Content) valid() bool {
	return validFilename(content.Filename) && validDigest(content.SHA256) && content.Mode <= 0o777
}

// Equal compares semantic lock state, including JSON observations and evidence.
func (entry Entry) Equal(other Entry) bool {
	if entry.InputVersion != other.InputVersion || entry.ContentRoot != other.ContentRoot {
		return false
	}
	if entry.Version != other.Version || entry.Resolver != other.Resolver || entry.ResolverVersion != other.ResolverVersion || entry.Declaration != other.Declaration || entry.Content != other.Content || !equalDownload(entry.Download, other.Download) || !sameJSON(entry.Observation, other.Observation) {
		return false
	}
	leftEvidence, err := canonicalEvidence(entry.Evidence)
	if err != nil {
		return false
	}
	rightEvidence, err := canonicalEvidence(other.Evidence)
	return err == nil && maps.EqualFunc(leftEvidence, rightEvidence, func(a, b json.RawMessage) bool { return bytes.Equal(a, b) })
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

func canonicalEvidence(evidence map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	if len(evidence) == 0 {
		return nil, nil
	}
	result := make(map[string]json.RawMessage, len(evidence))
	for name, value := range evidence {
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

func equalDownload(a, b *plugin.Download) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}
