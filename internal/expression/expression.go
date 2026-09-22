// Package expression evaluates CEL expressions in decoded configuration values.
package expression

import (
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	"cel.dev/cel-go/cel"
	"cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/operators"
	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
	"cel.dev/cel-go/common/types/traits"
	"cel.dev/cel-go/ext"
)

const (
	maxExpression = 64 << 10
	maxBytes      = 16 << 20
	maxNodes      = 100_000
	maxDepth      = 128
)

var (
	roots    = []string{"env", "evidence", "facts", "inputs"}
	language = sync.OnceValues(newLanguage)
)

func newLanguage() (*cel.Env, error) {
	return cel.NewEnv(
		cel.Variable("env", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("facts", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("evidence", cel.MapType(cel.StringType, cel.DynType)),
		cel.Variable("inputs", cel.MapType(cel.StringType, cel.DynType)),
		cel.OptionalTypes(),
		ext.Strings(),
		cel.ClearMacros(),
		cel.Macros(cel.HasMacro),
		cel.ParserExpressionSizeLimit(maxExpression),
		cel.ParserRecursionLimit(maxDepth),
		cel.ExpressionNodeLimit(maxNodes),
	)
}

// Check validates expressions without reading their context values. Only the
// named context roots are allowed; passing no roots permits literal expressions.
func Check(value any, allowedRoots ...string) error {
	_, err := transform(value, func(source string) (any, error) {
		parts, err := compile(source)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			for _, root := range part.roots() {
				if !slices.Contains(allowedRoots, root) {
					return nil, fmt.Errorf("context %q is not available here", root)
				}
			}
		}
		return source, nil
	})
	return err
}

// Eval resolves templates once, without changing value or contexts. Whole-value
// expressions retain their native type; expressions within text must be scalars.
// Contexts contain only data, with environment values represented as strings.
func Eval(value any, contexts map[string]any) (any, error) {
	budget := limits{}
	data, err := walk(reflect.ValueOf(contexts), "$", 0, &budget, false, func(s string) (any, error) { return s, nil })
	if err != nil {
		return nil, fmt.Errorf("expression contexts: %w", err)
	}
	contextValues := data.(map[string]any)
	for root, value := range contextValues {
		if !slices.Contains(roots, root) {
			return nil, fmt.Errorf("unknown expression context %q", root)
		}
		object, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("expression context %q must be an object", root)
		}
		if root == "env" {
			for _, item := range object {
				if _, ok := item.(string); !ok {
					return nil, errors.New("environment values must be strings")
				}
			}
		}
	}
	return transform(value, func(source string) (any, error) {
		parts, err := compile(source)
		if err != nil {
			return nil, err
		}
		var output strings.Builder
		for _, part := range parts {
			if part.tree == nil {
				output.WriteString(part.text)
				continue
			}
			for _, root := range part.roots() {
				if _, ok := contextValues[root]; !ok {
					return nil, fmt.Errorf("context %q is not available here", root)
				}
			}
			resolved, err := part.eval(contextValues)
			if err != nil {
				return nil, err
			}
			if len(parts) == 1 {
				return resolved, nil
			}
			text, err := scalarText(resolved)
			if err != nil {
				return nil, err
			}
			output.WriteString(text)
			if output.Len() > maxBytes {
				return nil, errors.New("expression result exceeds size limit")
			}
		}
		return output.String(), nil
	})
}

// Has reports whether a value contains an unescaped expression opener.
// Malformed expressions also return true so they can be rejected by Check.
func Has(value any) bool {
	found := false
	_, _ = transform(value, func(s string) (any, error) {
		found = found || hasOpener(s)
		return s, nil
	})
	return found
}

// Roots returns the sorted context roots referenced by a value's expressions.
func Roots(value any) ([]string, error) {
	found := map[string]bool{}
	err := visitExpressions(value, func(part segment) {
		for _, root := range part.roots() {
			found[root] = true
		}
	})
	if err != nil {
		return nil, err
	}
	return sortedKeys(found), nil
}

// Env captures the process environment in a context suitable for Eval.
func Env() map[string]any {
	environment := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			environment[key] = value
		}
	}
	return map[string]any{"env": environment}
}

// Environment captures only environment keys referenced by expressions. Missing
// keys remain absent so optional lookups can use their fallback. Dynamic key
// access, or use of the whole env object, conservatively captures all keys.
func Environment(value any) (map[string]string, error) {
	keys := map[string]bool{}
	all := false
	err := visitExpressions(value, func(part segment) {
		used, bounded := map[int64]bool{}, map[int64]bool{}
		ast.PostOrderVisit(part.tree.NativeRep().Expr(), ast.NewExprVisitor(func(node ast.Expr) {
			if node.Kind() == ast.IdentKind && node.AsIdent() == "env" {
				used[node.ID()] = true
			}
			if target, key, ok := selection(node); ok && target.Kind() == ast.IdentKind && target.AsIdent() == "env" {
				bounded[target.ID()], keys[key] = true, true
			}
		}))
		for id := range used {
			all = all || !bounded[id]
		}
	})
	if err != nil {
		return nil, err
	}
	if all {
		return Env()["env"].(map[string]string), nil
	}
	environment := map[string]string{}
	for key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			environment[key] = value
		}
	}
	return environment, nil
}

// Uses reports the keys of a context root whose field expressions read, such
// as the inputs whose facts are used. Reading a key's whole value counts as
// reading the field; a dynamic key or a use of the whole root reports all.
func Uses(value any, root, field string) ([]string, bool, error) {
	keys := map[string]bool{}
	all := false
	err := visitExpressions(value, func(part segment) {
		used, bounded := map[int64]bool{}, map[int64]bool{}
		selected, read := map[int64]string{}, map[int64]bool{}
		ast.PostOrderVisit(part.tree.NativeRep().Expr(), ast.NewExprVisitor(func(node ast.Expr) {
			if node.Kind() == ast.IdentKind && node.AsIdent() == root {
				used[node.ID()] = true
			}
			target, key, ok := selection(node)
			if !ok {
				return
			}
			if target.Kind() == ast.IdentKind && target.AsIdent() == root {
				bounded[target.ID()], selected[node.ID()] = true, key
			} else if name, found := selected[target.ID()]; found {
				read[target.ID()] = true
				keys[name] = keys[name] || key == field
			}
		}))
		for id := range used {
			all = all || !bounded[id]
		}
		for id, name := range selected {
			if !read[id] {
				keys[name] = true
			}
		}
	})
	var names []string
	for name, reads := range keys {
		if reads {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, all, err
}

// selection reports the operand and literal key of a field selection or
// literal index.
func selection(node ast.Expr) (ast.Expr, string, bool) {
	if node.Kind() == ast.SelectKind {
		selected := node.AsSelect()
		return selected.Operand(), selected.FieldName(), true
	}
	if node.Kind() != ast.CallKind {
		return nil, "", false
	}
	call := node.AsCall()
	args := call.Args()
	if !slices.Contains([]string{operators.Index, operators.OptIndex, operators.OptSelect}, call.FunctionName()) || len(args) != 2 || args[1].Kind() != ast.LiteralKind {
		return nil, "", false
	}
	name, ok := args[1].AsLiteral().(types.String)
	return args[0], string(name), ok
}

func visitExpressions(value any, visit func(segment)) error {
	_, err := transform(value, func(source string) (any, error) {
		parts, err := compile(source)
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			if part.tree != nil {
				visit(part)
			}
		}
		return source, nil
	})
	return err
}

type segment struct {
	text   string
	tree   *cel.Ast
	offset int
}

func (s segment) roots() []string {
	found := map[string]bool{}
	if s.tree != nil {
		ast.PostOrderVisit(s.tree.NativeRep().Expr(), ast.NewExprVisitor(func(node ast.Expr) {
			if node.Kind() == ast.IdentKind && slices.Contains(roots, node.AsIdent()) {
				found[node.AsIdent()] = true
			}
		}))
	}
	return sortedKeys(found)
}

func (s segment) eval(contexts map[string]any) (any, error) {
	environment, err := language()
	if err != nil {
		return nil, err
	}
	program, err := environment.Program(s.tree, cel.CostLimit(maxNodes))
	if err != nil {
		return nil, errors.New("cannot prepare expression")
	}
	result, _, err := program.Eval(contexts)
	if err != nil {
		// Evaluation errors can include context values, such as a failed numeric
		// conversion or a dynamically selected map key. Report only their location.
		reason := "evaluation failed"
		switch {
		case strings.HasPrefix(err.Error(), "no such key:"), strings.HasPrefix(err.Error(), "no such attribute:"):
			reason = "required reference is missing"
		case strings.Contains(err.Error(), "no such overload"):
			reason = "incompatible value types"
		case strings.Contains(err.Error(), "cost limit"):
			reason = "evaluation cost limit exceeded"
		}
		if evaluationError, ok := errors.AsType[*types.Err](err); ok {
			location := s.tree.NativeRep().SourceInfo().GetStartLocation(evaluationError.NodeID())
			return nil, fmt.Errorf("expression at character %d, line %d, column %d: %s", s.offset+1, location.Line(), location.Column()+1, reason)
		}
		return nil, fmt.Errorf("expression at character %d: %s", s.offset+1, reason)
	}
	budget := limits{}
	return native(result, &budget, 0)
}

func scalarText(value any) (string, error) {
	switch value := value.(type) {
	case string:
		return value, nil
	case bool:
		return strconv.FormatBool(value), nil
	case int64:
		return strconv.FormatInt(value, 10), nil
	case uint64:
		return strconv.FormatUint(value, 10), nil
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), nil
	case nil:
		return "", errors.New("cannot interpolate null into text")
	default:
		return "", errors.New("cannot interpolate an object or array into text")
	}
}

func native(value ref.Val, budget *limits, depth int) (any, error) {
	if err := budget.add(depth, 0); err != nil {
		return nil, err
	}
	switch value := value.(type) {
	case types.Null:
		return nil, nil
	case types.Bool:
		return bool(value), nil
	case types.String:
		return string(value), budget.add(depth, len(value))
	case types.Int:
		return int64(value), nil
	case types.Uint:
		return uint64(value), nil
	case types.Double:
		if math.IsInf(float64(value), 0) || math.IsNaN(float64(value)) {
			return nil, errors.New("expression result must be a finite number")
		}
		return float64(value), nil
	case traits.Lister:
		result := make([]any, 0)
		for iterator := value.Iterator(); iterator.HasNext() == types.True; {
			child, err := native(iterator.Next(), budget, depth+1)
			if err != nil {
				return nil, err
			}
			result = append(result, child)
		}
		return result, nil
	case traits.Mapper:
		result := map[string]any{}
		for iterator := value.Iterator(); iterator.HasNext() == types.True; {
			key, ok := iterator.Next().(types.String)
			if !ok {
				return nil, errors.New("expression object keys must be strings")
			}
			if err := budget.add(depth, len(key)); err != nil {
				return nil, err
			}
			child, err := native(value.Get(key), budget, depth+1)
			if err != nil {
				return nil, err
			}
			result[string(key)] = child
		}
		return result, nil
	default:
		return nil, errors.New("expression result must be a string, boolean, number, null, array, or object")
	}
}

type limits struct {
	nodes int
	bytes int
}

func (l *limits) add(depth, size int) error {
	l.nodes++
	l.bytes += size
	if depth > maxDepth || l.nodes > maxNodes || l.bytes > maxBytes {
		return errors.New("expression data exceeds size or depth limit")
	}
	return nil
}

func transform(value any, visit func(string) (any, error)) (any, error) {
	budget := limits{}
	return walk(reflect.ValueOf(value), "$", 0, &budget, true, visit)
}

func walk(value reflect.Value, path string, depth int, budget *limits, authored bool, visit func(string) (any, error)) (any, error) {
	if err := budget.add(depth, 0); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !value.IsValid() {
		return nil, nil
	}
	if value.Kind() == reflect.Interface {
		return walk(value.Elem(), path, depth, budget, authored, visit)
	}
	// New Go kinds do not become admissible expression data automatically.
	//exhaustive:ignore
	switch value.Kind() {
	case reflect.String:
		if err := budget.add(depth, value.Len()); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		result, err := visit(value.String())
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		return result, nil
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String {
			return nil, fmt.Errorf("%s: object keys must be strings", path)
		}
		result := make(map[string]any, value.Len())
		keys := value.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		for _, key := range keys {
			name := key.String()
			if authored && hasOpener(name) {
				return nil, fmt.Errorf("%s: expressions are not supported in object keys", path)
			}
			if err := budget.add(depth, len(name)); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			child, err := walk(value.MapIndex(key), path+"."+name, depth+1, budget, authored, visit)
			if err != nil {
				return nil, err
			}
			result[name] = child
		}
		return result, nil
	case reflect.Slice, reflect.Array:
		result := make([]any, value.Len())
		for i := range result {
			child, err := walk(value.Index(i), fmt.Sprintf("%s[%d]", path, i), depth+1, budget, authored, visit)
			if err != nil {
				return nil, err
			}
			result[i] = child
		}
		return result, nil
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return value.Interface(), nil
	case reflect.Float32, reflect.Float64:
		if math.IsInf(value.Float(), 0) || math.IsNaN(value.Float()) {
			return nil, fmt.Errorf("%s: numbers must be finite", path)
		}
		return value.Interface(), nil
	default:
		return nil, fmt.Errorf("%s: expression values must contain only data", path)
	}
}

func sortedKeys[M ~map[string]V, V any](values M) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
