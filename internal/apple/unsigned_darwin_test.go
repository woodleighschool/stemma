package apple

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/signature"
	"golang.org/x/sys/unix"
)

func TestUnsignedGenericBundleRejectsNativeSignatureAttributes(t *testing.T) {
	for _, name := range []string{"com.apple.cs.CodeDirectory", "com.apple.cs.Unknown"} {
		t.Run(name, func(t *testing.T) {
			app := scriptBundle(t)
			if err := unix.Setxattr(filepath.Join(app, "Contents/MacOS/script"), name, nil, 0); err != nil {
				t.Fatal(err)
			}
			_, err := VerifyApp(t.Context(), app, signature.Signer{})
			if err == nil || errors.Is(err, signature.ErrUnsigned) || !strings.Contains(err.Error(), "signature attributes") {
				t.Fatalf("native signature attribute presence: %v", err)
			}
		})
	}
}
