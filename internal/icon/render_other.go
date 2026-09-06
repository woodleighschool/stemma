//go:build !darwin

package icon

import (
	"context"
	"errors"
)

func render(context.Context, string, int) ([]byte, string, string, error) {
	return nil, "", "", errors.New("system icon rendering requires macOS; reuse a retained PNG")
}
