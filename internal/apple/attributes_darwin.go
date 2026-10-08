package apple

import (
	"context"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func nativeSignatureAttributes(ctx context.Context, file *os.File) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	n, err := unix.Flistxattr(int(file.Fd()), nil)
	if errors.Is(err, unix.ENOTSUP) {
		return false, nil
	}
	if err != nil || n == 0 {
		return false, err
	}
	names := make([]byte, n)
	n, err = unix.Flistxattr(int(file.Fd()), names)
	if err != nil {
		return false, err
	}
	for name := range strings.SplitSeq(string(names[:n]), "\x00") {
		if strings.HasPrefix(name, signatureAttribute) {
			return true, nil
		}
	}
	return false, nil
}
