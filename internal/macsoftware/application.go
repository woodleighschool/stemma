package macsoftware

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/woodleighschool/stemma/internal/apple"
	"github.com/woodleighschool/stemma/internal/diskimage"
	"github.com/woodleighschool/stemma/plugin"
)

// ErrNoApplication reports an installer whose preparation selected no application.
var ErrNoApplication = errors.New("installer has no selected application")

// Bundle materializes the application preparation selected for installer into
// workspace and returns its path, so authoring can read it while the installer
// bytes stay untouched.
func Bundle(ctx context.Context, installer plugin.Artifact, workspace string) (string, error) {
	data, ok := installer.Evidence["macos.application"]
	if !ok {
		return "", ErrNoApplication
	}
	var app plugin.Subject
	if err := json.Unmarshal(data, &app); err != nil || app.App == nil {
		return "", errors.New("macos.application evidence requires an application subject")
	}
	// Extraction writes into a new directory and expects its parent to exist.
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return "", err
	}
	expanded := filepath.Join(workspace, "expanded")
	switch strings.ToLower(installer.Format) {
	case "dmg":
		return diskimage.Extract(ctx, installer.Path, expanded, app.Path)
	case "pkg":
		return apple.ExtractApplication(ctx, installer.Path, app.Path, app.InstalledPath, expanded)
	default:
		return "", fmt.Errorf("installer format %q holds no application bundle", installer.Format)
	}
}
