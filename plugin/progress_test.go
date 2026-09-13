package plugin_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestProgressReaderPreservesBytesAndErrors(t *testing.T) {
	failure := errors.New("transfer interrupted")
	for _, test := range []struct {
		name   string
		total  int64
		reader io.Reader
		err    error
	}{
		{"known", 7, strings.NewReader("payload"), nil},
		{"unknown", -1, strings.NewReader("payload"), nil},
		{"partial", 20, io.MultiReader(strings.NewReader("payload"), failingReader{failure}), failure},
	} {
		t.Run(test.name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&logs, nil)))
			data, err := io.ReadAll(plugin.ProgressReader(ctx, test.reader, test.total))
			if string(data) != "payload" || !errors.Is(err, test.err) {
				t.Fatalf("read %q, %v", data, err)
			}
			decoder := json.NewDecoder(&logs)
			var records []struct {
				Current, Total int64
				Final          bool `json:"progress_final"`
			}
			for decoder.More() {
				var record struct {
					Current, Total int64
					Final          bool `json:"progress_final"`
				}
				if err := decoder.Decode(&record); err != nil {
					t.Fatal(err)
				}
				records = append(records, record)
			}
			last := records[len(records)-1]
			if records[0].Current != 0 || last.Current != 7 || last.Total != test.total || !last.Final {
				t.Fatalf("measurements: %+v", records)
			}
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }
