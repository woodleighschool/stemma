package plugin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/woodleighschool/stemma/plugin"
)

func TestExecutableProtocolPreservesPresenceAndCancellation(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "echo")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	command := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./testdata/echo")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, output)
	}
	metadata := json.RawMessage(`{"description":null,"unattended_install":false,"blocking_applications":[],"targets":{}}`)
	response, err := plugin.Run(t.Context(), binary, plugin.Request{Method: "observe", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(response.Observation, metadata) {
		t.Fatalf("presence changed: %s", response.Observation)
	}
	bound := json.RawMessage(`{"software_id":42}`)
	partial, err := plugin.Run(t.Context(), binary, plugin.Request{Method: "partial", Binding: bound})
	if err == nil || !bytes.Equal(partial.Binding, bound) {
		t.Fatalf("partial binding lost: response=%+v error=%v", partial, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := plugin.Run(ctx, binary, plugin.Request{Method: "wait"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestServeRejectsMalformedProtocolBeforeHandler(t *testing.T) {
	for _, input := range []string{`{"protocol":2}`, `{"protocol":1,"unknown":true}`, `{"protocol":1}{"protocol":1}`, `{"protocol":1`} {
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
