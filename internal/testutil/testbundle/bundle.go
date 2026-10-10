// Package testbundle generates a portable application workload for benchmarks.
package testbundle

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Write creates Benchmark.app with 256 resources and a 17 MiB executable.
// Repeated deterministic data gives codecs the same input on every host and crosses
// the PKG writer's 16 MiB PBZX block boundary. The bundle is unsigned.
func Write(t testing.TB) string {
	t.Helper()
	app := filepath.Join(t.TempDir(), "Benchmark.app")
	for _, dir := range []string{"MacOS", "Resources"} {
		if err := os.MkdirAll(filepath.Join(app, "Contents", dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	info := `<plist version="1.0"><dict><key>CFBundleIdentifier</key><string>org.example.benchmark</string><key>CFBundleExecutable</key><string>benchmark</string><key>CFBundleShortVersionString</key><string>1.0</string></dict></plist>`
	if err := os.WriteFile(filepath.Join(app, "Contents/Info.plist"), []byte(info), 0o644); err != nil {
		t.Fatal(err)
	}
	block := make([]byte, 32<<10)
	for i := 0; i < len(block); i += sha256.Size {
		digest := sha256.Sum256(fmt.Appendf(nil, "%d", i))
		copy(block[i:], digest[:])
	}
	file, err := os.OpenFile(filepath.Join(app, "Contents/MacOS/benchmark"), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	for range (17 << 20) / len(block) {
		if _, err := file.Write(block); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range 256 {
		name := filepath.Join(app, "Contents/Resources", fmt.Sprintf("%03d.dat", i))
		if err := os.WriteFile(name, block[i:i+512], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return app
}
