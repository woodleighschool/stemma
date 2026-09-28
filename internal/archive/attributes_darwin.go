package archive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// CheckXattrs rejects native code signatures that a byte-only copy would lose.
func CheckXattrs(ctx context.Context, file *os.File) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n, err := unix.Flistxattr(int(file.Fd()), nil)
	if errors.Is(err, unix.ENOTSUP) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read extended attributes: %w", err)
	}
	if n == 0 {
		return nil
	}
	names := make([]byte, n)
	n, err = unix.Flistxattr(int(file.Fd()), names)
	if err != nil {
		return fmt.Errorf("read extended attributes: %w", err)
	}
	for name := range strings.SplitSeq(string(names[:n]), "\x00") {
		if strings.HasPrefix(name, codeSignature) {
			return fmt.Errorf("%s keeps its code signature in extended attributes, which copying would discard", file.Name())
		}
	}
	return nil
}
