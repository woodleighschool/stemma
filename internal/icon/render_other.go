//go:build !darwin

package icon

import "context"

// Render is unavailable off macOS; committed assets still publish everywhere.
func Render(context.Context, string, int) ([]byte, error) {
	return nil, ErrUnsupportedHost
}
