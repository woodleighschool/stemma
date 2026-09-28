//go:build !darwin

package archive

import (
	"context"
	"os"
)

// CheckXattrs rejects native code signatures that a byte-only copy would lose.
// Native com.apple.cs attributes are specific to macOS; archived attributes are
// checked during extraction on every platform.
func CheckXattrs(ctx context.Context, _ *os.File) error { return ctx.Err() }
