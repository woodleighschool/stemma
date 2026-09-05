// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

package intunewin

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

var zipTime = time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)

// Windows paths must not alias on case-insensitive filesystems, become device
// names, or acquire alternate-stream semantics when Intune installs the payload.
func payloadPath(name string, windows bool) (string, error) {
	if windows {
		name = strings.ReplaceAll(name, `\`, "/")
	}
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, `\:`) {
		return "", fmt.Errorf("unsafe payload path %q", name)
	}
	if len(name) > 240 || strings.Count(name, "/") > 31 {
		return "", fmt.Errorf("payload path exceeds supported length or depth: %q", name)
	}
	for part := range strings.SplitSeq(name, "/") {
		if strings.ContainsAny(part, "<>\"|?*\x00") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return "", fmt.Errorf("unsupported Windows path %q", name)
		}
		for _, r := range part {
			if r < 32 || r > 126 {
				return "", fmt.Errorf("payload paths must use printable ASCII: %q", name)
			}
		}
		stem := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$" ||
			(len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9') {
			return "", fmt.Errorf("windows device path %q", name)
		}
	}
	return name, nil
}

func zipSource(ctx context.Context, sourceDir, setup string, target *os.File) error {
	root, err := os.OpenRoot(sourceDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	zw := zip.NewWriter(&limitedWriter{writer: target, remaining: maxContent})
	var total int64
	seen := map[string]bool{}
	foundSetup := false
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if name == "." {
			return nil
		}
		if _, err := payloadPath(name, false); err != nil {
			return err
		}
		if len(seen) >= maxFiles {
			return errors.New("too many payload entries")
		}
		fold := strings.ToLower(name)
		if seen[fold] {
			return fmt.Errorf("case-conflicting payload path %q", name)
		}
		seen[fold] = true
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("unsupported payload file type %q", name)
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: zipTime}
		if info.IsDir() {
			header.Name += "/"
			header.Method = zip.Store
			header.SetMode(fs.ModeDir | 0o755)
		} else {
			header.SetMode(0o644)
		}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		current, err := file.Stat()
		if err != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) {
			_ = file.Close()
			return fmt.Errorf("source file changed while packaging %q", name)
		}
		n, copyErr := copyContext(ctx, writer, file, maxContent-total)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		total += n
		if name == setup {
			foundSetup = true
		}
		return nil
	})
	if err != nil {
		_ = zw.Close()
		return err
	}
	if !foundSetup {
		_ = zw.Close()
		return errors.New("setup file is not a regular file in the source tree")
	}
	return zw.Close()
}

func inspectPayload(ctx context.Context, file *os.File, size int64, setup string, destination *os.Root) error {
	if err := checkZipDirectory(file, size); err != nil {
		return err
	}
	zr, err := zip.NewReader(file, size)
	if err != nil {
		return err
	}
	if len(zr.File) > maxFiles {
		return errors.New("too many payload entries")
	}
	setup, err = payloadPath(setup, true)
	if err != nil {
		return err
	}
	names := pathSet{spellings: map[string]string{}, directories: map[string]bool{}, explicit: map[string]bool{}}
	foundSetup := false
	var total int64
	for _, entry := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, err := payloadPath(strings.TrimSuffix(entry.Name, "/"), false)
		if err != nil {
			return err
		}
		mode := entry.Mode()
		if !mode.IsRegular() && !mode.IsDir() {
			return fmt.Errorf("unsupported archive file type %q", name)
		}
		if err := names.add(name, mode.IsDir()); err != nil {
			return err
		}
		if entry.UncompressedSize64 > uint64(maxContent-total) {
			return errors.New("payload exceeds size limit")
		}
		if mode.IsDir() {
			if entry.UncompressedSize64 != 0 {
				return errors.New("directory entry contains data")
			}
			if destination != nil {
				if err := destination.MkdirAll(filepath.FromSlash(name), 0o755); err != nil {
					return err
				}
			}
			continue
		}
		input, err := entry.Open()
		if err != nil {
			return err
		}
		output := io.Discard
		var extracted *os.File
		if destination != nil {
			if err := destination.MkdirAll(filepath.FromSlash(path.Dir(name)), 0o755); err != nil {
				_ = input.Close()
				return err
			}
			extracted, err = destination.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
			if err != nil {
				_ = input.Close()
				return err
			}
			output = extracted
		}
		n, copyErr := copyContext(ctx, output, input, maxContent-total)
		closeErr := input.Close()
		if extracted != nil {
			closeErr = errors.Join(closeErr, extracted.Close())
		}
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		total += n
		if name == setup {
			foundSetup = true
		}
	}
	if !foundSetup {
		return errors.New("setup file is missing from payload")
	}
	return nil
}

// Bound central-directory allocation before archive/zip builds its file list.
// This initial subset excludes ZIP64 and multi-volume archives.
func checkZipDirectory(reader io.ReaderAt, size int64) error {
	if size < 22 || size > maxContent+maxMetadata+1<<20 {
		return errors.New("ZIP exceeds supported size")
	}
	n := min(size, int64(65535+22))
	tail := make([]byte, n)
	if _, err := reader.ReadAt(tail, size-n); err != nil {
		return err
	}
	for i := len(tail) - 22; i >= 0; i-- {
		if binary.LittleEndian.Uint32(tail[i:]) != 0x06054b50 || i+22+int(binary.LittleEndian.Uint16(tail[i+20:])) != len(tail) {
			continue
		}
		record := tail[i:]
		count := binary.LittleEndian.Uint16(record[10:])
		directorySize := binary.LittleEndian.Uint32(record[12:])
		offset := binary.LittleEndian.Uint32(record[16:])
		if binary.LittleEndian.Uint16(record[4:]) != 0 || binary.LittleEndian.Uint16(record[6:]) != 0 ||
			binary.LittleEndian.Uint16(record[8:]) != count || count > maxFiles || directorySize > 16<<20 ||
			int64(offset)+int64(directorySize) != size-n+int64(i) {
			return errors.New("unsupported ZIP directory size, layout, or volume")
		}
		return nil
	}
	return errors.New("missing ZIP end record")
}

type pathSet struct {
	spellings   map[string]string
	directories map[string]bool
	explicit    map[string]bool
}

func (p *pathSet) add(name string, directory bool) error {
	key := strings.ToLower(name)
	if p.explicit[key] {
		return fmt.Errorf("duplicate payload path %q", name)
	}
	p.explicit[key] = true
	for current := name; current != "."; current = path.Dir(current) {
		fold := strings.ToLower(current)
		isDir := current != name || directory
		if spelling, exists := p.spellings[fold]; exists && (spelling != current || p.directories[fold] != isDir) {
			return fmt.Errorf("conflicting payload path %q", name)
		}
		p.spellings[fold] = current
		p.directories[fold] = isDir
	}
	return nil
}

type contextReader struct {
	context context.Context
	reader  io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.context.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func copyContext(ctx context.Context, w io.Writer, r io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(w, io.LimitReader(contextReader{ctx, r}, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, errors.New("content exceeds size limit")
	}
	return n, nil
}

type limitedWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("content exceeds size limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
