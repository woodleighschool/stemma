package pkgbuild

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/deploymenttheory/go-macos-pkg/pkg/xar"
	"github.com/woodleighschool/stemma/internal/fileio"
)

func writeXar(ctx context.Context, directory, output string, generated time.Time) error {
	out, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	writer, err := xar.NewWriter(fileio.Writer{Context: ctx, Writer: out}, xar.WriterOptions{CreationTime: generated, TempDir: directory})
	if err != nil {
		return err
	}
	defer func() { _ = writer.Close() }()
	for _, name := range []string{"Bom", "PackageInfo", "Payload", "Scripts"} {
		file, err := os.Open(filepath.Join(directory, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		err = writer.AddFile(name, xar.FileHeader{Mode: 0o644}, xar.EncodingNone, fileio.Reader{Context: ctx, Reader: file})
		_ = file.Close()
		if err != nil {
			return err
		}
	}
	if err := writer.Close(); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	return out.Close()
}
