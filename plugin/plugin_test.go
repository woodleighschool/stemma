package plugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	t.Run("plan preserves opaque fields", func(t *testing.T) {
		for _, fields := range []string{"", `null`, `{}`, `{"description":null,"enabled":false,"groups":[],"options":{}}`} {
			request := plugin.Request{Method: "plan", Config: json.RawMessage(fields), Metadata: json.RawMessage(fields)}
			response, err := plugin.Run(t.Context(), binary, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Changes) != 2 || string(response.Changes[0].After) != fields || string(response.Changes[1].After) != fields {
				t.Fatalf("presence changed for %q: %+v", fields, response)
			}
		}
	})
	t.Run("apply preserves binding presence and partial failure", func(t *testing.T) {
		for _, bound := range []string{"", `null`, `{"object_id":42}`} {
			for _, fail := range []bool{false, true} {
				config, err := json.Marshal(map[string]bool{"fail": fail})
				if err != nil {
					t.Fatal(err)
				}
				response, err := plugin.Run(t.Context(), binary, plugin.Request{Method: "apply", Config: config, Binding: json.RawMessage(bound)})
				if (err != nil) != fail || string(response.Binding) != bound {
					t.Fatalf("binding %q, fail=%v: response=%+v error=%v", bound, fail, response, err)
				}
				if fail && (response.Error != "later upload failed" || !strings.Contains(err.Error(), response.Error)) {
					t.Fatalf("handler error lost: response=%+v error=%v", response, err)
				}
			}
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
		config, err := json.Marshal(map[string]string{"wait_url": server.URL})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := plugin.Run(ctx, binary, plugin.Request{Method: "apply", Config: config}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
	})
	t.Run("request bound", func(t *testing.T) {
		fields := json.RawMessage(`{"padding":"` + strings.Repeat("x", 2<<20) + `"}`)
		if _, err := plugin.Run(t.Context(), binary, plugin.Request{Method: "plan", Metadata: fields}); err != nil {
			t.Fatalf("bounded message rejected: %v", err)
		}
		if _, err := plugin.Run(t.Context(), binary, plugin.Request{Method: "plan", Config: fields, Metadata: fields}); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("oversized request error = %v", err)
		}
	})
}

func TestServeRejectsMalformedProtocolBeforeHandler(t *testing.T) {
	for _, input := range []string{
		`{"protocol":2,"method":"plan"}`,
		`{"protocol":1,"method":"plan","unknown":true}`,
		`{"protocol":1,"method":"plan"}{"protocol":1}`,
		`{"protocol":1`,
		`{"protocol":1}`,
		`{"protocol":1,"method":"validate"}`,
		`{"protocol":1,"method":"describe"}`,
		`{"protocol":1,"method":"observe"}`,
	} {
		t.Run(input, func(t *testing.T) {
			called := false
			err := plugin.Serve(t.Context(), bytes.NewBufferString(input), new(bytes.Buffer), func(context.Context, plugin.Request) (plugin.Response, error) {
				called = true
				return plugin.Response{}, nil
			})
			if err == nil || called {
				t.Fatalf("err=%v handler called=%v", err, called)
			}
		})
	}
}

func TestRunRejectsUnsupportedMethodBeforeStartingExecutable(t *testing.T) {
	for _, method := range []string{"", "validate", "describe", "observe"} {
		_, err := plugin.Run(t.Context(), filepath.Join(t.TempDir(), "missing-plugin"), plugin.Request{Method: method})
		if err == nil || !strings.Contains(err.Error(), "method") || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("method %q: %v", method, err)
		}
	}
}

func TestServeBounds(t *testing.T) {
	var out bytes.Buffer
	called := false
	handle := func(context.Context, plugin.Request) (plugin.Response, error) {
		called = true
		return plugin.Response{Binding: json.RawMessage(`"` + strings.Repeat("x", 4<<20) + `"`)}, nil
	}
	input := `{"protocol":1,"method":"plan","metadata":{"padding":"` + strings.Repeat("x", 4<<20) + `"}}`
	if err := plugin.Serve(t.Context(), strings.NewReader(input), &out, handle); err == nil || !strings.Contains(err.Error(), "size limit") || called {
		t.Fatalf("oversized request: error=%v handler called=%v", err, called)
	}
	if err := plugin.Serve(t.Context(), strings.NewReader(`{"protocol":1,"method":"apply"}`), &out, handle); err == nil || !strings.Contains(err.Error(), "size limit") || !called || out.Len() != 0 {
		t.Fatalf("oversized response: error=%v handler called=%v output size=%d", err, called, out.Len())
	}
}
