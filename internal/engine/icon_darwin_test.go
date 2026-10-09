package engine

import (
	"bytes"
	"fmt"
	"image/png"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/testutil/testproject"
)

func TestIconFromDrawsAnInstalledApplication(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "stemma.yaml")
	testproject.Write(t, filename, fmt.Sprintf(iconProject, "http://vendor.invalid"))
	// Font Book declares its icon in an asset catalog, which only the system renderer reads.
	options := Options{ConfigPath: filename, Method: "icon", Resources: []string{"MacSoftware/branding"}, Icons: IconOptions{Presentation: icon.Glassy, Size: 256, From: "/System/Applications/Font Book.app"}}
	report, err := Run(t.Context(), options)
	if err != nil || len(report.Resources) != 1 || report.Resources[0].Icon != "created glassy" {
		t.Fatalf("icon from an installed application: %+v, %v", report.Resources, err)
	}
	data, err := icon.Read(root, "shared-artwork")
	if err != nil {
		t.Fatal(err)
	}
	if config, err := png.DecodeConfig(bytes.NewReader(data)); err != nil || config.Width != 256 || config.Height != 256 {
		t.Fatalf("asset is %dx%d: %v", config.Width, config.Height, err)
	}
}
