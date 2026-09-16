package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"sync"
	"time"
)

const messageLimit = 4 << 20

// Serve handles one bounded JSON request, streaming log records followed by one
// response. Handler errors retain partial output. Stdout belongs to the protocol.
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
	stream := &responseStream{out: out}
	ctx = WithLogger(ctx, slog.New(slog.NewJSONHandler(stream, &slog.HandlerOptions{Level: request.LogLevel})))
	response, err := registry.Handle(ctx, request)
	response.Protocol = ProtocolVersion
	if err != nil {
		response.Error = err.Error()
	}
	return stream.send(wireResponse{Response: response})
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
	request.LogLevel = slog.LevelError
	for _, level := range []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError} {
		if Logger(ctx).Enabled(ctx, level) {
			request.LogLevel = level
			break
		}
	}
	data, err := json.Marshal(request)
	if err != nil {
		return Response{}, fmt.Errorf("plugin request: %w", err)
	}
	if len(data) > messageLimit {
		return Response{}, errors.New("plugin request exceeds size limit")
	}
	processCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(processCtx, executable)
	command.Stdin = bytes.NewReader(data)
	command.WaitDelay = time.Second
	stdout := &responseReader{ctx: ctx, cancel: cancel}
	command.Stdout = stdout
	// Connection diagnostics can contain credentials and must not enter errors or state.
	command.Stderr = io.Discard
	processErr := command.Run()
	if stdout.pending.Len() > 0 && stdout.err == nil {
		stdout.err = stdout.accept(stdout.pending.Bytes())
	}
	response := stdout.response
	if stdout.err != nil || !stdout.finished {
		if ctx.Err() != nil {
			return response, ctx.Err()
		}
		if stdout.err != nil {
			return response, fmt.Errorf("plugin response: %w", stdout.err)
		}
		if processErr != nil {
			return response, fmt.Errorf("plugin process: %w", processErr)
		}
		return response, errors.New("plugin response is missing")
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

type wireResponse struct {
	Response

	Log json.RawMessage `json:"log,omitempty"`
}

// responseStream serializes logs from concurrent handlers with the final result.
type responseStream struct {
	mu  sync.Mutex
	out io.Writer
	err error
}

func (s *responseStream) send(response wireResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	data, err := json.Marshal(response)
	if err == nil && len(data) > messageLimit {
		err = errors.New("plugin response exceeds size limit")
	}
	if err == nil {
		_, err = s.out.Write(append(data, '\n'))
		// Only an I/O failure prevents the final result; rejected logs do not.
		s.err = err
	}
	return err
}

func (s *responseStream) Write(data []byte) (int, error) {
	err := s.send(wireResponse{Protocol: ProtocolVersion, Log: data})
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

type responseReader struct {
	ctx      context.Context
	cancel   context.CancelFunc
	pending  bytes.Buffer
	response Response
	finished bool
	err      error
}

func (r *responseReader) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 && r.err == nil {
		line, rest, complete := bytes.Cut(data, []byte{'\n'})
		if r.pending.Len()+len(line) > messageLimit {
			r.err = errors.New("plugin response exceeds size limit")
			break
		}
		_, _ = r.pending.Write(line)
		if complete {
			r.err = r.accept(r.pending.Bytes())
			r.pending.Reset()
		}
		data = rest
	}
	if r.err != nil {
		r.cancel()
		return 0, r.err
	}
	return n, nil
}

func (r *responseReader) accept(data []byte) error {
	if r.finished {
		return errors.New("message received after final response")
	}
	var message wireResponse
	if err := decode(bytes.NewReader(data), &message); err != nil {
		return err
	}
	if message.Protocol != ProtocolVersion {
		return fmt.Errorf("protocol %d is unsupported", message.Protocol)
	}
	if len(message.Log) == 0 {
		r.response, r.finished = message.Response, true
		return nil
	}
	if len(message.Output) != 0 || message.Error != "" {
		return errors.New("log message contains a result")
	}
	var record struct {
		Time    time.Time  `json:"time"`
		Level   slog.Level `json:"level"`
		Message string     `json:"msg"`
	}
	if err := json.Unmarshal(message.Log, &record); err != nil {
		return fmt.Errorf("log record: %w", err)
	}
	if record.Time.IsZero() || record.Message == "" {
		return errors.New("log record requires time and message")
	}
	var fields map[string]any
	decoder := json.NewDecoder(bytes.NewReader(message.Log))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return err
	}
	delete(fields, "time")
	delete(fields, "level")
	delete(fields, "msg")
	attrs := make([]slog.Attr, 0, len(fields))
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		attrs = append(attrs, slog.Any(key, logValue(key, fields[key])))
	}
	Logger(r.ctx).LogAttrs(r.ctx, record.Level, record.Message, attrs...)
	return nil
}

// logValue restores the integer and duration types that JSON erases, so plugin
// records read like the host's own.
func logValue(key string, value any) any {
	number, ok := value.(json.Number)
	if !ok {
		return value
	}
	if n, err := number.Int64(); err == nil {
		if key == "elapsed" {
			// Stage durations are encoded as nanoseconds.
			return time.Duration(n)
		}
		return n
	}
	f, _ := number.Float64()
	return f
}
