package engine

import (
	"context"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/inspect"
)

func (e *execution) inspectInput(ctx context.Context) error {
	key := e.roots[0]
	e.pending[key] = 1
	if err := e.prepare(ctx, key); err != nil {
		return err
	}
	prepared := e.prepared[key]
	if !prepared.ready {
		return nil
	}
	item := &e.report.Resources[prepared.report]
	input, err := materialize(ctx, e.session.store, prepared.inputs[e.opts.Input], filepath.Join(prepared.work, "inspection-input"))
	if err == nil {
		var result Inspection
		result, err = inspectInput(ctx, input, e.opts.InputPath, filepath.Join(prepared.work, "inspection"))
		if err == nil {
			e.report.Inspection = &result
		}
	}
	if err != nil {
		item.Error = err.Error()
		e.fail(ctx, ResourceError{Resource: key, Err: err})
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.complete(ctx, item)
}

func inspectInput(ctx context.Context, input Prepared, selection, work string) (Inspection, error) {
	if err := os.MkdirAll(work, 0o700); err != nil {
		return Inspection{}, err
	}
	source, err := contents.Open(ctx, input.artifact(), work)
	if err != nil {
		return Inspection{}, err
	}
	defer func() { _ = source.Close() }()
	facts, err := inspect.Selection(ctx, source, selection)
	if err != nil {
		return Inspection{}, err
	}
	name := input.Filename
	if selection != "" && selection != "." {
		name = filepath.Base(selection)
	}
	return Inspection{Filename: name, Format: artifactFormat(name, facts), Version: artifactVersion(facts), Facts: facts}, nil
}
