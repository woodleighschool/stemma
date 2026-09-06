// Package plugin defines Stemma's versioned executable destination protocol.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// ProtocolVersion is the protocol understood by this SDK.
const ProtocolVersion = 1

const messageLimit = 4 << 20

// Identity identifies a logical destination independently of its display metadata.
type Identity struct {
	Project     string `json:"project"`
	Recipe      string `json:"recipe"`
	Destination string `json:"destination"`
}

// Artifact is an immutable file leased by the engine, never a writable CAS object.
type Artifact struct {
	Path     string `json:"path"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	Filename string `json:"filename"`
	Version  string `json:"version,omitempty"`
}

// Request contains one plan or apply operation. Raw JSON retains absent, null and concrete fields.
// Config contains connection settings, whose credentials belong to the plugin.
type Request struct {
	Protocol int             `json:"protocol"`
	Method   string          `json:"method"`
	Identity Identity        `json:"identity"`
	Config   json.RawMessage `json:"config,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Artifact Artifact        `json:"artifact"`
	Binding  json.RawMessage `json:"binding,omitempty"`
}

// Change is a semantic destination change; empty Changes means no write is needed.
type Change struct {
	Kind   string          `json:"kind"`
	Field  string          `json:"field"`
	Action string          `json:"action"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
}

// Response carries a plan or the bindings recovered after application.
// On apply, Binding updates durable engine state: omission preserves it, null
// clears it, and a value replaces it, even when application returns an error.
type Response struct {
	Protocol int             `json:"protocol"`
	Changes  []Change        `json:"changes,omitempty"`
	Binding  json.RawMessage `json:"binding,omitempty"`
	Error    string          `json:"error,omitempty"`
}

// Handler implements one operation. Each invocation runs in a fresh process.
type Handler func(context.Context, Request) (Response, error)

// Serve handles exactly one bounded JSON request and writes exactly one response.
// Plugins reserve stdout for this protocol and send diagnostic logging to stderr.
func Serve(ctx context.Context, in io.Reader, out io.Writer, handle Handler) error {
	var request Request
	if err := decode(in, &request); err != nil {
		return fmt.Errorf("plugin request: %w", err)
	}
	if request.Protocol != ProtocolVersion {
		return fmt.Errorf("plugin protocol %d is unsupported", request.Protocol)
	}
	if request.Method != "plan" && request.Method != "apply" {
		return fmt.Errorf("plugin method %q is unsupported", request.Method)
	}
	response, err := handle(ctx, request)
	response.Protocol = ProtocolVersion
	if err != nil {
		response.Error = err.Error()
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return fmt.Errorf("plugin response: %w", err)
	}
	if len(encoded) > messageLimit {
		return errors.New("plugin response exceeds size limit")
	}
	_, err = out.Write(append(encoded, '\n'))
	return err
}

// Run starts an explicitly selected executable without a shell. Cancellation ends
// that process; callers own the artifact lease and persist bindings from apply
// responses, including partial bindings returned alongside an error.
func Run(ctx context.Context, executable string, request Request) (Response, error) {
	if request.Method != "plan" && request.Method != "apply" {
		return Response{}, fmt.Errorf("plugin method %q is unsupported", request.Method)
	}
	request.Protocol = ProtocolVersion
	data, err := json.Marshal(request)
	if err != nil {
		return Response{}, fmt.Errorf("plugin request: %w", err)
	}
	if len(data) > messageLimit {
		return Response{}, errors.New("plugin request exceeds size limit")
	}
	command := exec.CommandContext(ctx, executable)
	command.Stdin = bytes.NewReader(data)
	var stdout limitedBuffer
	command.Stdout = &stdout
	// Diagnostics may contain connection credentials. They are deliberately not
	// copied into engine errors or durable state.
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, fmt.Errorf("plugin process: %w", err)
	}
	var response Response
	if err := decode(bytes.NewReader(stdout.Bytes()), &response); err != nil {
		return Response{}, fmt.Errorf("plugin response: %w", err)
	}
	if response.Protocol != ProtocolVersion {
		return Response{}, fmt.Errorf("plugin response protocol %d is unsupported", response.Protocol)
	}
	if response.Error != "" {
		return response, fmt.Errorf("plugin: %s", response.Error)
	}
	return response, nil
}

func decode(reader io.Reader, value any) error {
	data, err := io.ReadAll(io.LimitReader(reader, messageLimit+1))
	if err != nil {
		return err
	}
	if len(data) > messageLimit {
		return errors.New("message exceeds size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("message must contain exactly one JSON object")
	}
	return nil
}

type limitedBuffer struct{ bytes.Buffer }

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	if buffer.Len()+len(data) > messageLimit {
		return 0, errors.New("plugin response exceeds size limit")
	}
	return buffer.Buffer.Write(data)
}
