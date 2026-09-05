// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

package msi

import (
	"encoding/binary"
	"errors"
	"math"
)

const (
	olePIDCodepage = uint32(1)
	oleVTInt16     = uint32(0x02)
	oleVTLPSTR     = uint32(0x1e)
	oleVTLPWSTR    = uint32(0x1f)
)

func readPropertySetString(data []byte, propertyID uint32) (string, bool) {
	if len(data) < 48 || binary.LittleEndian.Uint16(data[:2]) != 0xfffe {
		return "", false
	}
	sections := binary.LittleEndian.Uint32(data[24:28])
	if sections == 0 {
		return "", false
	}

	sectionOffset := binary.LittleEndian.Uint32(data[44:48])
	if int64(sectionOffset)+8 > int64(len(data)) {
		return "", false
	}
	section := int(sectionOffset)
	size := binary.LittleEndian.Uint32(data[section : section+4])
	if size < 8 || int64(section)+int64(size) > int64(len(data)) {
		return "", false
	}
	count := binary.LittleEndian.Uint32(data[section+4 : section+8])
	if count > 4096 || int64(section)+8+int64(count)*8 > int64(len(data)) {
		return "", false
	}

	codepage := int16(1252)
	target := -1
	for i := range count {
		entry := section + 8 + int(i)*8
		pid := binary.LittleEndian.Uint32(data[entry : entry+4])
		rel := binary.LittleEndian.Uint32(data[entry+4 : entry+8])
		abs, err := propertyOffset(section, size, rel)
		if err != nil {
			return "", false
		}
		if pid == olePIDCodepage && abs+6 <= len(data) && binary.LittleEndian.Uint32(data[abs:abs+4]) == oleVTInt16 {
			value := binary.LittleEndian.Uint16(data[abs+4 : abs+6])
			if value <= math.MaxInt16 {
				codepage = int16(value)
			}
		}
		if pid == propertyID {
			target = abs
		}
	}
	if target < 0 || target+8 > len(data) {
		return "", false
	}

	valueType := binary.LittleEndian.Uint32(data[target : target+4])
	switch valueType {
	case oleVTLPSTR:
		n := binary.LittleEndian.Uint32(data[target+4 : target+8])
		if n == 0 || int64(target)+8+int64(n) > int64(len(data)) {
			return "", false
		}
		return trimNulls(decodeBytes(data[target+8:target+8+int(n)], int(codepage))), true
	case oleVTLPWSTR:
		chars := binary.LittleEndian.Uint32(data[target+4 : target+8])
		byteLen := int64(chars) * 2
		if chars == 0 || int64(target)+8+byteLen > int64(len(data)) {
			return "", false
		}
		return trimNulls(utf16String(data[target+8 : target+8+int(byteLen)])), true
	default:
		return "", false
	}
}

func propertyOffset(section int, sectionSize uint32, rel uint32) (int, error) {
	if rel >= sectionSize {
		return 0, errors.New("property offset is outside section")
	}
	abs := int64(section) + int64(rel)
	if abs > int64(^uint(0)>>1) {
		return 0, errors.New("property offset is too large")
	}
	return int(abs), nil
}
