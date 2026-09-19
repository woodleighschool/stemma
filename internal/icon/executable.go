package icon

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// StageExecutable marks an isolated rendering bundle as native software without
// copying or following its vendor executable. The Mach-O header has no code or
// entry point; Quick Look needs its architecture and executable file type to
// avoid the prohibited badge. Existing files are never replaced.
func StageExecutable(bundle, name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:\x00") {
		return fmt.Errorf("unsafe CFBundleExecutable %q", name)
	}
	var cpu, subtype uint32
	switch runtime.GOARCH {
	case "arm64":
		cpu = 0x0100000c
	case "amd64":
		cpu, subtype = 0x01000007, 3
	default:
		return errors.New("icon rendering requires an arm64 or amd64 host")
	}
	var header []byte
	for _, value := range []uint32{0xfeedfacf, cpu, subtype, 2, 0, 0, 0x200085, 0} {
		header = binary.LittleEndian.AppendUint32(header, value)
	}
	root, err := os.OpenRoot(bundle)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.MkdirAll("Contents/MacOS", 0o755); err != nil {
		return err
	}
	f, err := root.OpenFile(filepath.Join("Contents", "MacOS", name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = f.Write(header)
	return errors.Join(err, f.Close())
}
