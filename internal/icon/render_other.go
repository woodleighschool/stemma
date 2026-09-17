//go:build !darwin

package macsoftware

import (
	"context"
	"errors"
)

func nativeIcon(context.Context, string, int) ([]byte, string, string, error) {
	return nil, "", "", errors.New("native rendering requires macOS")
}
