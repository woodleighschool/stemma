package icon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Render draws an application bundle's icon with the system renderer at size
// pixels, so the result carries the current macOS presentation.
func Render(ctx context.Context, app string, size int) ([]byte, error) {
	if !strings.EqualFold(filepath.Ext(app), ".app") {
		return nil, errors.New("native icon rendering requires a .app bundle")
	}
	info, err := os.Stat(app)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("native icon rendering requires a .app directory")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := quickLookPNG(ctx, app, size)
	if err != nil {
		return nil, fmt.Errorf("quick look icon rendering: %w", err)
	}
	return data, nil
}
