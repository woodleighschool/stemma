package engine

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

func TestCleanupFailureIsReportedWhileAnotherRunHoldsTheCache(t *testing.T) {
	store, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	release, err := store.Lease(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = release() }()
	var logs bytes.Buffer
	ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewTextHandler(&logs, nil)))
	closed := false
	s := session{closers: []func() error{
		func() error { closed = true; return nil },
		func() error { return errors.New("workspace is locked") },
	}}
	s.close(ctx)
	if !closed || !strings.Contains(logs.String(), "Run cleanup incomplete") || !strings.Contains(logs.String(), "workspace is locked") {
		t.Fatalf("cleanup hid failure or abandoned other releases: %s", logs.String())
	}
}
