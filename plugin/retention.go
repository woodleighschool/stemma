package plugin

import "errors"

// Retention keeps the current publication and the most recent others a
// destination holds for the same software. Providers order that family from the
// destination's own records and additionally protect native references.
type Retention struct {
	Keep int `json:"keep" jsonschema:"minimum=1" jsonschema_description:"Number of versions to retain, including the current publication. Keeps the current version and the N-1 newest others; versions protected by native references also remain."`
}

// Validate rejects a retention setting that could discard every publication.
func (r Retention) Validate() error {
	if r.Keep < 1 {
		return errors.New("retention.keep must be at least 1")
	}
	return nil
}
