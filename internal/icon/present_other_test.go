//go:build !darwin

package icon

import (
	"errors"
	"testing"
)

func TestGlassyPresentationNeedsMacOS(t *testing.T) {
	if _, err := Glassy.Resolve(); !errors.Is(err, ErrUnsupportedHost) {
		t.Fatalf("glassy resolved: %v", err)
	}
	if resolved, err := Auto.Resolve(); err != nil || resolved != Raw {
		t.Fatalf("auto resolved to %q: %v", resolved, err)
	}
}
