//go:build !darwin

package apple

import (
	"context"
	"os"
)

// Native com.apple.cs attributes are macOS-specific. Archives and disk images
// supply their logical attributes on every host through xattrFS.
func nativeSignatureAttributes(ctx context.Context, _ *os.File) (bool, error) {
	return false, ctx.Err()
}
