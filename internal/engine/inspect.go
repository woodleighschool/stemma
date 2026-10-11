package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/plugin"
)

func inspectPrepared(ctx context.Context, store *cas.Store, artifact Prepared) (plugin.Facts, error) {
	if artifact.Path != "" {
		return inspect.Read(ctx, artifact.Path)
	}
	work, err := os.MkdirTemp(filepath.Join(store.Dir, "work"), "inspection-*")
	if err != nil {
		return plugin.Facts{}, err
	}
	defer func() { cleanupError(ctx, os.RemoveAll(work)) }()
	artifact, err = materialize(ctx, store, artifact, work)
	if err != nil {
		return plugin.Facts{}, err
	}
	return inspect.Read(ctx, artifact.Path)
}

func (e *execution) inspectInput(ctx context.Context) error {
	return e.withInput(ctx, func(ctx context.Context, input Prepared, work string, _ *ResourceReport) error {
		if e.opts.Read != "" {
			return e.readStored(ctx, input, filepath.Join(work, "read"))
		}
		input, err := materialize(ctx, e.session.store, input, filepath.Join(work, "input"))
		if err != nil {
			return err
		}
		result, err := inspectInput(ctx, input.artifact(), e.opts.Input.Path, filepath.Join(work, "inspection"))
		if err == nil {
			e.report.Inspection = &result
		}
		return err
	})
}

// readStored writes the file the run names inside a stored artifact. The
// object is verified and then read where it is, so nothing is materialized.
func (e *execution) readStored(ctx context.Context, stored Prepared, work string) (err error) {
	done := plugin.Stage(ctx, "Reading artifact", plugin.Detail(stored.Filename))
	defer func() { done(err) }()
	store := e.session.store
	if err := store.Verify(ctx, stored.Payload); err != nil {
		return err
	}
	artifact := stored.artifact()
	if artifact.Path, err = store.Path(stored.Payload); err != nil {
		return err
	}
	if stored.Tree {
		artifact.Encoding = "tar"
	}
	return readMember(ctx, artifact, e.opts.Read, work, e.opts.ReadTo)
}

// ReadMember writes one file inside a local artifact to w, or the names a
// directory inside it holds.
func ReadMember(ctx context.Context, name, member string, w io.Writer) (err error) {
	done := plugin.Stage(ctx, "Reading artifact", plugin.Detail(filepath.Base(name)))
	defer func() { done(err) }()
	name, err = filepath.Abs(name)
	if err != nil {
		return err
	}
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "stemma-inspect-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(work) }()
	return readMember(ctx, plugin.Artifact{Path: name, Filename: filepath.Base(name), Tree: info.IsDir()}, member, work, w)
}

func readMember(ctx context.Context, artifact plugin.Artifact, member, work string, w io.Writer) (err error) {
	if err := os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	source, err := contents.Open(ctx, artifact, work)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, source.Close()) }()
	return inspect.Member(ctx, source, member, w)
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
