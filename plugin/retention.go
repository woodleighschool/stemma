package plugin

import (
	"cmp"
	"errors"
	"slices"
)

// Retention keeps the current payload and recent distinct successful publications.
// Providers additionally protect native references and require durable ownership.
type Retention struct {
	Keep int `json:"keep" jsonschema:"minimum=1"`
}

// Validate rejects a retention setting that could discard every publication.
func (r Retention) Validate() error {
	if r.Keep < 1 {
		return errors.New("retention.keep must be at least 1")
	}
	return nil
}

// Publications records successful payload transitions, independently of native IDs.
// It belongs in durable bindings. It does not establish ownership of remote objects.
type Publications struct {
	Sequence uint64            `json:"sequence"`
	Current  string            `json:"current"`
	Order    map[string]uint64 `json:"order"`
}

// Record advances only when the published payload changes, after intended references succeed.
func (p *Publications) Record(fingerprint string) uint64 {
	if fingerprint == p.Current && p.Order[fingerprint] != 0 {
		return p.Order[fingerprint]
	}
	if p.Order == nil {
		p.Order = make(map[string]uint64)
	}
	p.Sequence++
	p.Current = fingerprint
	p.Order[fingerprint] = p.Sequence
	return p.Sequence
}

// Retained selects payloads by publication order, never native ID or version string.
// Unknown history returns no selection; callers must not treat that as permission to delete.
func (p *Publications) Retained(keep int) map[string]bool {
	retained := make(map[string]bool)
	if keep < 1 || p.Current == "" || p.Order[p.Current] == 0 {
		return retained
	}
	retained[p.Current] = true
	var others []string
	for fingerprint, sequence := range p.Order {
		if fingerprint != p.Current && sequence != 0 {
			others = append(others, fingerprint)
		}
	}
	slices.SortFunc(others, func(a, b string) int { return cmp.Compare(p.Order[b], p.Order[a]) })
	for _, fingerprint := range others {
		if len(retained) >= keep {
			break
		}
		retained[fingerprint] = true
	}
	return retained
}
