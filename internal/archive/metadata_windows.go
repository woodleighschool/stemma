package archive

import (
	"encoding/binary"
	"errors"
	"os"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

func checkMetadata(f *os.File, fileInfo os.FileInfo) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks > 1 {
		return errors.New("hard links are unsupported")
	}
	if info.FileAttributes&(windows.FILE_ATTRIBUTE_ENCRYPTED|windows.FILE_ATTRIBUTE_COMPRESSED) != 0 {
		return errors.New("special Windows file attributes are unsupported")
	}
	// Go identifies ordinary symbolic links separately from other reparse tags.
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && fileInfo.Mode()&os.ModeSymlink == 0 {
		return errors.New("Windows reparse points other than symlinks are unsupported")
	}
	// FILE_STREAM_INFO entries expose named streams that a portable TAR would lose.
	var storage struct {
		_       uint64 // FILE_STREAM_INFO requires eight-byte alignment.
		streams [65536]byte
	}
	streams := storage.streams[:]
	if err := windows.GetFileInformationByHandleEx(windows.Handle(f.Fd()), windows.FileStreamInfo, &streams[0], uint32(len(streams))); err != nil {
		if errors.Is(err, windows.ERROR_HANDLE_EOF) {
			return nil
		}
		return err
	}
	for offset := uint32(0); ; {
		if offset > uint32(len(streams))-24 {
			return errors.New("invalid Windows stream information")
		}
		next := binary.LittleEndian.Uint32(streams[offset:])
		length := binary.LittleEndian.Uint32(streams[offset+4:])
		if length%2 != 0 || length > uint32(len(streams))-offset-24 {
			return errors.New("invalid Windows stream name")
		}
		name := make([]uint16, length/2)
		for i := range name {
			name[i] = binary.LittleEndian.Uint16(streams[offset+24+uint32(i)*2:])
		}
		if string(utf16.Decode(name)) != "::$DATA" {
			return errors.New("Windows alternate data streams are unsupported")
		}
		if next == 0 {
			break
		}
		if next < 24+length || next%8 != 0 || next > uint32(len(streams))-offset {
			return errors.New("invalid Windows stream offset")
		}
		offset += next
	}
	// Windows ACLs are host access controls, not macOS install ACLs. Package
	// ownership remains the explicitly declared root:wheel policy.
	return nil
}

func checkSymlinkMetadata(root *os.Root, name string, info os.FileInfo) error {
	f, err := root.OpenFile(name, os.O_RDONLY|windows.O_FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	actual, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(info, actual) {
		return errors.New("symlink changed during metadata inspection")
	}
	return CheckMetadata(f, actual)
}
