package icon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func render(ctx context.Context, source string, size int) ([]byte, string, string, error) {
	if !strings.EqualFold(filepath.Ext(source), ".app") {
		return nil, "", "", errors.New("system icon rendering requires a .app bundle")
	}
	info, err := os.Stat(source)
	if err != nil {
		return nil, "", "", err
	}
	if !info.IsDir() {
		return nil, "", "", errors.New("system icon source must be a .app directory")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := quickLookPNG(ctx, source, size)
	if err != nil {
		return nil, "", "", fmt.Errorf("quick look icon rendering: %w", err)
	}
	version, err := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-productVersion").Output()
	if err != nil {
		return nil, "", "", err
	}
	build, err := exec.CommandContext(ctx, "/usr/bin/sw_vers", "-buildVersion").Output()
	if err != nil {
		return nil, "", "", err
	}
	return data, strings.TrimSpace(string(version)), strings.TrimSpace(string(build)), nil
}
