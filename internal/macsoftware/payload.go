package macsoftware

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/internal/inspect"
	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

type payload struct {
	local       string
	name        string
	archivePath string
	image       *diskimage.Image
}

func (p *payload) close() {
	if p.image != nil {
		_ = p.image.Close()
	}
}

func (p *payload) stat() (fs.FileInfo, error) {
	if p.image != nil {
		return p.image.Stat(p.name)
	}
	return os.Stat(p.local)
}

func (p *payload) inspect(ctx context.Context) (plugin.Facts, error) {
	if p.local != "" {
		return inspect.Read(ctx, p.local)
	}
	return inspect.ReadFS(ctx, p.image, p.name)
}

// verifyApp verifies the selected application where it lies: inside the open
// disk image, or on disk.
func (p *payload) verifyApp(ctx context.Context, want signature.Signer) (signature.Result, error) {
	if p.image != nil {
		return apple.VerifyAppFS(ctx, p.image, p.name, want)
	}
	return apple.VerifyApp(ctx, p.local, want)
}

// materialize copies a flat package out of a disk image, because the package
// published is a local file. A payload already on disk stays where it is.
func (p *payload) materialize(ctx context.Context, workspace string) (string, error) {
	if p.local != "" {
		return p.local, nil
	}
	done := plugin.Stage(ctx, "Extracting package from disk image")
	var err error
	p.local, err = p.image.Extract(ctx, filepath.Join(workspace, "expanded"), p.name, nil)
	done(err)
	return p.local, err
}

func selectPayload(ctx context.Context, spec Spec, input plugin.Artifact, workspace string) (*payload, error) {
	selection := spec.PackagePath
	if selection == "" && spec.Application != nil {
		selection = spec.Application.Path
	}
	p := &payload{local: input.Path, name: filepath.Base(input.Path)}
	if input.Tree {
		if strings.EqualFold(filepath.Ext(input.Path), ".app") && (selection == "" || selection == ".") {
			return p, nil
		}
		selected, err := archive.Select(input.Path, selection)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(input.Path, selected)
		if err != nil {
			return nil, err
		}
		p.local, p.name, p.archivePath = selected, filepath.Base(selected), filepath.ToSlash(relative)
		return p, nil
	}
	if strings.EqualFold(filepath.Ext(input.Filename), ".dmg") || input.Format == "dmg" {
		image, err := diskimage.Open(ctx, input.Path)
		if err != nil {
			return nil, err
		}
		selected, err := image.Select(ctx, selection)
		if err != nil {
			_ = image.Close()
			return nil, err
		}
		return &payload{image: image, name: selected, archivePath: selected}, nil
	}
	if isArchive(input.Filename) {
		expanded := filepath.Join(workspace, "expanded")
		done := plugin.Stage(ctx, "Extracting archive")
		err := archive.Extract(ctx, input.Path, expanded)
		done(err)
		if err != nil {
			return nil, err
		}
		selected, err := archive.Select(expanded, selection)
		if err != nil {
			return nil, err
		}
		relative, err := filepath.Rel(expanded, selected)
		if err != nil {
			return nil, err
		}
		p.local, p.name, p.archivePath = selected, filepath.Base(selected), filepath.ToSlash(relative)
		return p, nil
	}
	if spec.PackagePath != "" {
		return nil, fmt.Errorf("package_path requires an archive source")
	}
	return p, nil
}
