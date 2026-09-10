package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Descriptor identifies one provider and its advertised operation contracts.
type Descriptor struct {
	Name       string      `json:"name"`
	Version    string      `json:"version"`
	Operations []Operation `json:"operations"`
}

// Operation advertises a capability. Empty Platforms means portable; otherwise
// entries are GOOS/GOARCH pairs. SideEffects is none, workspace, or remote.
// Methods contains validate, run, plan, or apply; describe is implicit.
// ConfigSchema constrains authored configuration independently of runtime inputs;
// providers must supply it when configuration is constrained.
// RequiresInspection requests facts for the primary reconciliation artifact.
type Operation struct {
	Name               string          `json:"name"`
	Kind               string          `json:"kind"`
	ConfigSchema       json.RawMessage `json:"config_schema,omitempty"`
	MetadataSchema     json.RawMessage `json:"metadata_schema,omitempty"`
	RequiresInspection bool            `json:"requires_inspection,omitempty"`
	InputSchema        json.RawMessage `json:"input_schema"`
	OutputSchema       json.RawMessage `json:"output_schema"`
	Platforms          []string        `json:"platforms,omitempty"`
	SideEffects        string          `json:"side_effects"`
	Methods            []string        `json:"methods"`
}

// SupportsPlatform reports whether the operation supports this runner.
func (operation Operation) SupportsPlatform(goos, goarch string) bool {
	return len(operation.Platforms) == 0 || slices.Contains(operation.Platforms, goos+"/"+goarch)
}

// SupportsMethod reports whether the operation declares this invocation method.
func (operation Operation) SupportsMethod(method string) bool {
	return slices.Contains(operation.Methods, method)
}

// Registry owns a collision-safe set of operation handlers. Registration and
// dispatch may run concurrently; handlers own their own concurrency requirements.
type Registry struct {
	mu         sync.RWMutex
	name       string
	version    string
	operations map[string]registeredOperation
}

type registeredOperation struct {
	descriptor Operation
	handle     Handler
	config     *jsonschema.Schema
	metadata   *jsonschema.Schema
	input      *jsonschema.Schema
	output     *jsonschema.Schema
}

// New creates a provider registry. Register and Handle reject invalid identities.
func New(name, version string) *Registry {
	return &Registry{name: name, version: version, operations: make(map[string]registeredOperation)}
}

// Register validates an operation and rejects duplicate names, including names
// already registered by a different provider. It takes a copy of the descriptor.
func (registry *Registry) Register(operation Operation, handle Handler) error {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if err := validateIdentity(registry.name, registry.version); err != nil {
		return err
	}
	if _, exists := registry.operations[operation.Name]; exists {
		return fmt.Errorf("operation %q is already registered", operation.Name)
	}
	if handle == nil {
		return fmt.Errorf("operation %q has no handler", operation.Name)
	}
	registered, err := compileOperation(operation)
	if err != nil {
		return err
	}
	registered.handle = handle
	registry.operations[operation.Name] = registered
	return nil
}

// Descriptor returns an independent copy with operations sorted by name.
func (registry *Registry) Descriptor() Descriptor {
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	descriptor := Descriptor{Name: registry.name, Version: registry.version, Operations: make([]Operation, 0, len(registry.operations))}
	for _, operation := range registry.operations {
		descriptor.Operations = append(descriptor.Operations, cloneOperation(operation.descriptor))
	}
	slices.SortFunc(descriptor.Operations, func(a, b Operation) int { return strings.Compare(a.Name, b.Name) })
	return descriptor
}

// Handle checks the protocol, declared method, runner, and input contract before
// dispatch. Successful output is validated; failed operations retain partial output.
// Validation may return no output because it has not produced operation results.
func (registry *Registry) Handle(ctx context.Context, request Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := validateRequest(request); err != nil {
		return Response{}, err
	}
	if request.Method == "describe" {
		descriptor := registry.Descriptor()
		if err := validateIdentity(descriptor.Name, descriptor.Version); err != nil {
			return Response{}, err
		}
		output, err := json.Marshal(descriptor)
		return Response{Protocol: ProtocolVersion, Output: output}, err
	}
	registry.mu.RLock()
	operation, exists := registry.operations[request.Operation]
	registry.mu.RUnlock()
	if !exists {
		return Response{}, fmt.Errorf("operation %q is not registered", request.Operation)
	}
	if !operation.descriptor.SupportsMethod(request.Method) {
		return Response{}, fmt.Errorf("operation %q does not support method %q", request.Operation, request.Method)
	}
	if !operation.descriptor.SupportsPlatform(runtime.GOOS, runtime.GOARCH) {
		return Response{}, fmt.Errorf("operation %q does not support runner %s/%s", request.Operation, runtime.GOOS, runtime.GOARCH)
	}
	if err := validateData(operation.input, request.Input); err != nil {
		return Response{}, fmt.Errorf("operation %q input: %w", request.Operation, err)
	}
	if operation.config != nil {
		var input map[string]json.RawMessage
		if err := json.Unmarshal(request.Input, &input); err != nil || input == nil {
			return Response{}, fmt.Errorf("operation %q configuration requires an object input", request.Operation)
		}
		config := input["config"]
		if len(config) == 0 || bytes.Equal(bytes.TrimSpace(config), []byte("null")) {
			config = json.RawMessage(`{}`)
		}
		if err := validateData(operation.config, config); err != nil {
			return Response{}, fmt.Errorf("operation %q config: %w", request.Operation, err)
		}
	}
	if operation.metadata != nil {
		var input struct {
			Metadata json.RawMessage `json:"metadata"`
		}
		if err := json.Unmarshal(request.Input, &input); err != nil {
			return Response{}, err
		}
		if len(input.Metadata) == 0 || bytes.Equal(bytes.TrimSpace(input.Metadata), []byte("null")) {
			input.Metadata = json.RawMessage(`{}`)
		}
		if err := validateData(operation.metadata, input.Metadata); err != nil {
			return Response{}, fmt.Errorf("operation %q metadata: %w", request.Operation, err)
		}
	}
	response, err := operation.handle(ctx, request)
	response.Protocol = ProtocolVersion
	if err == nil && response.Error != "" {
		err = errors.New(response.Error)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		response.Error = err.Error()
		return response, err
	}
	if request.Method != "validate" || len(response.Output) != 0 {
		if err := validateData(operation.output, response.Output); err != nil {
			return response, fmt.Errorf("operation %q output: %w", request.Operation, err)
		}
	}
	return response, nil
}

// ValidateDescriptor checks a provider's identity, operation contracts, and names.
func ValidateDescriptor(descriptor Descriptor) error {
	if err := validateIdentity(descriptor.Name, descriptor.Version); err != nil {
		return err
	}
	seen := make(map[string]bool, len(descriptor.Operations))
	for _, operation := range descriptor.Operations {
		if seen[operation.Name] {
			return fmt.Errorf("operation %q is advertised more than once", operation.Name)
		}
		seen[operation.Name] = true
		if err := ValidateOperation(operation); err != nil {
			return err
		}
	}
	return nil
}

// ValidateOperation checks a contract without registering or executing a handler.
func ValidateOperation(operation Operation) error {
	_, err := compileOperation(operation)
	return err
}

var operationName = regexp.MustCompile(`^[a-z][a-z0-9]*([._/-][a-z0-9]+)*$`)
var platformName = regexp.MustCompile(`^[a-z][a-z0-9]*/[a-z][a-z0-9]*$`)

// ValidOperationName reports whether name uses lowercase operation namespaces
// separated by dots, underscores, slashes or hyphens.
func ValidOperationName(name string) bool {
	return operationName.MatchString(name)
}

func validateIdentity(name, version string) error {
	if !ValidOperationName(name) {
		return fmt.Errorf("invalid provider name %q", name)
	}
	if version == "" || strings.TrimSpace(version) != version {
		return errors.New("provider version must be nonempty without surrounding whitespace")
	}
	return nil
}

func compileOperation(operation Operation) (registeredOperation, error) {
	if !ValidOperationName(operation.Name) || !ValidOperationName(operation.Kind) {
		return registeredOperation{}, fmt.Errorf("operation %q requires a valid name and kind", operation.Name)
	}
	switch operation.SideEffects {
	case "none", "workspace", "remote":
	default:
		return registeredOperation{}, fmt.Errorf("operation %q has invalid side effects %q", operation.Name, operation.SideEffects)
	}
	if len(operation.Methods) == 0 {
		return registeredOperation{}, fmt.Errorf("operation %q must declare methods", operation.Name)
	}
	for i, method := range operation.Methods {
		if !validMethod(method) || method == "describe" || slices.Contains(operation.Methods[:i], method) {
			return registeredOperation{}, fmt.Errorf("operation %q has invalid or duplicate method %q", operation.Name, method)
		}
	}
	for i, platform := range operation.Platforms {
		if !platformName.MatchString(platform) || slices.Contains(operation.Platforms[:i], platform) {
			return registeredOperation{}, fmt.Errorf("operation %q has invalid or duplicate platform %q", operation.Name, platform)
		}
	}
	var config *jsonschema.Schema
	if len(operation.ConfigSchema) != 0 {
		var err error
		config, err = compileSchema(operation.ConfigSchema)
		if err != nil {
			return registeredOperation{}, fmt.Errorf("operation %q config: %w", operation.Name, err)
		}
	}
	var metadata *jsonschema.Schema
	if len(operation.MetadataSchema) != 0 {
		var err error
		metadata, err = compileSchema(operation.MetadataSchema)
		if err != nil {
			return registeredOperation{}, fmt.Errorf("operation %q metadata: %w", operation.Name, err)
		}
	}
	input, err := compileSchema(operation.InputSchema)
	if err != nil {
		return registeredOperation{}, fmt.Errorf("operation %q input: %w", operation.Name, err)
	}
	output, err := compileSchema(operation.OutputSchema)
	if err != nil {
		return registeredOperation{}, fmt.Errorf("operation %q output: %w", operation.Name, err)
	}
	return registeredOperation{descriptor: cloneOperation(operation), config: config, metadata: metadata, input: input, output: output}, nil
}

func cloneOperation(operation Operation) Operation {
	operation.ConfigSchema = bytes.Clone(operation.ConfigSchema)
	operation.MetadataSchema = bytes.Clone(operation.MetadataSchema)
	operation.InputSchema = bytes.Clone(operation.InputSchema)
	operation.OutputSchema = bytes.Clone(operation.OutputSchema)
	operation.Platforms = slices.Clone(operation.Platforms)
	operation.Methods = slices.Clone(operation.Methods)
	return operation
}

func validMethod(method string) bool {
	switch method {
	case "describe", "validate", "run", "plan", "apply":
		return true
	default:
		return false
	}
}

func validateRequest(request Request) error {
	if request.Protocol != ProtocolVersion {
		return fmt.Errorf("plugin protocol %d is unsupported", request.Protocol)
	}
	if !validMethod(request.Method) {
		return fmt.Errorf("plugin method %q is unsupported", request.Method)
	}
	if request.Method == "describe" {
		if request.Operation != "" || len(request.Input) != 0 {
			return errors.New("describe must omit operation and input")
		}
		return nil
	}
	if !ValidOperationName(request.Operation) {
		return errors.New("plugin request requires a valid operation name")
	}
	if !json.Valid(request.Input) {
		return errors.New("plugin request requires valid JSON input")
	}
	return nil
}
