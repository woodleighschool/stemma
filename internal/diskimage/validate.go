package diskimage

import (
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/deploymenttheory/go-apfs-v2/pkg/disk"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
	"github.com/woodleighschool/stemma/internal/fileio"
	"howett.net/plist"
)

const maxChunkSize = 64 << 20

type block struct {
	Name   string `plist:"Name"`
	CFName string `plist:"CFName"`
	Data   []byte `plist:"Data"`
}

// decodeDMG validates chunk bounds while writing the single filesystem to a
// sparse file. Zero-fill extents consume disk address space, not memory.
func decodeDMG(ctx context.Context, filename string, filesystem *os.File) error {
	f, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 512 || info.Size() > maxBytes {
		return errors.New("invalid disk image size")
	}
	var footer disk.DMGFooter
	if err := binary.Read(io.NewSectionReader(f, info.Size()-512, 512), binary.BigEndian, &footer); err != nil {
		return err
	}
	if string(footer.Signature[:]) != "koly" || footer.Version != 4 || footer.HeaderSize != 512 {
		return errors.New("not a supported UDIF disk image")
	}
	if (footer.SegmentCount != footer.SegmentNumber || footer.SegmentCount > 1) || footer.RsrcForkLength != 0 {
		return errors.New("segmented and resource-fork disk images are unsupported")
	}
	end := uint64(info.Size() - 512)
	if footer.SectorCount == 0 || footer.SectorCount > uint64(maxBytes/512) || !within(footer.DataForkOffset, footer.DataForkLength, end) || !within(footer.PlistOffset, footer.PlistLength, end) || footer.PlistLength == 0 || footer.PlistLength > 16<<20 {
		return errors.New("disk image footer exceeds size limits")
	}
	var doc struct {
		ResourceFork struct {
			Blocks []block `plist:"blkx"`
		} `plist:"resource-fork"`
	}
	if err := plist.NewDecoder(io.NewSectionReader(f, int64(footer.PlistOffset), int64(footer.PlistLength))).Decode(&doc); err != nil {
		return err
	}
	if len(doc.ResourceFork.Blocks) == 0 || len(doc.ResourceFork.Blocks) > 256 {
		return errors.New("invalid disk image partition count")
	}
	seen := map[string]bool{}
	var gpt disk.GPTHeader
	var tableBytes uint64
	filesystems := 0
	for _, partition := range doc.ResourceFork.Blocks {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := partition.Name
		if name == "" {
			name = partition.CFName
		}
		if seen[name] {
			return errors.New("duplicate disk image partition name")
		}
		seen[name] = true
		isFilesystem := strings.Contains(name, "Apple_HFS") || strings.Contains(name, "Apple_APFS") || name == "disk image" || name == "(disk image)"
		if isFilesystem {
			filesystems++
			if filesystems > 1 {
				return errors.New("disk image has multiple filesystems; exactly one is required")
			}
		}
		data := partition.Data
		if len(data) < 204 || string(data[:4]) != "mish" {
			return errors.New("invalid disk image block table")
		}
		start := binary.BigEndian.Uint64(data[8:])
		sectors := binary.BigEndian.Uint64(data[16:])
		offset := binary.BigEndian.Uint64(data[24:])
		count := binary.BigEndian.Uint32(data[200:])
		if !within(start, sectors, footer.SectorCount) || offset > footer.DataForkLength || count == 0 || count > maxEntries || uint64(count)*40 != uint64(len(data)-204) {
			return errors.New("disk image block table exceeds bounds")
		}
		isHeader := name == "GPT Header (Primary GPT Header : 1)"
		isTable := name == "GPT Partition Data (Primary GPT Table : 2)"
		if (isHeader || isTable) && sectors > (1<<20)/512 {
			return errors.New("GPT metadata exceeds size limit")
		}
		if isTable {
			tableBytes = sectors * 512
		}
		var header bytes.Buffer
		var previousEnd uint64
		for i := range count {
			var chunk disk.DMGChunk
			if err := binary.Read(bytes.NewReader(data[204+i*40:244+i*40]), binary.BigEndian, &chunk); err != nil {
				return err
			}
			if chunk.Type == 0x7ffffffe || chunk.Type == 0xffffffff {
				if chunk.DiskLength != 0 || chunk.CompressedLength != 0 {
					return errors.New("invalid disk image marker chunk")
				}
				continue
			}
			if chunk.DiskLength == 0 || chunk.DiskOffset != previousEnd || !within(chunk.DiskOffset, chunk.DiskLength, sectors) || !within(chunk.CompressedOffset, chunk.CompressedLength, footer.DataForkLength-offset) {
				return errors.New("disk image chunk exceeds bounds or leaves a gap")
			}
			previousEnd = chunk.DiskOffset + chunk.DiskLength
			chunk.DiskLength *= 512
			chunk.CompressedOffset += offset + footer.DataForkOffset
			if chunk.CompressedLength > maxChunkSize {
				return errors.New("compressed disk image chunk exceeds size limit")
			}
			if chunk.Type == 0 || chunk.Type == 2 {
				if chunk.CompressedLength != 0 {
					return errors.New("zero-fill disk image chunk contains compressed data")
				}
				if isHeader {
					return errors.New("GPT header cannot be zero-filled")
				}
				if isFilesystem {
					if _, err := filesystem.Seek(int64(chunk.DiskLength), io.SeekCurrent); err != nil {
						return err
					}
				}
				continue
			}
			if chunk.DiskLength > maxChunkSize {
				return errors.New("expanded disk image chunk exceeds size limit")
			}
			output := io.Discard
			if isFilesystem {
				output = filesystem
			}
			if isHeader {
				output = &header
			}
			if err := validateChunk(ctx, f, chunk, output); err != nil {
				return err
			}
		}
		if previousEnd != sectors {
			return errors.New("disk image chunks do not cover the partition")
		}
		if isFilesystem {
			if err := filesystem.Truncate(int64(sectors * 512)); err != nil {
				return err
			}
		}
		if isHeader {
			if err := binary.Read(&header, binary.LittleEndian, &gpt); err != nil {
				return err
			}
			if err := gpt.Verify(); err != nil {
				return err
			}
			if gpt.EntriesCount == 0 || gpt.EntriesCount > 256 || gpt.EntriesSize != 128 {
				return errors.New("GPT partition count exceeds size limit")
			}
		}
	}
	if filesystems != 1 {
		return fmt.Errorf("disk image has %d filesystems; exactly one is required", filesystems)
	}
	if gpt.EntriesCount != 0 && uint64(gpt.EntriesCount)*uint64(gpt.EntriesSize) > tableBytes {
		return errors.New("truncated GPT partition table")
	}
	return nil
}

func within(offset, length, end uint64) bool { return offset <= end && length <= end-offset }

func validateChunk(ctx context.Context, image io.ReaderAt, chunk disk.DMGChunk, output io.Writer) error {
	compressed := io.NewSectionReader(image, int64(chunk.CompressedOffset), int64(chunk.CompressedLength))
	var reader io.Reader
	switch chunk.Type {
	case 1:
		if chunk.CompressedLength != chunk.DiskLength {
			return errors.New("uncompressed disk image chunk length mismatch")
		}
		reader = compressed
	case 0x80000004:
		data := make([]byte, chunk.CompressedLength)
		if _, err := io.ReadFull(compressed, data); err != nil {
			return err
		}
		decoded, err := disk.DecompressADC(data, int(chunk.DiskLength))
		if err != nil {
			return err
		}
		reader = bytes.NewReader(decoded)
	case 0x80000005:
		stream, err := zlib.NewReader(compressed)
		if err != nil {
			return err
		}
		defer func() { _ = stream.Close() }()
		reader = stream
	case 0x80000006:
		reader = bzip2.NewReader(compressed)
	case 0x80000008:
		buffered := bufio.NewReader(compressed)
		header, err := buffered.Peek(13)
		if err != nil {
			return err
		}
		if bytes.HasPrefix(header, []byte{0xfd, '7', 'z', 'X', 'Z', 0}) {
			reader, err = (xz.ReaderConfig{DictCap: maxChunkSize}).NewReader(buffered)
		} else {
			if binary.LittleEndian.Uint32(header[1:5]) > maxChunkSize {
				return errors.New("LZMA dictionary exceeds size limit")
			}
			reader, err = lzma.NewReader(buffered)
		}
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported disk image compression 0x%08x", chunk.Type)
	}
	n, err := io.Copy(output, io.LimitReader(fileio.Reader{Context: ctx, Reader: reader}, int64(chunk.DiskLength)+1))
	if err != nil {
		return err
	}
	if n != int64(chunk.DiskLength) {
		return errors.New("decompressed disk image chunk length mismatch")
	}
	return nil
}
