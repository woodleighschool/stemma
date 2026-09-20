package macsoftware

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/contents"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

type payload struct {
	source      *contents.Source
	node        contents.Node
	local       string
	name        string
	archivePath string
}

func (p *payload) close()                     { _ = p.source.Close() }
func (p *payload) stat() (fs.FileInfo, error) { return p.node.Stat() }
func (p *payload) inspect(ctx context.Context) (plugin.Facts, error) {
	if p.node.Local != "" {
		return inspect.Read(ctx, p.node.Local)
	}
	return inspect.ReadFS(ctx, p.node.FS, p.node.Path)
}
func (p *payload) verifyApp(ctx context.Context, want signature.Signer) (signature.Result, error) {
	if p.node.Local != "" {
		return apple.VerifyApp(ctx, p.node.Local, want)
	}
	return apple.VerifyAppFS(ctx, p.node.FS, p.node.Path, want)
}
func (p *payload) materialize(ctx context.Context, workspace string) (string, error) {
	done := plugin.Stage(ctx, "Extracting package from disk image")
	var err error
	p.local, err = p.node.Materialize(ctx, filepath.Join(workspace, "expanded"))
	done(err)
	return p.local, err
}

func selectPayload(ctx context.Context, spec Spec, input plugin.Artifact, workspace string) (*payload, error) {
	source, err := contents.Open(input, workspace)
	if err != nil {
		return nil, err
	}
	selection := spec.PackagePath
	if selection == "" && spec.Application != nil {
		selection = spec.Application.Path
	}
	if spec.PackagePath != "" && !source.Traversable() {
		_ = source.Close()
		return nil, fmt.Errorf("package_path requires an archive source")
	}
	node, err := source.Select(ctx, selection)
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	p := &payload{source: source, node: node, local: node.Local, name: filepath.Base(node.Path)}
	if source.Traversable() && node.Local != input.Path {
		p.archivePath = node.Path
	}
	return p, nil
}
