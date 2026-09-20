package expression

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEvalPreservesNativeValues(t *testing.T) {
	contexts := map[string]any{
		"env":   map[string]string{"VERSION": "1.2.3", "TEXT": "{{ facts.missing }}", "BOOLEAN": "false"},
		"facts": map[string]any{"app": map[string]any{"enabled": false, "count": 0, "nothing": nil, "empty": ""}},
	}
	for _, test := range []struct {
		name   string
		source any
		want   any
	}{
		{"literal string", "false", "false"},
		{"environment string", "{{ env.BOOLEAN }}", "false"},
		{"false", "{{ facts.app.enabled }}", false},
		{"zero", "{{ facts.app.count }}", int64(0)},
		{"null", "{{ null }}", nil},
		{"native map", `{{ {"nested": [1, false, null], "text": "false"} }}`, map[string]any{"nested": []any{int64(1), false, nil}, "text": "false"}},
		{"native list", "{{ [1, 2.5, true] }}", []any{int64(1), 2.5, true}},
		{"embedded", "release {{ env.VERSION }} {{ false }} {{ 2.5 }}", "release 1.2.3 false 2.5"},
		{"whitespace is text", " {{ true }} ", " true "},
		{"adjacent", "{{ 1 }}{{ 2 }}", "12"},
		{"no rerender", "{{ env.TEXT }}", "{{ facts.missing }}"},
		{"escaped", `\{{ env.MISSING }}`, "{{ env.MISSING }}"},
		{"escaped and evaluated", `\{{ missing }} {{ env.VERSION }}`, "{{ missing }} 1.2.3"},
		{"nested values", map[string]any{"values": []any{"{{ true }}", "v{{ env.VERSION }}"}}, map[string]any{"values": []any{true, "v1.2.3"}}},
		{"shell syntax", "echo ${PATH} ${PATH:-fallback}", "echo ${PATH} ${PATH:-fallback}"},
		{"quoted delimiter", `{{ "}}" + "{{" }}`, "}}{{"},
		{"map closing adjacent", `{{{"nested": {"x": 1}}}}`, map[string]any{"nested": map[string]any{"x": int64(1)}}},
		{"unicode", `α{{ "😀}}" }}ω`, "α😀}}ω"},
		{"raw string", `{{ r"a\b}}" }}`, `a\b}}`},
		{"multiline string", "{{ '''hello\n}}world''' }}", "hello\n}}world"},
		{"comment delimiter", "{{ 1 // }} in comment\n + 2 }}", int64(3)},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := Eval(test.source, contexts)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("got %#v (%T), want %#v (%T)", got, got, test.want, test.want)
			}
		})
	}
}

func TestExplicitFallbackPreservesPresence(t *testing.T) {
	contexts := map[string]any{"facts": map[string]any{"app": map[string]any{"enabled": false, "count": 0, "empty": "", "nothing": nil}}}
	for _, test := range []struct {
		source string
		want   any
	}{
		{`{{ facts.?missing.nested.orValue("fallback") }}`, "fallback"},
		{`{{ facts.app.?enabled.orValue(true) }}`, false},
		{`{{ facts.app.?count.orValue(10) }}`, int64(0)},
		{`{{ facts.app.?empty.orValue("fallback") }}`, ""},
		{`{{ facts.app.?nothing.orValue("fallback") }}`, nil},
		{`{{ facts.app.?enabled.orValue(facts.missing) }}`, false},
	} {
		got, err := Eval(test.source, contexts)
		if err != nil || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("%s: got %#v, %v; want %#v", test.source, got, err, test.want)
		}
	}
}

func TestCheckRejectsInvalidAndUnavailableExpressions(t *testing.T) {
	for _, value := range []any{
		"{{ env.X }}", "{{ shell('echo') }}", "{{ now() }}", "{{ 1 == '1' }}", "{{ true + 1 }}",
		"{{ [1, 2].map(x, x + 1) }}", "{{ [1, 2].all(x, x > 0) }}", "{{ optional.of(1).optMap(x, x + 1) }}",
		"{{ facts.app }}", "{{", "{{ 1 }", "{{ 1 } }", "{{ }}", "{{ env.X ?? 'fallback' }}",
		map[string]any{"{{ env.KEY }}": "value"},
	} {
		if err := Check(value); err == nil {
			t.Fatalf("accepted %#v", value)
		}
	}
	if err := Check("{{ env.VERSION }}", "env"); err != nil {
		t.Fatal(err)
	}
	if err := Check(map[string]any{"$input": "app", "script": "echo ${PATH}"}); err != nil {
		t.Fatal(err)
	}
	if err := Check(`\{{ env.SECRET }}`); err != nil {
		t.Fatal(err)
	}
}

func TestEvalRejectsMissingReferencesAndInvalidText(t *testing.T) {
	contexts := map[string]any{"env": map[string]string{}, "facts": map[string]any{"text": "1", "number": 1}}
	for _, source := range []string{
		"{{ env.MISSING }}", "{{ facts.missing }}", "{{ facts.missing == null }}", "{{ facts.missing.?nested.orValue(1) }}",
		"{{ facts.text + facts.number }}", "{{ facts.text ? true : false }}", "x{{ null }}", "x{{ [1] }}", "x{{ {'a': 1} }}",
		"{{ {1: 'value'} }}", "{{ b'bytes' }}", "{{ optional.none() }}", "{{ double('NaN') }}", "{{ timestamp('2026-01-01T00:00:00Z') }}",
	} {
		if _, err := Eval(source, contexts); err == nil {
			t.Fatalf("accepted %s", source)
		}
	}
	got, err := Eval("{{ facts.text == facts.number }}", contexts)
	if err != nil || got != false {
		t.Fatalf("equality coerced types: %#v, %v", got, err)
	}
}

func TestErrorsDoNotExposeContextValues(t *testing.T) {
	secret := "synthetic-secret-value"
	contexts := map[string]any{"env": map[string]string{"SECRET": secret}}
	for _, source := range []string{"{{ int(env.SECRET) }}", "{{ env[env.SECRET] }}"} {
		_, err := Eval(map[string]any{"payload": []any{source}}, contexts)
		if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "$.payload[0]") {
			t.Fatalf("unhelpful or unsafe error: %v", err)
		}
	}
}

func TestStringTransformationsArePure(t *testing.T) {
	contexts := map[string]any{"env": map[string]string{"PASSWORD": "it's $(literal)"}}
	got, err := Eval(`password='{{ env.PASSWORD.replace("'", "'\\''") }}'`, contexts)
	if err != nil || got != `password='it'\''s $(literal)'` {
		t.Fatalf("replacement: %#v, %v", got, err)
	}
	for _, source := range []string{"{{ env.PASSWORD.Set('value') }}", "{{ env.PASSWORD.Exec() }}", "{{ [1].map(x, x) }}"} {
		if err := Check(source, "env"); err == nil {
			t.Fatalf("accepted unregistered method or comprehension: %s", source)
		}
	}
}

func TestRenderedShellArgumentRemainsLiteral(t *testing.T) {
	shell, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("POSIX shell unavailable")
	}
	marker := filepath.Join(t.TempDir(), "executed")
	password := "it's $(touch '" + marker + "') `touch '" + marker + "'`\n{{ env.SHOULD_NOT_RENDER }}"
	contexts := map[string]any{"env": map[string]string{"PASSWORD": password}}
	argument, err := Eval(`'{{ env.PASSWORD.replace("'", "'\\''") }}'`, contexts)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.CommandContext(t.Context(), shell, "-c", "printf '%s' "+argument.(string)).Output()
	if err != nil || string(output) != password {
		t.Fatalf("shell changed the argument: %q, %v", output, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("shell executed command substitution: %v", err)
	}
}

func TestContextsAndResultsRemainIndependent(t *testing.T) {
	data := map[string]any{"enabled": false, "literal": "{{ env.SECRET }}", "nested": []string{"one"}, "{{ key }}": "literal"}
	contexts := map[string]any{"facts": map[string]any{"data": data}}
	got, err := Eval("{{ facts.data }}", contexts)
	if err != nil {
		t.Fatal(err)
	}
	result := got.(map[string]any)
	result["enabled"] = true
	result["nested"].([]any)[0] = "changed"
	if data["enabled"] != false || data["nested"].([]string)[0] != "one" || result["literal"] != "{{ env.SECRET }}" {
		t.Fatal("evaluation mutated or reinterpreted context data")
	}
	if result["{{ key }}"] != "literal" {
		t.Fatal("context data was treated as authored syntax")
	}
	if _, err := Eval("{{ facts.call() }}", map[string]any{"facts": map[string]any{"call": func() string { return "unsafe" }}}); err == nil {
		t.Fatal("accepted a function in a data context")
	}
}

func TestReferencesTrackOnlyEnvironmentDependencies(t *testing.T) {
	t.Setenv("STEMMA_EXPRESSION_VERSION", "1.2.3")
	t.Setenv("STEMMA_EXPRESSION_SUFFIX", "pkg")
	t.Setenv("STEMMA_EXPRESSION_UNUSED", "irrelevant")
	value := []any{
		"{{ env.STEMMA_EXPRESSION_VERSION + facts.app.version }}",
		`{{ env["STEMMA_EXPRESSION_SUFFIX"] }}`,
		`{{ env.?STEMMA_EXPRESSION_MISSING.orValue("fallback") }}`,
		`\{{ env.STEMMA_EXPRESSION_UNUSED }}`,
	}
	got, err := Environment(value)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"STEMMA_EXPRESSION_VERSION": "1.2.3", "STEMMA_EXPRESSION_SUFFIX": "pkg"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	used, err := Roots(value)
	if err != nil || !reflect.DeepEqual(used, []string{"env", "facts"}) {
		t.Fatalf("roots: %#v, %v", used, err)
	}
	for _, source := range []string{"{{ env }}", "{{ env[facts.name] }}"} {
		got, err := Environment(source)
		if err != nil || got["STEMMA_EXPRESSION_UNUSED"] != "irrelevant" {
			t.Fatalf("dynamic environment access was not captured: %v", err)
		}
	}
	if !Has(value) || Has(`\{{ env.NAME }}`) || !Has("{{ broken") {
		t.Fatal("expression detection disagrees with escaping or malformed expressions")
	}
}

func TestExpressionLimits(t *testing.T) {
	if err := Check("{{ '" + strings.Repeat("a", maxExpression) + "' }}"); err == nil {
		t.Fatal("accepted oversized expression")
	}
	var deep any = "value"
	for range maxDepth + 1 {
		deep = []any{deep}
	}
	if err := Check(deep); err == nil {
		t.Fatal("accepted excessive data nesting")
	}
	_, err := Eval("{{ env.LARGE + env.LARGE }}", map[string]any{"env": map[string]string{"LARGE": strings.Repeat("x", maxBytes)}})
	if err == nil {
		t.Fatal("accepted oversized context or result")
	}
	_, err = Eval("{{ env.LARGE + env.LARGE }}", map[string]any{"env": map[string]string{"LARGE": strings.Repeat("x", 1<<20)}})
	if err == nil || !strings.Contains(err.Error(), "cost limit") {
		t.Fatalf("evaluation cost limit was not applied: %v", err)
	}
}

func TestConcurrentEvaluations(t *testing.T) {
	for range 16 {
		t.Run("evaluate", func(t *testing.T) {
			t.Parallel()
			got, err := Eval("{{ env.VALUE + '!' }}", map[string]any{"env": map[string]string{"VALUE": "same"}})
			if err != nil || got != "same!" {
				t.Fatalf("got %#v, %v", got, err)
			}
		})
	}
}
