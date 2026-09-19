package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/invopop/jsonschema"
)

// SchemaFor derives a closed contract from Go's JSON fields. Optional fields use
// omitempty or omitzero; jsonschema tags add constraints, defaults and descriptions.
// Types can use invopop's JSONSchemaExtend hook for enums and semantic annotations.
func SchemaFor[T any]() *jsonschema.Schema {
	r := jsonschema.Reflector{DoNotReference: true}
	schema := r.ReflectFromType(reflect.TypeFor[T]())
	schema.ID = ""
	return schema
}

// Register derives an operation's input, configuration and output contracts from
// its handler. The framework applies defaults and validates configuration before
// calling the handler, including on validate and locked requests.
func Register[I, O any](registry *Registry, operation Operation, handle func(context.Context, I) (O, error)) error {
	if handle == nil {
		return fmt.Errorf("operation %q has no handler", operation.Name)
	}
	input := SchemaFor[I]()
	var err error
	if input.Properties != nil {
		if config, ok := input.Properties.Get("config"); ok {
			operation.ConfigSchema, err = json.Marshal(config)
			if err != nil {
				return err
			}
			// Resource run requests contain prepared config rather than the declaration.
			// ConfigSchema is checked separately at the appropriate lifecycle boundary.
			input.Properties.Set("config", jsonschema.TrueSchema)
		}
	}
	operation.InputSchema, err = json.Marshal(input)
	if err != nil {
		return err
	}
	operation.OutputSchema, err = json.Marshal(SchemaFor[O]())
	if err != nil {
		return err
	}
	return registry.Register(operation, func(ctx context.Context, envelope Request) (Response, error) {
		var request I
		if err := decode(bytes.NewReader(envelope.Input), &request); err != nil {
			return Response{}, fmt.Errorf("operation %s input: %w", operation.Name, err)
		}
		if request, ok := any(&request).(interface{ setMethod(string) }); ok {
			request.setMethod(envelope.Method)
		} else if request, ok := any(request).(interface{ setMethod(string) }); ok {
			request.setMethod(envelope.Method)
		}
		if request, ok := any(request).(interface{ validateConfig() error }); ok {
			if err := request.validateConfig(); err != nil {
				return Response{}, fmt.Errorf("operation %s config: %w", operation.Name, err)
			}
		}
		if operation.Resolver != nil && envelope.Method == "validate" {
			return Response{}, nil
		}
		output, callErr := handle(ctx, request)
		data, encodeErr := json.Marshal(output)
		return Response{Output: data}, errors.Join(callErr, encodeErr)
	})
}

func validateConfig[C any](config C) error {
	if config, ok := any(config).(interface{ Validate() error }); ok {
		return config.Validate()
	}
	if config, ok := any(&config).(interface{ Validate() error }); ok {
		return config.Validate()
	}
	return nil
}
