package engine

import (
	"context"

	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/plugin"
)

// BuiltinSchema describes built-in operations without loading a project or plugins.
func BuiltinSchema() ([]byte, error) {
	operations, err := builtins(nil)
	if err != nil {
		return nil, err
	}
	return config.Schema(operations.registry.Descriptor())
}

// ProjectSchema describes trusted operations without acquiring software inputs
// or contacting destinations. Offline requires verified cached plugin bundles.
func ProjectSchema(ctx context.Context, opts Options) (result []byte, err error) {
	project, err := config.LoadSchemaProject(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	done := plugin.Stage(ctx, "Generating project schema")
	defer func() { done(err) }()
	operations, cleanup, err := projectOperations(ctx, project, opts)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	// The schema covers every operation, so it waits for all of them.
	if err := operations.complete(); err != nil {
		return nil, err
	}
	return config.ProjectSchema(project, operations.registry.Descriptor())
}
