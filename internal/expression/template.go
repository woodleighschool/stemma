package expression

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"cel.dev/cel-go/parser/gen"
	"github.com/antlr4-go/antlr/v4"
)

func hasOpener(source string) bool {
	for start := 0; start < len(source); {
		index := strings.Index(source[start:], "{{")
		if index < 0 {
			return false
		}
		index += start
		if index == 0 || source[index-1] != '\\' {
			return true
		}
		start = index + 2
	}
	return false
}

func compile(source string) ([]segment, error) {
	var parts []segment
	var text strings.Builder
	for offset := 0; offset < len(source); {
		index := strings.Index(source[offset:], "{{")
		if index < 0 {
			text.WriteString(source[offset:])
			break
		}
		index += offset
		if index > offset && source[index-1] == '\\' {
			text.WriteString(source[offset : index-1])
			text.WriteString("{{")
			offset = index + 2
			continue
		}
		text.WriteString(source[offset:index])
		if text.Len() != 0 {
			parts = append(parts, segment{text: text.String()})
			text.Reset()
		}
		body, consumed, err := expressionEnd(source[index+2:])
		if err != nil {
			return nil, fmt.Errorf("expression at character %d: %w", utf8.RuneCountInString(source[:index])+1, err)
		}
		environment, err := language()
		if err != nil {
			return nil, err
		}
		tree, issues := environment.Compile(body)
		if err := issues.Err(); err != nil {
			return nil, fmt.Errorf("expression at character %d: %w", utf8.RuneCountInString(source[:index])+1, err)
		}
		parts = append(parts, segment{tree: tree, offset: utf8.RuneCountInString(source[:index])})
		offset = index + 2 + consumed
	}
	if text.Len() != 0 || len(parts) == 0 {
		parts = append(parts, segment{text: text.String()})
	}
	return parts, nil
}

func expressionEnd(source string) (string, int, error) {
	// CEL's lexer owns strings, escapes, comments, and literal braces. Delimiter
	// discovery uses those tokens rather than maintaining a second CEL grammar.
	runes := []rune(source)
	if len(runes) > maxExpression+2 {
		runes = runes[:maxExpression+2]
	}
	lexer := gen.NewCELLexer(antlr.NewInputStream(string(runes)))
	lexer.RemoveErrorListeners()
	depth := 0
	for token := lexer.NextToken(); token.GetTokenType() != antlr.TokenEOF; token = lexer.NextToken() {
		switch token.GetTokenType() {
		case gen.CELLexerLBRACE:
			depth++
		case gen.CELLexerRBRACE:
			if depth > 0 {
				depth--
				continue
			}
			next := lexer.NextToken()
			if next.GetTokenType() == gen.CELLexerRBRACE && next.GetStart() == token.GetStop()+1 {
				body := string(runes[:token.GetStart()])
				return body, len(string(runes[:next.GetStop()+1])), nil
			}
			return "", 0, errors.New("expected closing }} delimiter")
		}
	}
	if len([]rune(source)) > maxExpression {
		return "", 0, errors.New("expression exceeds size limit")
	}
	return "", 0, errors.New("unclosed {{ expression")
}
