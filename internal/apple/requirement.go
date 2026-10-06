package apple

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/deploymenttheory/go-macos-pkg/pkg/pkgsign"
)

// requirement is the complete Developer ID conjunction recorded by codesign:
// identifier, Apple anchor, Developer ID intermediate and application leaf,
// and the leaf's team. Other requirement forms are unsupported.
type requirement struct {
	identifier string
	team       string
}

var (
	developerIDCAField          = "field." + pkgsign.OIDDeveloperIDCA.String()
	developerIDApplicationField = "field." + pkgsign.OIDDeveloperIDApplication.String()
)

// satisfiedBy reports whether authenticated code meets the requirement. Its
// certificate already chains to an Apple root, which is what anchor apple
// generic asks of it, and the Developer ID class is the intermediate and
// application clauses.
func (r requirement) satisfiedBy(identity codeIdentity) bool {
	return identity.identifier == r.identifier && identity.class == classDeveloperID && identity.team == r.team
}

// parseRequirement reads requirement text as codesign writes it: clauses
// joined by and, quoted or bare strings, and comments such as /* exists */.
func parseRequirement(text string) (requirement, error) {
	tokens, err := requirementTokens(text)
	if err != nil {
		return requirement{}, fmt.Errorf("%w: requirement: %w", ErrUnsupported, err)
	}
	clauses := [][]requirementToken{nil}
	for _, token := range tokens {
		if token.is("and") {
			clauses = append(clauses, nil)
			continue
		}
		clauses[len(clauses)-1] = append(clauses[len(clauses)-1], token)
	}
	var r requirement
	seen := map[string]bool{}
	for _, clause := range clauses {
		kind, value := developerIDClause(clause)
		if kind == "" {
			return requirement{}, fmt.Errorf("%w: requirement clause %q", ErrUnsupported, clauseText(clause))
		}
		if seen[kind] {
			return requirement{}, fmt.Errorf("%w: requirement repeats its %s clause", ErrUnsupported, kind)
		}
		seen[kind] = true
		switch kind {
		case "identifier":
			r.identifier = value
		case "team":
			r.team = value
		}
	}
	for _, kind := range []string{"identifier", "anchor", "intermediate", "application", "team"} {
		if !seen[kind] {
			return requirement{}, fmt.Errorf("%w: requirement is not the Developer ID form: it has no %s clause", ErrUnsupported, kind)
		}
	}
	return r, nil
}

// developerIDClause names a clause of the Developer ID form and its value, or
// returns an empty kind.
func developerIDClause(clause []requirementToken) (kind, value string) {
	switch {
	case len(clause) == 2 && clause[0].is("identifier") && clause[1].value():
		return "identifier", clause[1].text
	case len(clause) == 3 && clause[0].is("identifier") && clause[1].is("=") && clause[2].value():
		return "identifier", clause[2].text
	case len(clause) == 3 && clause[0].is("anchor") && clause[1].is("apple") && clause[2].is("generic"):
		return "anchor", ""
	case len(clause) < 5 || !clause[0].is("certificate") && !clause[0].is("cert") || !clause[2].is("[") || !clause[4].is("]"):
		return "", ""
	}
	position, key, match := clause[1], clause[3], clause[5:]
	leaf := position.is("leaf") || position.is("0")
	// A field without a match operator tests that it exists.
	exists := len(match) == 0 || len(match) == 1 && match[0].is("exists")
	switch {
	case position.is("1") && key.is(developerIDCAField) && exists:
		return "intermediate", ""
	case leaf && key.is(developerIDApplicationField) && exists:
		return "application", ""
	case leaf && key.is("subject.OU") && len(match) == 2 && match[0].is("=") && match[1].value():
		return "team", match[1].text
	}
	return "", ""
}

type requirementToken struct {
	text   string
	quoted bool
	symbol bool
}

// is reports whether the token is the keyword or symbol s. A quoted string is
// never a keyword.
func (t requirementToken) is(s string) bool {
	return !t.quoted && t.text == s
}

func (t requirementToken) value() bool {
	if t.quoted {
		return true
	}
	// codesign quotes values other than plain identifiers. In particular,
	// bare numbers and hexadecimal data must not become literal strings.
	if t.symbol || t.text == "" || !asciiLetter(t.text[0]) {
		return false
	}
	for i := range len(t.text) {
		if !asciiLetter(t.text[i]) && (t.text[i] < '0' || t.text[i] > '9') {
			return false
		}
	}
	switch t.text {
	case "certificate", "always", "host", "guest", "cdhash", "entitlement",
		"library", "timestamp", "legacy", "never", "cert", "plugin", "absent",
		"or", "leaf", "info", "designated", "apple", "trusted", "true", "notarized",
		"and", "root", "platform", "anchor", "false", "generic", "identifier", "exists":
		return false
	}
	return true
}

func requirementTokens(text string) ([]requirementToken, error) {
	var tokens []requirementToken
	for i := 0; i < len(text); {
		switch c := text[i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			i++
		case strings.HasPrefix(text[i:], "/*"):
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				return nil, errors.New("unterminated comment")
			}
			i += end + 4
		case c == '"':
			var value strings.Builder
			for i++; ; i++ {
				if i >= len(text) {
					return nil, errors.New("unterminated string")
				}
				if text[i] == '"' {
					i++
					break
				}
				if text[i] == '\\' {
					i++
					if i >= len(text) || text[i] != '"' {
						return nil, errors.New("unsupported string escape")
					}
				}
				value.WriteByte(text[i])
			}
			tokens = append(tokens, requirementToken{text: value.String(), quoted: true})
		case strings.IndexByte("[]=<>!()", c) >= 0:
			tokens = append(tokens, requirementToken{text: text[i : i+1], symbol: true})
			i++
		case bareRequirementByte(c):
			start := i
			for i < len(text) && bareRequirementByte(text[i]) {
				i++
			}
			tokens = append(tokens, requirementToken{text: text[start:i]})
		default:
			return nil, fmt.Errorf("unexpected character %q", c)
		}
	}
	return tokens, nil
}

func asciiLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func bareRequirementByte(c byte) bool {
	return asciiLetter(c) || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-'
}

func clauseText(clause []requirementToken) string {
	words := make([]string, len(clause))
	for i, token := range clause {
		words[i] = token.text
		if token.quoted {
			words[i] = strconv.Quote(token.text)
		}
	}
	return strings.Join(words, " ")
}
