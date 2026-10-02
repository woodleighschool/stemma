package macsoftware

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestNativeArchiveApplicationSignature(t *testing.T) {
	mount := mountMetadataImage(t, metadataApplication(t))
	if output, err := exec.CommandContext(t.Context(), "/usr/bin/codesign", "--verify", "--strict", "--deep", filepath.Join(mount, "GenericFixture.app")).CombinedOutput(); err != nil {
		t.Fatalf("native signature: %v: %s", err, output)
	}
}

func TestNativeArchiveResourceFork(t *testing.T) {
	mount := mountMetadataImage(t, resourceForkApplication(t))
	data, err := os.ReadFile(filepath.Join(mount, "Example.app/Contents/Info.plist/..namedfork/rsrc"))
	if err != nil || !bytes.Equal(data, bytes.Repeat([]byte("fork"), 20000)) {
		t.Fatalf("native resource fork changed: %v", err)
	}
}

func mountMetadataImage(t *testing.T, artifact plugin.Artifact) string {
	t.Helper()
	mount := filepath.Join(t.TempDir(), "volume")
	if output, err := exec.CommandContext(t.Context(), "/usr/bin/hdiutil", "attach", "-readonly", "-nobrowse", "-noautoopen", "-mountpoint", mount, artifact.Path).CombinedOutput(); err != nil {
		t.Fatalf("mount: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.CommandContext(context.Background(), "/usr/bin/hdiutil", "detach", "-force", mount).CombinedOutput(); err != nil {
			t.Errorf("detach: %v: %s", err, output)
		}
	})
	return mount
}
