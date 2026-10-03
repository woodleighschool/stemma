package engine

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/plugin"
)

func (e *execution) inspectInput(ctx context.Context) error {
	return e.withInput(ctx, func(ctx context.Context, input Prepared, work string, _ *ResourceReport) error {
		result, err := inspectInput(ctx, input.artifact(), e.opts.Input.Path, filepath.Join(work, "inspection"))
		if err == nil {
			e.report.Inspection = &result
		}
		return err
	})
}

// Inspect inventories a local artifact using the same facts as input expressions.
func Inspect(ctx context.Context, name, selection string) (result Inspection, err error) {
	done := plugin.Stage(ctx, "Inspecting artifact", plugin.Detail(filepath.Base(name)))
	defer func() { done(err) }()
	name, err = filepath.Abs(name)
	if err != nil {
		return Inspection{}, err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return Inspection{}, err
	}
	work, err := os.MkdirTemp("", "stemma-inspect-*")
	if err != nil {
		return Inspection{}, err
	}
	defer func() { _ = os.RemoveAll(work) }()
	return inspectInput(ctx, plugin.Artifact{Path: name, Filename: filepath.Base(name), Tree: info.IsDir()}, selection, work)
}

func inspectInput(ctx context.Context, input plugin.Artifact, selection, work string) (Inspection, error) {
	if err := os.MkdirAll(work, 0o700); err != nil {
		return Inspection{}, err
	}
	source, err := contents.Open(ctx, input, work)
	if err != nil {
		return Inspection{}, err
	}
	defer func() { _ = source.Close() }()
	facts, err := inspect.Source(ctx, source)
	if err != nil {
		return Inspection{}, err
	}
	name := input.Filename
	if selection != "" && selection != "." {
		if !fs.ValidPath(selection) || strings.ContainsAny(selection, "\\\x00\r\n\t") {
			return Inspection{}, fmt.Errorf("invalid subject path %q", selection)
		}
		var selected []plugin.Subject
		var available []string
		for _, subject := range facts.Subjects {
			available = append(available, subject.ID)
			if subject.ID == selection || strings.HasPrefix(subject.ID, selection+"/") {
				selected = append(selected, subject)
			}
		}
		if len(selected) == 0 {
			return Inspection{}, fmt.Errorf("no facts at %q; available subject IDs: %s", selection, inspect.FormatSubjectIDs(available))
		}
		facts.Subjects = selected
		name = filepath.Base(selection)
	}
	return Inspection{Filename: name, Format: artifactFormat(name, facts), Version: artifactVersion(facts), Facts: facts}, nil
}
