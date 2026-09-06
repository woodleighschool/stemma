package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const messageLimit = 4 << 20

// Serve handles exactly one bounded JSON request and writes one response. Handler
// errors are encoded with partial output. Plugins reserve stdout for the protocol.
func Serve(ctx context.Context, in io.Reader, out io.Writer, registry *Registry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var request Request
	if err := decode(in, &request); err != nil {
		return fmt.Errorf("plugin request: %w", err)
	}
	if err := validateRequest(request); err != nil {
		return err
	}
	response, err := registry.Handle(ctx, request)
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

// Run invokes an explicitly selected trusted executable without a shell or a
// sandbox. A zero request protocol selects ProtocolVersion; other versions fail.
// Cancellation ends the process. Partial output survives operation/process errors.
func Run(ctx context.Context, executable string, request Request) (Response, error) {
	if request.Protocol == 0 {
		request.Protocol = ProtocolVersion
	}
	if err := validateRequest(request); err != nil {
		return Response{}, err
	}
	data, err := json.Marshal(request)
	if err != nil {
		return Response{}, fmt.Errorf("plugin request: %w", err)
	}
	if len(data) > messageLimit {
		return Response{}, errors.New("plugin request exceeds size limit")
	}
	command := exec.CommandContext(ctx, executable)
	command.Stdin = bytes.NewReader(data)
	command.WaitDelay = time.Second
	var stdout limitedBuffer
	command.Stdout = &stdout
	// Connection diagnostics can contain credentials and must not enter errors or state.
	command.Stderr = io.Discard
	processErr := command.Run()
	var response Response
	if err := decode(bytes.NewReader(stdout.Bytes()), &response); err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		if processErr != nil {
			return Response{}, fmt.Errorf("plugin process: %w", processErr)
		}
		return Response{}, fmt.Errorf("plugin response: %w", err)
	}
	if response.Protocol != ProtocolVersion {
		return Response{}, fmt.Errorf("plugin response protocol %d is unsupported", response.Protocol)
	}
	if ctx.Err() != nil {
		return response, ctx.Err()
	}
	if response.Error != "" {
		return response, fmt.Errorf("plugin: %s", response.Error)
	}
	if processErr != nil {
		return response, fmt.Errorf("plugin process: %w", processErr)
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
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return errors.New("message must contain exactly one JSON object")
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
