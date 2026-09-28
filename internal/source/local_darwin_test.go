package source

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/sys/unix"
)

func TestLocalInputsRefuseSignatureLoss(t *testing.T) {
	for name, input := range map[string]plugin.Input{
		"file":          {Resolver: "file", Config: map[string]any{"path": "payload/helper"}},
		"folder":        {Resolver: "file", Config: map[string]any{"path": "payload"}},
		"selected tree": {Resolver: "local", Config: map[string]any{"include": []string{"payload/**"}}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			name := filepath.Join(root, "payload", "helper")
			writeInput(t, name, "signed content", 0o644)
			store, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			m := New(store, root, false)
			if err := unix.Setxattr(name, "com.apple.quarantine", []byte("synthetic"), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Resolve(t.Context(), input); err != nil {
				t.Fatal(err)
			}
			if err := unix.Setxattr(name, "com.apple.cs.CodeSignature", []byte("signature"), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Resolve(t.Context(), input); err == nil || !strings.Contains(err.Error(), "code signature in extended attributes") {
				t.Fatalf("signature discarded: %v", err)
			}
		})
	}
}
