package archive

import (
	"encoding/binary"
	"errors"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func checkSymlinkMetadata(root *os.Root, name string, info os.FileInfo) error {
	f, err := root.OpenFile(name, os.O_RDONLY|unix.O_SYMLINK, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return CheckMetadata(f, info)
}

func checkACL(f *os.File, info os.FileInfo) error {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		flags := stat.Flags
		if info.Mode().IsRegular() {
			// Compression and document tracking describe host storage, not payload policy.
			flags &^= unix.UF_COMPRESSED | unix.UF_TRACKED
		}
		if flags != 0 {
			return errors.New("BSD file flags are unsupported")
		}
	}
	// fgetattrlist returns an attrreference for ATTR_CMN_EXTENDED_SECURITY.
	// A nonempty security descriptor is outside the ordinary-file package subset.
	attrs := struct {
		Count, Reserved                       uint16
		Common, Volume, Directory, File, Fork uint32
	}{Count: 5, Common: 0x00400000}
	var result [65536]byte
	_, _, errno := syscall.Syscall6(syscall.SYS_FGETATTRLIST, f.Fd(), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&result[0])), uintptr(len(result)), 0, 0)
	if errno != 0 {
		return errno
	}
	size := binary.LittleEndian.Uint32(result[:4])
	if size < 12 || size > uint32(len(result)) {
		return errors.New("invalid file security attributes")
	}
	if binary.LittleEndian.Uint32(result[8:12]) != 0 {
		return errors.New("ACLs and extended file security are unsupported")
	}
	return nil
}

func ignoreXattr(name string, info os.FileInfo) bool {
	switch name {
	case "com.apple.provenance", "com.apple.quarantine", "com.apple.metadata:kMDItemWhereFroms":
		return true
	case "com.apple.decmpfs":
		// Normal reads return logical bytes, including when a materialized file
		// retains this attribute. Active compression hides its backing resource
		// fork from ordinary xattr listings; a visible resource fork is still rejected.
		return info.Mode().IsRegular()
	default:
		return false
	}
}
