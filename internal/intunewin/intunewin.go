// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

package intunewin

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Write packages regular files and directories with portable Windows paths.
// SetupFile may be source-relative or absolute. OutputPath must be outside the
// source tree and must not exist. Temporary files are removed on failure.
// Payload identity ignores source timestamps and normalizes Windows file modes.
func Write(ctx context.Context, sourceDir, setupFile, outputPath string) (Metadata, error) {
	var m Metadata
	if err := ctx.Err(); err != nil {
		return m, err
	}
	source, err := filepath.Abs(sourceDir)
	if err != nil {
		return m, err
	}
	source, err = filepath.EvalSymlinks(source)
	if err != nil {
		return m, err
	}
	setup := setupFile
	if filepath.IsAbs(setup) {
		setup, err = filepath.Rel(source, setup)
		if err != nil {
			return m, err
		}
	}
	setup, err = payloadPath(filepath.ToSlash(setup), false)
	if err != nil {
		return m, err
	}
	output, err := filepath.Abs(outputPath)
	if err != nil {
		return m, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return m, fmt.Errorf("output directory must exist: %w", err)
	}
	output = filepath.Join(parent, filepath.Base(output))
	rel, err := filepath.Rel(source, output)
	if err != nil {
		return m, err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return m, errors.New("output must be outside source tree")
	}
	if _, err := os.Lstat(output); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = os.ErrExist
		}
		return m, fmt.Errorf("output path: %w", err)
	}
	workspace, err := os.MkdirTemp(parent, ".intunewin-")
	if err != nil {
		return m, err
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	plain, err := os.Create(filepath.Join(workspace, "payload.zip"))
	if err != nil {
		return m, err
	}
	defer func() { _ = plain.Close() }()
	if err := zipSource(ctx, source, setup, plain); err != nil {
		return m, err
	}
	if _, err := plain.Seek(0, io.SeekStart); err != nil {
		return m, err
	}
	encrypted, err := os.Create(filepath.Join(workspace, "encrypted"))
	if err != nil {
		return m, err
	}
	defer func() { _ = encrypted.Close() }()
	m.Name = filepath.Base(setup)
	m.SetupFile = strings.ReplaceAll(setup, "/", `\`)
	if err := encrypt(ctx, plain, encrypted, &m); err != nil {
		return Metadata{}, err
	}
	data, err := marshalDetection(m)
	if err != nil {
		return Metadata{}, err
	}
	envelope, err := os.Create(filepath.Join(workspace, "envelope"))
	if err != nil {
		return Metadata{}, err
	}
	defer func() { _ = envelope.Close() }()
	if err := outerZip(ctx, envelope, encrypted, data); err != nil {
		return Metadata{}, err
	}
	if err := envelope.Sync(); err != nil {
		return Metadata{}, err
	}
	if err := envelope.Close(); err != nil {
		return Metadata{}, err
	}
	if err := ctx.Err(); err != nil {
		return Metadata{}, err
	}
	// Link atomically publishes a complete file without replacing a concurrent writer.
	if err := os.Link(envelope.Name(), output); err != nil {
		return Metadata{}, fmt.Errorf("publish envelope: %w", err)
	}
	return m, nil
}

func outerZip(ctx context.Context, output io.Writer, encrypted *os.File, data []byte) error {
	if _, err := encrypted.Seek(0, io.SeekStart); err != nil {
		return err
	}
	zw := zip.NewWriter(output)
	writer, err := zw.CreateHeader(&zip.FileHeader{Name: contentEntry, Method: zip.Store, Modified: zipTime})
	if err != nil {
		return err
	}
	if _, err := copyContext(ctx, writer, encrypted, maxContent+64); err != nil {
		_ = zw.Close()
		return err
	}
	writer, err = zw.CreateHeader(&zip.FileHeader{Name: metadataEntry, Method: zip.Deflate, Modified: zipTime})
	if err != nil {
		_ = zw.Close()
		return err
	}
	if _, err := writer.Write(data); err != nil {
		_ = zw.Close()
		return err
	}
	return zw.Close()
}

// Inspect authenticates and decrypts the envelope, validates the payload paths,
// reads every payload member to verify ZIP integrity, and checks the setup file.
// It does not execute the installer or verify its publisher signature.
func Inspect(path string) (Metadata, error) {
	return inspect(context.Background(), path, nil)
}

// Extract verifies the entire package before publishing its regular-file tree.
// DestinationDir must not exist, and its parent must exist. No symlinks, device
// paths, traversal paths, or case-conflicting names are accepted.
func Extract(ctx context.Context, path, destinationDir string) error {
	destination, err := filepath.Abs(destinationDir)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(destination); !errors.Is(err, os.ErrNotExist) {
		if err == nil {
			err = os.ErrExist
		}
		return err
	}
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".intunewin-extract-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(stage) }()
	root, err := os.OpenRoot(stage)
	if err != nil {
		return err
	}
	_, readErr := inspect(ctx, path, root)
	closeErr := root.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return publishDirectory(stage, destination)
}

func inspect(ctx context.Context, path string, destination *os.Root) (Metadata, error) {
	var m Metadata
	if err := ctx.Err(); err != nil {
		return m, err
	}
	file, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return m, err
	}
	if err := checkZipDirectory(file, info.Size()); err != nil {
		return m, err
	}
	container, err := zip.NewReader(file, info.Size())
	if err != nil {
		return m, err
	}
	var metadata, payload *zip.File
	if len(container.File) > maxFiles {
		return m, errors.New("too many envelope entries")
	}
	for _, entry := range container.File {
		switch entry.Name {
		case metadataEntry:
			if metadata != nil {
				return m, errors.New("duplicate Detection.xml")
			}
			metadata = entry
		case contentEntry:
			if payload != nil {
				return m, errors.New("duplicate encrypted payload")
			}
			payload = entry
		}
	}
	if metadata == nil || payload == nil {
		return m, errors.New("missing envelope metadata or payload")
	}
	if metadata.UncompressedSize64 > uint64(maxMetadata) || payload.UncompressedSize64 > uint64(maxContent+64) {
		return m, errors.New("envelope exceeds size limit")
	}
	reader, err := metadata.Open()
	if err != nil {
		return m, err
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxMetadata+1))
	closeErr := reader.Close()
	if err != nil {
		return m, err
	}
	if closeErr != nil {
		return m, closeErr
	}
	if int64(len(data)) > maxMetadata {
		return m, errors.New("detection.xml exceeds size limit")
	}
	m, err = parseDetection(data)
	if err != nil {
		return Metadata{}, err
	}
	workspace, err := os.MkdirTemp("", "stemma-intunewin-")
	if err != nil {
		return Metadata{}, err
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	encrypted, err := os.Create(filepath.Join(workspace, "encrypted"))
	if err != nil {
		return Metadata{}, err
	}
	defer func() { _ = encrypted.Close() }()
	reader, err = payload.Open()
	if err != nil {
		return Metadata{}, err
	}
	_, copyErr := copyContext(ctx, encrypted, reader, maxContent+64)
	closeErr = reader.Close()
	if copyErr != nil {
		return Metadata{}, copyErr
	}
	if closeErr != nil {
		return Metadata{}, closeErr
	}
	if _, err := encrypted.Seek(0, io.SeekStart); err != nil {
		return Metadata{}, err
	}
	plain, err := os.Create(filepath.Join(workspace, "payload.zip"))
	if err != nil {
		return Metadata{}, err
	}
	defer func() { _ = plain.Close() }()
	if err := decrypt(ctx, encrypted, plain, &m); err != nil {
		return Metadata{}, err
	}
	if err := inspectPayload(ctx, plain, m.PlaintextSize, m.SetupFile, destination); err != nil {
		return Metadata{}, err
	}
	return m, nil
}
