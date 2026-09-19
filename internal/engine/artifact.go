package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

func (o *operations) check(name string, preparation bool) error {
	op, err := o.operation(name)
	if err != nil {
		return err
	}
	methods := []string{"validate", "plan", "apply"}
	if preparation {
		methods = []string{"validate", "run"}
		if op.SideEffects == "remote" {
			return fmt.Errorf("operation %s declares remote effects and cannot run during preparation", name)
		}
	}
	for _, method := range methods {
		if !op.SupportsMethod(method) {
			return fmt.Errorf("operation %s must support %s", name, method)
		}
	}
	if !op.SupportsPlatform(runtime.GOOS, runtime.GOARCH) {
		return fmt.Errorf("operation %s does not support %s/%s", name, runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

func (p Prepared) artifact() plugin.Artifact {
	return plugin.Artifact{Mode: p.Mode, Path: p.Path, SHA256: p.Payload.SHA256, Size: p.Payload.Size, Filename: p.Filename, Format: p.Format, Version: p.Version, Tree: p.Tree, Facts: p.Facts, EntryPoint: p.EntryPoint, Evidence: p.Evidence}
}

func importPath(ctx context.Context, store *cas.Store, path string, tree bool, work string) (cas.Ref, error) {
	if !tree {
		file, err := os.Open(path)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = file.Close() }()
		info, err := file.Stat()
		if err != nil {
			return cas.Ref{}, err
		}
		if !info.Mode().IsRegular() {
			return cas.Ref{}, errors.New("artifact must be a regular file or tree")
		}
		return store.Import(ctx, file, "")
	}
	packed, err := os.CreateTemp(work, "tree-*.tar")
	if err != nil {
		return cas.Ref{}, err
	}
	defer func() { _ = os.Remove(packed.Name()) }()
	err = archive.Pack(ctx, path, packed)
	err = errors.Join(err, packed.Close())
	if err != nil {
		return cas.Ref{}, err
	}
	return store.ImportFile(ctx, packed.Name(), "")
}

var outputName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func safeOutputName(name string) bool {
	return outputName.MatchString(name) && name != "." && name != ".."
}
func safeFilename(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:") && strings.IndexFunc(name, unicode.IsControl) < 0
}
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && filepath.IsLocal(rel)
}

func validateFacts(facts plugin.Facts) error {
	if facts.Version == 0 && len(facts.Subjects) == 0 {
		return nil
	}
	if facts.Version != plugin.FactsVersion {
		return errors.New("unsupported artifact facts version")
	}
	ids := map[string]bool{}
	for _, subject := range facts.Subjects {
		if subject.ID == "" || ids[subject.ID] || subject.Kind == "" {
			return errors.New("artifact facts require unique subject IDs and kinds")
		}
		if subject.Path != "" && (!filepath.IsLocal(filepath.FromSlash(subject.Path)) || strings.Contains(subject.Path, "\\")) {
			return errors.New("artifact subject path must be relative to its artifact")
		}
		if subject.InstalledPath != "" && !strings.HasPrefix(subject.InstalledPath, "/") {
			return errors.New("installed subject path must be absolute")
		}
		ids[subject.ID] = true
	}
	for _, subject := range facts.Subjects {
		if subject.Parent != "" && (!ids[subject.Parent] || subject.Parent == subject.ID) {
			return errors.New("artifact subject has an invalid parent")
		}
	}
	return nil
}
