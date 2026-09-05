// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

package msi

import "strings"

const streamNameAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz._"

func decodeStreamName(raw string) string {
	var b strings.Builder
	b.Grow(len(raw) * 2)
	for _, r := range raw {
		switch {
		case r >= 0x4800 && r < 0x4840:
			b.WriteByte(streamNameAlphabet[r-0x4800])
		case r >= 0x3800 && r < 0x4800:
			x := r - 0x3800
			b.WriteByte(streamNameAlphabet[x&0x3f])
			b.WriteByte(streamNameAlphabet[(x>>6)&0x3f])
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
