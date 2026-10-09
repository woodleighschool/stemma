package apple

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

// VerifySubject checks an application or package in an opened input.
// Callers define the scope; component receipts are never signing subjects.
func VerifySubject(ctx context.Context, source *contents.Source, subject plugin.Subject, workspace string) (signature.Result, error) {
	if subject.Kind != "app" && subject.Kind != "container" {
		return signature.Result{}, fmt.Errorf("%q is not an application or package signing subject", subject.Path)
	}
	selection := subject.Path
	if selection == "." && source.Artifact().ContentRoot == "" {
		selection = ""
	}
	node, err := source.At(ctx, selection)
	if err != nil {
		return signature.Result{}, err
	}
	if subject.Kind == "app" {
		return VerifyAppFS(ctx, node.FS, node.Path, signature.Signer{})
	}
	local := node.Local
	if local == "" {
		stage, err := os.MkdirTemp(workspace, ".verify-package-")
		if err != nil {
			return signature.Result{}, err
		}
		defer func() { _ = os.RemoveAll(stage) }()
		local, err = node.Materialize(ctx, filepath.Join(stage, "package"))
		if err != nil {
			return signature.Result{}, err
		}
	}
	return VerifyPackage(ctx, local, signature.Signer{})
}
