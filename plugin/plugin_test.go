package plugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/plugin"
)

func TestExecutableProtocol(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "echo")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./testdata/echo")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	t.Run("logs stream before completion and obey the caller level", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		release := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer server.Close()
		logs := &notifyingWriter{seen: make(chan struct{})}
		ctx = plugin.WithLogger(ctx, slog.New(slog.NewJSONHandler(logs, nil)).With("resource", "fixture"))
		result := make(chan error, 1)
		go func() {
			_, err := plugin.Run(ctx, binary, reconcileRequest(t, plugin.ReconcileRequest{Method: "plan", Config: raw(t, map[string]string{"wait_url": server.URL})}))
			result <- err
		}()
		select {
		case <-logs.seen:
		case <-ctx.Done():
			close(release)
			t.Fatal("plugin did not stream its stage while running")
		}
		close(release)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if text := logs.String(); strings.Contains(text, "Fixture request") || !strings.Contains(text, `"resource":"fixture"`) || !strings.Contains(text, `"current":3`) || !strings.Contains(text, `"total":7`) {
			t.Fatalf("plugin lost caller scope or log filtering: %s", text)
		}
		var debug bytes.Buffer
		debugCtx := plugin.WithLogger(t.Context(), slog.New(slog.NewTextHandler(&debug, &slog.HandlerOptions{Level: slog.LevelDebug})))
		if _, err := plugin.Run(debugCtx, binary, reconcileRequest(t, plugin.ReconcileRequest{Method: "plan"})); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(debug.String(), "Fixture request") {
			t.Fatal("debug level did not reach the plugin")
		}
	})
	t.Run("one executable exposes resource and destination contracts", func(t *testing.T) {
		response, err := plugin.Run(t.Context(), binary, plugin.Request{Method: "describe"})
		if err != nil {
			t.Fatal(err)
		}
		var descriptor plugin.Descriptor
		if err := json.Unmarshal(response.Output, &descriptor); err != nil {
			t.Fatal(err)
		}
		if err := plugin.ValidateDescriptor(descriptor); err != nil {
			t.Fatal(err)
		}
		if descriptor.Name != "echo" || len(descriptor.Operations) != 3 || descriptor.Operations[0].Name != "echo.build" || descriptor.Operations[2].Name != "echo.reconcile" {
			t.Fatalf("descriptor = %+v", descriptor)
		}
		facts := plugin.Facts{Version: plugin.FactsVersion, Subjects: []plugin.Subject{
			{ID: "package", Kind: "package", Package: &plugin.PackageFacts{Identifier: "org.example.package", Version: "4.2", HasPayload: true}},
			{ID: "app", Parent: "package", Kind: "application", Path: "Example.app", InstalledPath: "/Applications/Example.app", App: &plugin.AppFacts{BundleID: "org.example.app", Version: "4.1", Build: "402"}},
		}}
		request := plugin.ResourceRequest{Workspace: t.TempDir(), Inputs: map[string]plugin.Artifact{"vendor": {Filename: "Example.pkg", Format: "pkg", Facts: facts}}}
		response, err = plugin.Run(t.Context(), binary, plugin.Request{Operation: "echo.build", Method: "run", Input: raw(t, request)})
		if err != nil {
			t.Fatal(err)
		}
		var output plugin.ResourceResult
		if err := json.Unmarshal(response.Output, &output); err != nil {
			t.Fatal(err)
		}
		subjects := output.Artifacts["installer"].Facts.Subjects
		if output.Artifacts["installer"].Facts.Version != plugin.FactsVersion || len(subjects) != 2 || subjects[0].Package.Version != "4.2" || subjects[1].Parent != "package" || subjects[1].App.Version != "4.1" || subjects[1].App.Build != "402" {
			t.Fatalf("artifact facts lost: %+v", output)
		}
		if _, err := plugin.Run(t.Context(), binary, plugin.Request{Operation: "echo.build", Method: "apply", Input: raw(t, request)}); err == nil {
			t.Fatal("resource accepted undeclared apply method")
		}
	})
	t.Run("plan preserves opaque fields", func(t *testing.T) {
		for _, fields := range []string{"", `null`, `{}`, `{"description":null,"enabled":false,"groups":[],"options":{}}`} {
			request := plugin.ReconcileRequest{Method: "plan", Config: json.RawMessage(fields), Metadata: json.RawMessage(fields)}
			response, err := plugin.Run(t.Context(), binary, reconcileRequest(t, request))
			if err != nil {
				t.Fatal(err)
			}
			result := reconcileResponse(t, response)
			if len(result.Changes) != 2 || string(result.Changes[0].After) != fields || string(result.Changes[1].After) != fields {
				t.Fatalf("presence changed for %q: %+v", fields, result)
			}
		}
	})
	t.Run("apply preserves binding presence and partial failure", func(t *testing.T) {
		for _, bound := range []string{"", `null`, `{"object_id":42}`} {
			for _, fail := range []bool{false, true} {
				request := plugin.ReconcileRequest{Method: "apply", Config: raw(t, map[string]bool{"fail": fail}), Binding: json.RawMessage(bound)}
				response, err := plugin.Run(t.Context(), binary, reconcileRequest(t, request))
				result := reconcileResponse(t, response)
				if (err != nil) != fail || string(result.Binding) != bound {
					t.Fatalf("binding %q, fail=%v: response=%+v error=%v", bound, fail, result, err)
				}
				if fail && (response.Error != "later upload failed" || !strings.Contains(err.Error(), response.Error)) {
					t.Fatalf("handler error lost: response=%+v error=%v", response, err)
				}
			}
		}
	})
	t.Run("authored configuration obeys its declared schema", func(t *testing.T) {
		request := plugin.ReconcileRequest{Method: "validate", Config: json.RawMessage(`{"fail":"yes"}`)}
		if _, err := plugin.Run(t.Context(), binary, reconcileRequest(t, request)); err == nil || !strings.Contains(err.Error(), "config") {
			t.Fatalf("invalid authored configuration error = %v", err)
		}
	})
	t.Run("cancellation stops an active executable", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done()
		}))
		defer server.Close()
		request := plugin.ReconcileRequest{Method: "apply", Config: raw(t, map[string]string{"wait_url": server.URL})}
		if _, err := plugin.Run(ctx, binary, reconcileRequest(t, request)); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	})
	t.Run("cancellation retains a fully written response", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			cancel()
			<-r.Context().Done()
		}))
		defer server.Close()
		t.Setenv("STEMMA_ECHO_RESPONSE", `{"protocol":3,"output":{"binding":{"id":7}}}`)
		t.Setenv("STEMMA_ECHO_WAIT_URL", server.URL)
		response, err := plugin.Run(ctx, binary, plugin.Request{Method: "describe"})
		if !errors.Is(err, context.Canceled) || response.Protocol != plugin.ProtocolVersion || string(response.Output) != `{"binding":{"id":7}}` {
			t.Fatalf("cancellation lost buffered response: response=%+v err=%v", response, err)
		}
	})
	t.Run("request bound", func(t *testing.T) {
		fields := json.RawMessage(`{"padding":"` + strings.Repeat("x", 2<<20) + `"}`)
		request := plugin.ReconcileRequest{Method: "plan", Metadata: fields}
		if _, err := plugin.Run(t.Context(), binary, reconcileRequest(t, request)); err != nil {
			t.Fatalf("bounded message rejected: %v", err)
		}
		request.Config = fields
		if _, err := plugin.Run(t.Context(), binary, reconcileRequest(t, request)); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("oversized request error = %v", err)
		}
	})
	t.Run("reject malformed responses", func(t *testing.T) {
		for _, response := range []string{
			`{"protocol":1,"output":{}}`, `{"protocol":"2"}`, `{"protocol":3,"unknown":true}`, `{"protocol":3}{}`, `null`, `{"protocol":3`,
		} {
			t.Setenv("STEMMA_ECHO_RESPONSE", response)
			if _, err := plugin.Run(t.Context(), binary, plugin.Request{Method: "describe"}); err == nil {
				t.Errorf("accepted response %s", response)
			}
		}
	})
	t.Run("process errors retain partial output without diagnostics", func(t *testing.T) {
		t.Setenv("STEMMA_ECHO_RESPONSE", `{"protocol":3,"output":{"binding":{"id":7}}}`)
		t.Setenv("STEMMA_ECHO_FAIL", "1")
		response, err := plugin.Run(t.Context(), binary, plugin.Request{Method: "describe"})
		if err == nil || string(reconcileResponse(t, response).Binding) != `{"id":7}` || strings.Contains(err.Error(), "credential") {
			t.Fatalf("partial output=%s error=%v", response.Output, err)
		}
	})
}

type notifyingWriter struct {
	bytes.Buffer

	seen chan struct{}
	once sync.Once
}

func (w *notifyingWriter) Write(data []byte) (int, error) {
	n, err := w.Buffer.Write(data)
	if bytes.Contains(data, []byte("Waiting for fixture")) {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

func TestRegistryContracts(t *testing.T) {
	registry := plugin.New("fixture", "1")
	operation := echoOperation("fixture.echo")
	called := 0
	handler := func(_ context.Context, request plugin.Request) (plugin.Response, error) {
		called++
		if request.Method == "validate" {
			return plugin.Response{}, nil
		}
		return plugin.Response{Output: request.Input}, nil
	}
	if err := registry.Register(operation, handler); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(operation, handler); err == nil {
		t.Fatal("duplicate operation replaced original handler")
	}
	if err := registry.Register(echoOperation("fixture.other"), handler); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fixture.echo", "fixture.other"} {
		var out bytes.Buffer
		request := plugin.Request{Protocol: plugin.ProtocolVersion, Operation: name, Method: "run", Input: json.RawMessage(`{"value":7}`)}
		if err := plugin.Serve(t.Context(), bytes.NewReader(raw(t, request)), &out, registry); err != nil {
			t.Fatal(err)
		}
		var response plugin.Response
		if err := json.Unmarshal(out.Bytes(), &response); err != nil || response.Error != "" || string(response.Output) != `{"value":7}` {
			t.Fatalf("response=%s error=%v", out.Bytes(), err)
		}
	}
	if called != 2 {
		t.Fatalf("handler calls = %d", called)
	}
	for _, request := range []plugin.Request{
		{Protocol: 1, Operation: operation.Name, Method: "run", Input: json.RawMessage(`{"value":7}`)},
		{Protocol: plugin.ProtocolVersion, Operation: "missing", Method: "run", Input: json.RawMessage(`{"value":7}`)},
		{Protocol: plugin.ProtocolVersion, Operation: operation.Name, Method: "apply", Input: json.RawMessage(`{"value":7}`)},
		{Protocol: plugin.ProtocolVersion, Operation: operation.Name, Method: "run", Input: json.RawMessage(`{"value":"7"}`)},
		{Protocol: plugin.ProtocolVersion, Operation: operation.Name, Method: "run", Input: json.RawMessage(`{"value":7,"extra":true}`)},
	} {
		if _, err := registry.Handle(t.Context(), request); err == nil {
			t.Errorf("accepted invalid request %+v", request)
		}
	}
	if called != 2 {
		t.Fatalf("invalid request reached handler; calls = %d", called)
	}
	if _, err := registry.Handle(t.Context(), plugin.Request{Protocol: plugin.ProtocolVersion, Operation: operation.Name, Method: "validate", Input: json.RawMessage(`{"value":7}`)}); err != nil {
		t.Fatalf("validation required operation output: %v", err)
	}
	operation.Platforms = []string{"other/processor"}
	operation.Name = "fixture.unavailable"
	if err := registry.Register(operation, handler); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Handle(t.Context(), plugin.Request{Protocol: plugin.ProtocolVersion, Operation: operation.Name, Method: "run", Input: json.RawMessage(`{"value":7}`)}); err == nil || !strings.Contains(err.Error(), "runner") {
		t.Fatalf("unsupported runner error = %v", err)
	}
}

func TestRegistryChecksOutputAndPreservesPartialErrors(t *testing.T) {
	for _, fail := range []bool{false, true} {
		registry := plugin.New("fixture", "1")
		if err := registry.Register(echoOperation("fixture.echo"), func(context.Context, plugin.Request) (plugin.Response, error) {
			response := plugin.Response{Output: json.RawMessage(`{"binding":{"id":7}}`)}
			if fail {
				return response, errors.New("later write failed")
			}
			return response, nil
		}); err != nil {
			t.Fatal(err)
		}
		response, err := registry.Handle(t.Context(), plugin.Request{Protocol: plugin.ProtocolVersion, Operation: "fixture.echo", Method: "run", Input: json.RawMessage(`{"value":7}`)})
		if err == nil {
			t.Fatal("accepted output missing a required contract field")
		}
		if string(response.Output) != `{"binding":{"id":7}}` {
			t.Fatalf("partial failure output lost: %+v", response)
		}
		if !fail && !strings.Contains(err.Error(), "output") {
			t.Fatalf("output contract error = %v", err)
		}
	}
}

func TestRegistryConfigSchema(t *testing.T) {
	registry := plugin.New("fixture", "1")
	operation := echoOperation("fixture.echo")
	operation.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"}},"additionalProperties":false}`)
	operation.InputSchema = json.RawMessage(`true`)
	operation.OutputSchema = json.RawMessage(`true`)
	called := false
	if err := registry.Register(operation, func(_ context.Context, request plugin.Request) (plugin.Response, error) {
		called = true
		return plugin.Response{Output: request.Input}, nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, config := range []string{"", `null`, `{}`, `{"enabled":false}`} {
		request := plugin.Request{Protocol: plugin.ProtocolVersion, Operation: operation.Name, Method: "run", Input: raw(t, struct {
			Config json.RawMessage `json:"config,omitempty"`
		}{json.RawMessage(config)})}
		response, err := registry.Handle(t.Context(), request)
		if err != nil || string(response.Output) != string(request.Input) {
			t.Fatalf("config %q changed: output=%s err=%v", config, response.Output, err)
		}
	}
	called = false
	for _, input := range []string{`{"config":{"enabled":"false"}}`, `{"config":{"unknown":true}}`, `{"config":[]}`, `"not an object"`, `null`} {
		if _, err := registry.Handle(t.Context(), plugin.Request{Protocol: plugin.ProtocolVersion, Operation: operation.Name, Method: "run", Input: json.RawMessage(input)}); err == nil {
			t.Errorf("accepted invalid configuration input %s", input)
		}
	}
	if called {
		t.Fatal("invalid configuration reached the handler")
	}
}

func TestRejectMalformedDescriptors(t *testing.T) {
	for _, mutate := range []func(*plugin.Operation){
		func(op *plugin.Operation) { op.Name = "" },
		func(op *plugin.Operation) { op.Kind = "" },
		func(op *plugin.Operation) { op.SideEffects = "unknown" },
		func(op *plugin.Operation) { op.Methods = nil },
		func(op *plugin.Operation) { op.Methods = []string{"describe"} },
		func(op *plugin.Operation) { op.Methods = []string{"run", "run"} },
		func(op *plugin.Operation) { op.Platforms = []string{"darwin"} },
		func(op *plugin.Operation) { op.InputSchema = json.RawMessage(`{"type":"nonsense"}`) },
		func(op *plugin.Operation) { op.ConfigSchema = json.RawMessage(`{"type":"nonsense"}`) },
		func(op *plugin.Operation) { op.OutputSchema = nil },
	} {
		operation := echoOperation("fixture.echo")
		mutate(&operation)
		if err := plugin.ValidateOperation(operation); err == nil {
			t.Errorf("accepted operation %+v", operation)
		}
	}
	op := echoOperation("fixture.echo")
	if err := plugin.ValidateDescriptor(plugin.Descriptor{Name: "fixture", Version: "1", Operations: []plugin.Operation{op, op}}); err == nil {
		t.Fatal("descriptor accepted duplicate operation names")
	}
	if err := plugin.ValidateDescriptor(plugin.Descriptor{Name: "fixture"}); err == nil {
		t.Fatal("descriptor accepted absent provider version")
	}
}

func TestSchemaValidation(t *testing.T) {
	schema := json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object","properties":{"items":{"type":"array","items":{"$ref":"#/$defs/item"},"minItems":1,"uniqueItems":true}},"required":["items"],"additionalProperties":false,"$defs":{"item":{"type":"integer","minimum":1}}}`)
	if err := plugin.ValidateSchema(schema, json.RawMessage(`{"items":[1,2]}`)); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{`{"items":[0]}`, `{"items":[1,1]}`, `{"items":[]}`, `{"items":["1"]}`, `{}`, `{"items":[1],"extra":true}`, `null`} {
		if err := plugin.ValidateSchema(schema, json.RawMessage(data)); err == nil {
			t.Errorf("accepted %s", data)
		}
	}
	for _, reference := range []string{"https://example.invalid/schema.json", "file:///etc/passwd", "./local.json"} {
		schema := raw(t, map[string]string{"$ref": reference})
		if err := plugin.ValidateSchema(schema, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "external schema resources") {
			t.Errorf("external reference %q error = %v", reference, err)
		}
	}
}

func TestServeRejectsMalformedProtocolBeforeHandler(t *testing.T) {
	for _, input := range []string{
		`{"protocol":1,"method":"describe"}`, `{"protocol":3,"method":"describe","unknown":true}`,
		`{"protocol":3,"method":"describe"}{"protocol":3}`, `{"protocol":3`, `{"protocol":3}`,
		`{"protocol":3,"method":"observe"}`, `{"protocol":"2","method":"describe"}`, `null`, `[]`,
		`{"protocol":3,"method":"run","operation":"fixture.echo"}`, `{"protocol":3,"method":"describe","input":{}}`,
	} {
		t.Run(input, func(t *testing.T) {
			registry := plugin.New("fixture", "1")
			err := plugin.Serve(t.Context(), strings.NewReader(input), new(bytes.Buffer), registry)
			if err == nil {
				t.Fatal("accepted malformed protocol")
			}
		})
	}
}

func TestRunRejectsUnsupportedMethodBeforeStartingExecutable(t *testing.T) {
	for _, method := range []string{"", "observe", "execute"} {
		_, err := plugin.Run(t.Context(), filepath.Join(t.TempDir(), "missing-plugin"), plugin.Request{Method: method})
		if err == nil || !strings.Contains(err.Error(), "method") || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("method %q: %v", method, err)
		}
	}
}

func TestServeBounds(t *testing.T) {
	var out bytes.Buffer
	called := false
	registry := plugin.New("fixture", "1")
	operation := echoOperation("fixture.echo")
	operation.OutputSchema = json.RawMessage(`true`)
	if err := registry.Register(operation, func(context.Context, plugin.Request) (plugin.Response, error) {
		called = true
		return plugin.Response{Output: json.RawMessage(`"` + strings.Repeat("x", 4<<20) + `"`)}, errors.New("partial error")
	}); err != nil {
		t.Fatal(err)
	}
	input := `{"protocol":3,"operation":"fixture.echo","method":"run","input":{"padding":"` + strings.Repeat("x", 4<<20) + `"}}`
	if err := plugin.Serve(t.Context(), strings.NewReader(input), &out, registry); err == nil || !strings.Contains(err.Error(), "size limit") || called {
		t.Fatalf("oversized request: error=%v handler called=%v", err, called)
	}
	input = `{"protocol":3,"operation":"fixture.echo","method":"run","input":{"value":7}}`
	if err := plugin.Serve(t.Context(), strings.NewReader(input), &out, registry); err == nil || !strings.Contains(err.Error(), "size limit") || !called || out.Len() != 0 {
		t.Fatalf("oversized response: error=%v handler called=%v output size=%d", err, called, out.Len())
	}
}

func TestOversizedDiagnosticPreservesResponse(t *testing.T) {
	registry := plugin.New("fixture", "1")
	if err := registry.Register(echoOperation("fixture.echo"), func(ctx context.Context, request plugin.Request) (plugin.Response, error) {
		plugin.Logger(ctx).InfoContext(ctx, strings.Repeat("x", 4<<20))
		return plugin.Response{Output: request.Input}, nil
	}); err != nil {
		t.Fatal(err)
	}
	input := raw(t, plugin.Request{Protocol: plugin.ProtocolVersion, Operation: "fixture.echo", Method: "run", Input: json.RawMessage(`{"value":7}`)})
	var out bytes.Buffer
	if err := plugin.Serve(t.Context(), bytes.NewReader(input), &out, registry); err != nil {
		t.Fatal(err)
	}
	var response plugin.Response
	if err := json.Unmarshal(out.Bytes(), &response); err != nil || string(response.Output) != `{"value":7}` {
		t.Fatalf("diagnostic lost final result: response=%+v error=%v", response, err)
	}
}

func TestRegistryCancellation(t *testing.T) {
	registry := plugin.New("fixture", "1")
	ctx, cancel := context.WithCancel(t.Context())
	if err := registry.Register(echoOperation("fixture.echo"), func(ctx context.Context, _ plugin.Request) (plugin.Response, error) {
		cancel()
		return plugin.Response{Output: json.RawMessage(`{"binding":{"id":7}}`)}, ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	response, err := registry.Handle(ctx, plugin.Request{Protocol: plugin.ProtocolVersion, Operation: "fixture.echo", Method: "run", Input: json.RawMessage(`{"value":7}`)})
	if !errors.Is(err, context.Canceled) || len(response.Output) == 0 {
		t.Fatalf("cancellation lost error or partial output: response=%+v err=%v", response, err)
	}
}

func echoOperation(name string) plugin.Operation {
	schema := json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"],"additionalProperties":false}`)
	return plugin.Operation{Name: name, Kind: "inspect", InputSchema: schema, OutputSchema: schema, SideEffects: "none", Methods: []string{"validate", "run"}}
}

func reconcileRequest(t *testing.T, request plugin.ReconcileRequest) plugin.Request {
	t.Helper()
	return plugin.Request{Operation: "echo.reconcile", Method: request.Method, Input: raw(t, request)}
}

func reconcileResponse(t *testing.T, response plugin.Response) plugin.ReconcileResponse {
	t.Helper()
	var result plugin.ReconcileResponse
	if err := json.Unmarshal(response.Output, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func raw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
