//go:build !darwin

package icon

import "context"

const hostPresentation = Raw

// glassy is unavailable off macOS; Resolve rejects it before any work starts.
func glassy(context.Context, Subject, int, string) ([]byte, error) {
	return nil, ErrUnsupportedHost
}
