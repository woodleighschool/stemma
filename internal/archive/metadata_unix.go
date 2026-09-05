//go:build darwin || linux

package archive

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

func checkMetadata(f *os.File, info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && !info.IsDir() && stat.Nlink > 1 {
		return errors.New("hard links are unsupported")
	}
	count, err := unix.Flistxattr(int(f.Fd()), nil)
	if err != nil {
		return err
	}
	if count > 1<<20 {
		return errors.New("extended attribute list exceeds limit")
	}
	names := make([]byte, count)
	count, err = unix.Flistxattr(int(f.Fd()), names)
	if err != nil {
		return err
	}
	for name := range bytes.SplitSeq(names[:count], []byte{0}) {
		if len(name) == 0 || runtime.GOOS == "darwin" && string(name) == "com.apple.provenance" {
			continue
		}
		return fmt.Errorf("extended attribute %q is unsupported", name)
	}
	return checkACL(f, info)
}
