//go:build darwin

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

	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/appkit"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/corefoundation"
	"github.com/deploymenttheory/go-bindings-macosplatform/bindings/frameworks/quicklookthumbnailing"
)

// Foundation must be loaded before the bindings cache Objective-C classes.
//go:cgo_import_dynamic _ _ "/System/Library/Frameworks/Foundation.framework/Versions/C/Foundation"

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
	request := quicklookthumbnailing.NewThumbnailGenerationRequestWithFileAtURLSizeScaleRepresentationTypes(
		source, corefoundation.CGSize{Width: float64(size), Height: float64(size)}, 1,
		quicklookthumbnailing.ThumbnailGenerationRequestRepresentationTypeIcon,
	).WithIconMode(true)
	defer request.Release()
	generator := quicklookthumbnailing.SharedGenerator()
	defer generator.Release()
	defer generator.CancelRequest(request)
	representation, err := generator.GenerateBestRepresentationForRequest(ctx, request)
	if err != nil {
		return nil, "", "", fmt.Errorf("quick look icon rendering: %w", err)
	}
	if representation == nil {
		return nil, "", "", errors.New("quick look returned no icon")
	}
	defer representation.Release()
	image := representation.CGImage()
	if image.Object == nil {
		return nil, "", "", errors.New("quick look returned no bitmap")
	}
	defer image.Release()
	bitmap := appkit.NewBitmapImageRepWithCGImage(image)
	if bitmap == nil {
		return nil, "", "", errors.New("cannot encode quick look icon as PNG")
	}
	defer bitmap.Release()
	data := bitmap.RepresentationUsingTypeProperties(appkit.BitmapImageFileTypePNG, nil)
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
