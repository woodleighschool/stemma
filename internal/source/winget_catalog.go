package source

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/url"
	"path"
	"strings"
	"sync"

	_ "github.com/ncruces/go-sqlite3/driver" // Registers the CGO-free SQLite driver for the immutable source index.
	sqliteio "github.com/ncruces/go-sqlite3/util/ioutil"
	"github.com/ncruces/go-sqlite3/vfs/readervfs"
)

const wingetURL = "https://cdn.winget.microsoft.com/cache/"

type wingetPackage struct {
	ID     string
	Latest string
	SHA256 string
}

// A manager uses one source snapshot so multiple inputs cannot observe different releases mid-run.
type wingetCatalog struct {
	mu       sync.Mutex
	packages map[string]wingetPackage
}

func (m *Manager) wingetPackage(ctx context.Context, id string) (wingetPackage, error) {
	m.winget.mu.Lock()
	loaded := m.winget.packages != nil
	m.winget.mu.Unlock()
	if !loaded {
		data, err := m.metadataBytes(ctx, wingetURL+"source2.msix")
		if err != nil {
			return wingetPackage{}, fmt.Errorf("winget source: %w", err)
		}
		m.winget.mu.Lock()
		if m.winget.packages == nil {
			var packages map[string]wingetPackage
			packages, err = readWingetCatalog(ctx, data)
			if err == nil {
				m.winget.packages = packages
			}
		}
		m.winget.mu.Unlock()
		if err != nil {
			return wingetPackage{}, fmt.Errorf("winget index: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return wingetPackage{}, err
	}
	m.winget.mu.Lock()
	p, ok := m.winget.packages[strings.ToLower(id)]
	m.winget.mu.Unlock()
	if !ok {
		return p, fmt.Errorf("winget package %q is not in the source", id)
	}
	return p, nil
}

func readWingetCatalog(ctx context.Context, data []byte) (map[string]wingetPackage, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	file, err := archive.Open("Public/index.db")
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err = io.ReadAll(io.LimitReader(file, metadataLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > metadataLimit {
		return nil, errors.New("expanded index exceeds 64 MiB")
	}
	name := "winget-" + rand.Text()
	readervfs.Create(name, sqliteio.NewSizeReaderAt(bytes.NewReader(data)))
	defer readervfs.Delete(name)
	db, err := sql.Open("sqlite3", "file:"+name+"?vfs=reader&mode=ro")
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	var major, minor string
	if err := db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE name = 'majorVersion'`).Scan(&major); err != nil {
		return nil, err
	}
	if err := db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE name = 'minorVersion'`).Scan(&minor); err != nil {
		return nil, err
	}
	if major != "2" || minor != "0" {
		return nil, fmt.Errorf("unsupported source index version %s.%s", major, minor)
	}
	rows, err := db.QueryContext(ctx, "SELECT id, latest_version, hash FROM packages")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	packages := make(map[string]wingetPackage)
	for rows.Next() {
		var p wingetPackage
		var digest []byte
		if err := rows.Scan(&p.ID, &p.Latest, &digest); err != nil {
			return nil, err
		}
		if len(digest) != sha256.Size || p.Latest == "" || !wingetIdentifier.MatchString(p.ID) {
			return nil, errors.New("source index contains an invalid package identity")
		}
		p.SHA256 = hex.EncodeToString(digest)
		if _, exists := packages[strings.ToLower(p.ID)]; exists {
			return nil, fmt.Errorf("source index repeats package %q", p.ID)
		}
		packages[strings.ToLower(p.ID)] = p
	}
	return packages, rows.Err()
}

func (m *Manager) wingetMetadata(ctx context.Context, relative, digest string) ([]byte, error) {
	if !validDigest(digest) || !safeRelative(relative) || path.Clean(relative) != relative {
		return nil, errors.New("winget metadata has an invalid path or digest")
	}
	key, err := fingerprint(struct {
		Kind   string `json:"kind"`
		SHA256 string `json:"sha256"`
	}{"winget.metadata/1", digest})
	if err != nil {
		return nil, err
	}
	matches := func(data []byte) bool {
		sum := sha256.Sum256(data)
		return len(data) <= metadataLimit && hex.EncodeToString(sum[:]) == digest
	}
	var data []byte
	if m.Store.RecallSource(key, &data) && matches(data) {
		return data, nil
	}
	data, err = m.metadataBytes(ctx, (&url.URL{Scheme: "https", Host: "cdn.winget.microsoft.com", Path: "/cache/" + relative}).String())
	if err != nil {
		return nil, err
	}
	if !matches(data) {
		return nil, errors.New("winget metadata SHA256 mismatch")
	}
	if err := m.Store.RememberSource(key, data); err != nil {
		return nil, err
	}
	return data, nil
}

// Windows Compression API buffer mode frames MSZIP blocks separately from CAB.
// The standard library owns DEFLATE; each block reuses the previous 32 KiB dictionary.
func decompressWinget(data []byte) ([]byte, error) {
	if len(data) < 24 || !bytes.Equal(data[:6], []byte{0x0a, 0x51, 0xe5, 0xc0, 0x18, 0}) || data[7] != 2 {
		return nil, errors.New("unsupported winget compression header")
	}
	crc := crc32.Update(crc32.ChecksumIEEE(data[:6]), crc32.IEEETable, data[7:24])
	if byte(crc&0xff) != data[6] {
		return nil, errors.New("winget compression header checksum mismatch")
	}
	size := binary.LittleEndian.Uint64(data[8:16])
	first := binary.LittleEndian.Uint64(data[16:24])
	if size == 0 || size > metadataLimit || first != min(size, 32768) {
		return nil, errors.New("invalid winget decompressed size")
	}
	result := make([]byte, 0, int(size))
	for offset := 24; offset < len(data); {
		if len(data)-offset < 4 {
			return nil, io.ErrUnexpectedEOF
		}
		length := int(binary.LittleEndian.Uint32(data[offset:]))
		offset += 4
		if length < 2 || length > len(data)-offset || !bytes.Equal(data[offset:offset+2], []byte("CK")) {
			return nil, errors.New("invalid winget compressed block")
		}
		block := bytes.NewReader(data[offset+2 : offset+length])
		dictionary := result[max(0, len(result)-32768):]
		reader := flate.NewReaderDict(block, dictionary)
		decoded, err := io.ReadAll(io.LimitReader(reader, 32769))
		_ = reader.Close()
		if err != nil {
			return nil, fmt.Errorf("winget compressed block: %w", err)
		}
		want := min(int(size)-len(result), 32768)
		if len(decoded) != want || block.Len() != 0 {
			return nil, errors.New("winget compressed block length mismatch")
		}
		result = append(result, decoded...)
		offset += length
	}
	if uint64(len(result)) != size {
		return nil, errors.New("winget decompressed size mismatch")
	}
	return result, nil
}
