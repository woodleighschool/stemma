package intunewin

import (
	"context"
	"github.com/woodleighschool/stemma/internal/intunecontent"
	"os"
)

// ErrIntegrity means the package failed a cryptographic or metadata check.
var ErrIntegrity = intunecontent.ErrIntegrity

func encrypt(ctx context.Context, plain, encrypted *os.File, m *Metadata) error {
	info, err := intunecontent.Encrypt(ctx, plain, encrypted)
	if err != nil {
		return err
	}
	m.PayloadSHA256 = info.PayloadSHA256
	m.PlaintextSize = info.PlaintextSize
	m.EncryptedContentSize = info.EncryptedContentSize
	m.EncryptionInfo = info.EncryptionInfo
	return nil
}

func decrypt(ctx context.Context, encrypted, plain *os.File, m *Metadata) error {
	info := intunecontent.Info{PlaintextSize: m.PlaintextSize, EncryptionInfo: m.EncryptionInfo}
	if err := intunecontent.Decrypt(ctx, encrypted, plain, &info); err != nil {
		return err
	}
	m.PayloadSHA256 = info.PayloadSHA256
	m.EncryptedContentSize = info.EncryptedContentSize
	return nil
}
