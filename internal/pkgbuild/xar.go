// Portions adapted from Fleet.
// Copyright (c) 2020-present Fleet Device Management Inc
// Copyright (c) 2017 Kolide
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

package pkgbuild

import (
	"bytes"
	"compress/zlib"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/woodleighschool/stemma/internal/fileio"
)

type xarMember struct {
	Modified string  `xml:"mtime"`
	ID       int     `xml:"id,attr"`
	Name     string  `xml:"name"`
	Type     string  `xml:"type"`
	Mode     string  `xml:"mode"`
	Data     xarData `xml:"data"`
}
type xarData struct {
	Length    int64     `xml:"length"`
	Offset    int64     `xml:"offset"`
	Size      int64     `xml:"size"`
	Encoding  xarStyle  `xml:"encoding"`
	Archived  xarDigest `xml:"archived-checksum"`
	Extracted xarDigest `xml:"extracted-checksum"`
}
type xarStyle struct {
	Style string `xml:"style,attr"`
}
type xarDigest struct {
	Style  string `xml:"style,attr"`
	Digest string `xml:",chardata"`
}
type xarDocument struct {
	XMLName xml.Name `xml:"xar"`
	TOC     xarTOC   `xml:"toc"`
}
type xarTOC struct {
	Created  string         `xml:"creation-time"`
	Checksum xarTOCChecksum `xml:"checksum"`
	Files    []xarMember    `xml:"file"`
}
type xarTOCChecksum struct {
	Style  string `xml:"style,attr"`
	Offset int64  `xml:"offset"`
	Size   int64  `xml:"size"`
}

// writeXar stores the component members without another compression layer.
// Member bytes stream from private files; only the small TOC is held in memory.
func writeXar(ctx context.Context, directory, output string, generated time.Time) error {
	document := xarDocument{TOC: xarTOC{Created: generated.Format("2006-01-02T15:04:05"), Checksum: xarTOCChecksum{Style: "sha1", Size: 20}}}
	offset := int64(20)
	for _, name := range []string{"Bom", "PackageInfo", "Payload", "Scripts"} {
		f, err := os.Open(filepath.Join(directory, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		digest := sha1.New()
		size, err := io.Copy(digest, fileio.Reader{Context: ctx, Reader: f})
		_ = f.Close()
		if err != nil {
			return err
		}
		checksum := xarDigest{Style: "sha1", Digest: hex.EncodeToString(digest.Sum(nil))}
		document.TOC.Files = append(document.TOC.Files, xarMember{Modified: generated.Format(time.RFC3339), ID: len(document.TOC.Files) + 1, Name: name, Type: "file", Mode: "0644", Data: xarData{Length: size, Size: size, Offset: offset, Encoding: xarStyle{Style: "application/octet-stream"}, Archived: checksum, Extracted: checksum}})
		offset += size
	}
	toc, err := xml.Marshal(document)
	if err != nil {
		return err
	}
	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	if _, err = zw.Write(toc); err != nil {
		return err
	}
	if err = zw.Close(); err != nil {
		return err
	}
	f, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	for _, value := range []any{uint32(0x78617221), uint16(28), uint16(1), uint64(compressed.Len()), uint64(len(toc)), uint32(1)} {
		if err := binary.Write(f, binary.BigEndian, value); err != nil {
			return err
		}
	}
	if _, err = f.Write(compressed.Bytes()); err != nil {
		return err
	}
	checksum := sha1.Sum(compressed.Bytes())
	if _, err = f.Write(checksum[:]); err != nil {
		return err
	}
	for _, member := range document.TOC.Files {
		source, err := os.Open(filepath.Join(directory, member.Name))
		if err != nil {
			return err
		}
		n, err := io.Copy(f, fileio.Reader{Context: ctx, Reader: source})
		_ = source.Close()
		if err != nil {
			return err
		}
		if n != member.Data.Size {
			return fmt.Errorf("package member %s changed during assembly", member.Name)
		}
	}
	if err := f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
