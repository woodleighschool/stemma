package macpkg

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/invopop/jsonschema"
)

// Script supplies literal text or selects a file or tree from a declared input.
type Script struct {
	Content *string
	Input   string
	Path    string
}

type scriptReference struct {
	Input string `json:"$input" jsonschema:"minLength=1" jsonschema_description:"Declared input supplying the script or resource."`
	Path  string `json:"path,omitempty" jsonschema_description:"Exact path within a tree, ZIP, TAR or DMG. A dot selects the contents root. Omit to use the original input."`
}

func (s Script) MarshalJSON() ([]byte, error) {
	if s.Content != nil {
		return json.Marshal(*s.Content)
	}
	return json.Marshal(scriptReference{Input: s.Input, Path: s.Path})
}

func (s *Script) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	*s = Script{}
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &s.Content)
	}
	var ref scriptReference
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ref); err != nil {
		return err
	}
	s.Input, s.Path = ref.Input, ref.Path
	return s.validate()
}

func (s Script) JSONSchema() *jsonschema.Schema {
	r := jsonschema.Reflector{DoNotReference: true}
	reference := r.Reflect(scriptReference{})
	reference.ID, reference.Version = "", ""
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{{Type: "string"}, reference}}
}

func (s Script) validate() error {
	if s.Content != nil {
		if s.Input != "" || s.Path != "" {
			return errors.New("script must be text or an input reference")
		}
		return nil
	}
	if s.Input == "" {
		return errors.New("script reference requires $input")
	}
	return nil
}
