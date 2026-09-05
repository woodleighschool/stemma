// Portions adapted from WrapTune-MacOS.
// Copyright (c) 2026 thefinder808
// SPDX-License-Identifier: MIT
// See LICENSE for the full license text.

// Package intunecontent implements the shared ProfileVersion1 transport encryption.
// It encrypts raw input bytes, with no operating-system-specific container.
package intunecontent

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/woodleighschool/stemma/internal/fileio"
)

// ErrIntegrity means the package failed a cryptographic or metadata check.
var ErrIntegrity = errors.New("intune content integrity verification failed")

func encrypt(ctx context.Context, plain, encrypted *os.File, m *Info) error {
	key, macKey, iv := make([]byte, 32), make([]byte, 32), make([]byte, 16)
	for _, value := range [][]byte{key, macKey, iv} {
		if _, err := rand.Read(value); err != nil {
			return err
		}
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	cbc := cipher.NewCBCEncrypter(block, iv)
	if _, err := encrypted.Write(make([]byte, 32)); err != nil {
		return err
	}
	mac := hmac.New(sha256.New, macKey)
	out := io.MultiWriter(encrypted, mac)
	if _, err := out.Write(iv); err != nil {
		return err
	}
	hash := sha256.New()
	input := io.TeeReader(plain, hash)
	buffer := make([]byte, 64*1024)
	var size int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := io.ReadFull(input, buffer)
		size += int64(n)
		if size > maxContent {
			return errors.New("intune content exceeds 2 GiB")
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return readErr
		}
		if readErr != nil {
			padding := aes.BlockSize - n%aes.BlockSize
			for i := n; i < n+padding; i++ {
				buffer[i] = byte(padding)
			}
			n += padding
		}
		cbc.CryptBlocks(buffer[:n], buffer[:n])
		if _, err := out.Write(buffer[:n]); err != nil {
			return err
		}
		if readErr != nil {
			break
		}
	}
	if _, err := encrypted.WriteAt(mac.Sum(nil), 0); err != nil {
		return err
	}
	m.PlaintextSize = size
	m.EncryptedContentSize = 48 + size + int64(aes.BlockSize-int(size%aes.BlockSize))
	m.PayloadSHA256 = hex.EncodeToString(hash.Sum(nil))
	m.EncryptionInfo = EncryptionInfo{
		EncryptionKey: base64.StdEncoding.EncodeToString(key), MacKey: base64.StdEncoding.EncodeToString(macKey),
		InitializationVector: base64.StdEncoding.EncodeToString(iv), Mac: base64.StdEncoding.EncodeToString(mac.Sum(nil)),
		ProfileIdentifier: profile, FileDigest: base64.StdEncoding.EncodeToString(hash.Sum(nil)), FileDigestAlgorithm: "SHA256",
	}
	return nil
}

// Decrypt authenticates the entire encrypted file before decrypting it.
func Decrypt(ctx context.Context, encrypted, plain *os.File, m *Info) error {
	key, err := decodeField(m.EncryptionInfo.EncryptionKey, "EncryptionKey", 32)
	if err != nil {
		return err
	}
	macKey, err := decodeField(m.EncryptionInfo.MacKey, "MacKey", 32)
	if err != nil {
		return err
	}
	iv, err := decodeField(m.EncryptionInfo.InitializationVector, "InitializationVector", 16)
	if err != nil {
		return err
	}
	expectedMAC, err := decodeField(m.EncryptionInfo.Mac, "Mac", 32)
	if err != nil {
		return err
	}
	expectedDigest, err := decodeField(m.EncryptionInfo.FileDigest, "FileDigest", 32)
	if err != nil {
		return err
	}
	info, err := encrypted.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size < 64 || (size-48)%aes.BlockSize != 0 || size != 48+m.PlaintextSize+int64(aes.BlockSize-int(m.PlaintextSize%aes.BlockSize)) {
		return fmt.Errorf("%w: encrypted length", ErrIntegrity)
	}
	header := make([]byte, 48)
	if _, err := io.ReadFull(encrypted, header); err != nil {
		return err
	}
	if !hmac.Equal(header[:32], expectedMAC) || !hmac.Equal(header[32:], iv) {
		return fmt.Errorf("%w: header differs from Detection.xml", ErrIntegrity)
	}
	mac := hmac.New(sha256.New, macKey)
	mac.Write(iv)
	if _, err := copyContext(ctx, mac, encrypted, maxContent+16); err != nil {
		return err
	}
	if !hmac.Equal(mac.Sum(nil), expectedMAC) {
		return fmt.Errorf("%w: HMAC", ErrIntegrity)
	}
	if _, err := encrypted.Seek(48, io.SeekStart); err != nil {
		return err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	cbc := cipher.NewCBCDecrypter(block, iv)
	hash := sha256.New()
	out := io.MultiWriter(plain, hash)
	buffer := make([]byte, 64*1024)
	remaining := size - 48
	var count int64
	for remaining > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int(min(int64(len(buffer)), remaining))
		if _, err := io.ReadFull(encrypted, buffer[:n]); err != nil {
			return err
		}
		cbc.CryptBlocks(buffer[:n], buffer[:n])
		remaining -= int64(n)
		if remaining == 0 {
			padding := int(buffer[n-1])
			if padding < 1 || padding > aes.BlockSize {
				return fmt.Errorf("%w: padding", ErrIntegrity)
			}
			for _, value := range buffer[n-padding : n] {
				if int(value) != padding {
					return fmt.Errorf("%w: padding", ErrIntegrity)
				}
			}
			n -= padding
		}
		if _, err := out.Write(buffer[:n]); err != nil {
			return err
		}
		count += int64(n)
	}
	if count != m.PlaintextSize || !hmac.Equal(hash.Sum(nil), expectedDigest) {
		return fmt.Errorf("%w: payload digest or size", ErrIntegrity)
	}
	m.EncryptedContentSize = size
	m.PayloadSHA256 = hex.EncodeToString(hash.Sum(nil))
	return nil
}

// EncryptionInfo contains the base64-encoded Intune content commit parameters.
// These keys provide transport integrity; they are not publisher credentials.
type EncryptionInfo struct {
	EncryptionKey        string `xml:"EncryptionKey" json:"encryptionKey"`
	MacKey               string `xml:"MacKey" json:"macKey"`
	InitializationVector string `xml:"InitializationVector" json:"initializationVector"`
	Mac                  string `xml:"Mac" json:"mac"`
	ProfileIdentifier    string `xml:"ProfileIdentifier" json:"profileIdentifier"`
	FileDigest           string `xml:"FileDigest" json:"fileDigest"`
	FileDigestAlgorithm  string `xml:"FileDigestAlgorithm" json:"fileDigestAlgorithm"`
}

// Info binds encryption parameters to the exact plaintext and ciphertext sizes.
type Info struct {
	PayloadSHA256        string
	PlaintextSize        int64
	EncryptedContentSize int64
	EncryptionInfo       EncryptionInfo
}

// Encrypt writes a fresh randomly encrypted transport file from the current
// position of plain. The encrypted destination must be empty and seekable.
func Encrypt(ctx context.Context, plain, encrypted *os.File) (Info, error) {
	var result Info
	if err := encrypt(ctx, plain, encrypted, &result); err != nil {
		return Info{}, err
	}
	return result, nil
}

const maxContent = int64(2 << 30)
const profile = "ProfileVersion1"

func copyContext(ctx context.Context, w io.Writer, r io.Reader, limit int64) (int64, error) {
	n, err := io.Copy(w, io.LimitReader(fileio.Reader{Context: ctx, Reader: r}, limit+1))
	if err != nil {
		return n, err
	}
	if n > limit {
		return n, errors.New("intune content exceeds size limit")
	}
	return n, nil
}
func decodeField(value, name string, size int) ([]byte, error) {
	data, err := base64.StdEncoding.Strict().DecodeString(value)
	if err != nil || len(data) != size {
		return nil, fmt.Errorf("invalid %s: expected %d base64-encoded bytes", name, size)
	}
	return data, nil
}
