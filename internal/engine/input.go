package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/source"
)

// InputSelection identifies a named input and an optional path within it.
type InputSelection struct {
	Name string
	Path string
}

// inputOwner prefers a resource's own input; inherited names must be unique.
func inputOwner(plans map[string]resourcePlan, root, name string) (string, error) {
	matches, visited := map[string]bool{}, map[string]bool{}
	var visit func(string)
	visit = func(key string) {
		if visited[key] {
			return
		}
		visited[key] = true
		if _, ok := plans[key].Inputs[name]; ok {
			matches[key] = true
			return
		}
		for _, producer := range producers(plans[key]) {
			visit(producer)
		}
	}
	visit(root)
	keys := sortedKeys(matches)
	switch len(keys) {
	case 0:
		return "", fmt.Errorf("resource and its build dependencies have no input %q", name)
	case 1:
		return keys[0], nil
	default:
		return "", fmt.Errorf("input %q is ambiguous across %s", name, strings.Join(keys, ", "))
	}
}

// withInput leases an input for a read operation. Only a selected resource
// output needs preparation; its consumer's build expressions remain unevaluated.
func (e *execution) withInput(ctx context.Context, consume func(context.Context, Prepared, string, *ResourceReport) error) error {
	key := e.roots[0]
	plan := e.plans[key]
	item := ResourceReport{Name: plan.Resource.Metadata.Name, Kind: plan.Resource.Kind, Key: key}
	ctx = resourceContext(ctx, plan.Resource)
	err := func() error {
		declaration := e.plans[e.inputOwner].Inputs[e.opts.Input.Name]
		if ref := declaration.Resource; ref != nil {
			if err := e.prepare(ctx, ref.Key()); err != nil {
				return err
			}
		}
		var entry source.Entry
		if declaration.Resource == nil {
			var err error
			entry, _, err = e.locked.ReadInput(ctx, e.inputOwner, e.opts.Input.Name)
			if err != nil {
				return err
			}
		}
		input, err := e.input(declaration, entry)
		if err != nil {
			return err
		}
		work, err := os.MkdirTemp(filepath.Join(e.session.store.Dir, "work"), "input-*")
		if err != nil {
			return err
		}
		e.workdirs = append(e.workdirs, work)
		input, err = materialize(ctx, e.session.store, input, filepath.Join(work, "input"))
		if err != nil {
			return err
		}
		return consume(ctx, input, work, &item)
	}()
	if err != nil {
		item.Error = err.Error()
		e.fail(ctx, ResourceError{Resource: key, Err: err})
	}
	e.report.Resources = append(e.report.Resources, item)
	return e.complete(ctx, &e.report.Resources[len(e.report.Resources)-1])
}
