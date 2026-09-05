// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

package msi

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const msiTableMarker = rune(0x4840)

type database struct {
	file      *compoundFile
	byRaw     map[string]directoryEntry
	byDecoded map[string]directoryEntry
}
type stringTable struct {
	strings     []string
	bytesPerRef int
}

func newDatabase(data []byte) (*database, error) {
	cf, err := newCompoundFile(data)
	if err != nil {
		return nil, err
	}
	db := &database{
		file:      cf,
		byRaw:     make(map[string]directoryEntry),
		byDecoded: make(map[string]directoryEntry),
	}
	for _, stream := range cf.streams {
		if _, exists := db.byRaw[stream.name]; exists {
			return nil, errors.New("duplicate MSI stream")
		}
		db.byRaw[stream.name] = stream
		decoded := strings.TrimPrefix(decodeStreamName(stream.name), string(msiTableMarker))
		if _, exists := db.byDecoded[decoded]; exists {
			return nil, errors.New("duplicate decoded MSI stream")
		}
		db.byDecoded[decoded] = stream
	}
	return db, nil
}

func (db *database) readProperties() (map[string]string, error) {
	table, ok, err := db.decoded("Property")
	if err != nil || !ok {
		return nil, err
	}
	strings, err := db.loadStrings()
	if err != nil {
		return nil, err
	}
	rowSize := strings.bytesPerRef * 2
	if rowSize == 0 || len(table)%rowSize != 0 {
		return nil, errors.New("property table has an invalid row size")
	}
	rows := len(table) / rowSize
	props := make(map[string]string, rows)
	for row := range rows {
		nameIndex, ok := readRef(table, row*strings.bytesPerRef, strings.bytesPerRef)
		if !ok {
			return nil, errors.New("property name reference is truncated")
		}
		valueIndex, ok := readRef(table, rows*strings.bytesPerRef+row*strings.bytesPerRef, strings.bytesPerRef)
		if !ok {
			return nil, errors.New("property value reference is truncated")
		}
		name := strings.get(nameIndex)
		if name == "" {
			continue
		}
		if _, exists := props[name]; exists {
			return nil, errors.New("duplicate MSI property")
		}
		if nameIndex >= len(strings.strings) || valueIndex >= len(strings.strings) {
			return nil, errors.New("invalid MSI string reference")
		}
		props[name] = strings.get(valueIndex)
	}
	return props, nil
}

func (db *database) packageCode() string {
	for name, entry := range db.byRaw {
		if !strings.HasSuffix(name, "SummaryInformation") {
			continue
		}
		data, err := db.file.readStream(entry)
		if err != nil {
			return ""
		}
		value, ok := readPropertySetString(data, 9)
		if !ok {
			return ""
		}
		return value
	}
	return ""
}

func (db *database) loadStrings() (*stringTable, error) {
	pool, ok, err := db.decoded("_StringPool")
	if err != nil || !ok {
		return nil, errors.New("MSI has no _StringPool stream")
	}
	data, ok, err := db.decoded("_StringData")
	if err != nil {
		return nil, err
	}
	if !ok {
		data = nil
	}
	if len(pool)%4 != 0 {
		return nil, errors.New("_StringPool length is not a multiple of four")
	}
	entries := len(pool) / 4
	if entries == 0 {
		return nil, errors.New("_StringPool is empty")
	}

	header := binary.LittleEndian.Uint32(pool[:4])
	codepage := int(header & 0x7fffffff)
	if codepage != 0 && codepage != 1252 && codepage != 65001 && codepage != 1200 {
		return nil, fmt.Errorf("unsupported MSI code page %d", codepage)
	}
	bytesPerRef := 2
	if header&0x80000000 != 0 {
		bytesPerRef = 3
	}

	table := &stringTable{
		strings:     make([]string, 1, entries),
		bytesPerRef: bytesPerRef,
	}
	offset := 0
	for i := 1; i < entries; i++ {
		entry := pool[i*4 : i*4+4]
		n := int(binary.LittleEndian.Uint16(entry[:2]))
		refs := int(binary.LittleEndian.Uint16(entry[2:4]))
		if n == 0 && refs != 0 {
			if i+1 >= entries {
				return nil, errors.New("long string entry is truncated")
			}
			low := int(binary.LittleEndian.Uint16(pool[(i+1)*4 : (i+1)*4+2]))
			n = refs<<16 | low
			value, next, err := readStringData(data, offset, n, codepage)
			if err != nil {
				return nil, err
			}
			table.strings = append(table.strings, value, "")
			offset = next
			i++
			continue
		}
		value, next, err := readStringData(data, offset, n, codepage)
		if err != nil {
			return nil, err
		}
		table.strings = append(table.strings, value)
		offset = next
	}
	if len(table.strings) > 0xffff {
		table.bytesPerRef = 3
	}
	return table, nil
}

func (db *database) decoded(name string) ([]byte, bool, error) {
	entry, ok := db.byDecoded[name]
	if !ok {
		return nil, false, nil
	}
	data, err := db.file.readStream(entry)
	if err != nil {
		return nil, false, fmt.Errorf("read MSI stream %s: %w", name, err)
	}
	return data, true, nil
}

func (st *stringTable) get(index int) string {
	if index <= 0 || index >= len(st.strings) {
		return ""
	}
	return st.strings[index]
}

func readRef(data []byte, off int, size int) (int, bool) {
	if size != 2 && size != 3 {
		return 0, false
	}
	if off < 0 || off+size > len(data) {
		return 0, false
	}
	value := int(data[off]) | int(data[off+1])<<8
	if size == 3 {
		value |= int(data[off+2]) << 16
	}
	return value, true
}

func readStringData(data []byte, off int, n int, codepage int) (string, int, error) {
	if off < 0 || n < 0 || off+n > len(data) {
		return "", 0, errors.New("_StringData is shorter than _StringPool declares")
	}
	value := data[off : off+n]
	if codepage == 0 {
		for _, b := range value {
			if b > 127 {
				return "", 0, errors.New("non-ASCII MSI string with unspecified code page")
			}
		}
	}
	if codepage == 65001 && !utf8.Valid(value) {
		return "", 0, errors.New("invalid UTF-8 MSI string")
	}
	if codepage == 1200 && len(value)%2 != 0 {
		return "", 0, errors.New("truncated UTF-16 MSI string")
	}
	return decodeBytes(data[off:off+n], codepage), off + n, nil
}

func decodeBytes(data []byte, codepage int) string {
	switch codepage {
	case 1200:
		return utf16String(data)
	case 65001:
		if utf8.Valid(data) {
			return string(data)
		}
	}

	runes := make([]rune, len(data))
	for i, b := range data {
		runes[i] = rune(b)
		if codepage == 1252 && b >= 0x80 && b <= 0x9f {
			runes[i] = windows1252[b-0x80]
		}
	}
	return string(runes)
}

var windows1252 = []rune{'€', '\u0081', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '\u008d', 'Ž', '\u008f', '\u0090', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '\u009d', 'ž', 'Ÿ'}

func trimNulls(s string) string {
	return strings.TrimRight(s, "\x00")
}
