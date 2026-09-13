package plugin_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestStagesFinishIndependentlyAndOnlyOnce(t *testing.T) {
	var out bytes.Buffer
	ctx := plugin.WithLogger(t.Context(), slog.New(slog.NewJSONHandler(&out, nil)).With("resource", "example"))
	parent := plugin.Stage(ctx, "Prepare outputs")
	child := plugin.Stage(ctx, "Inspect application")
	child(nil)
	parent(errors.New("package verification failed"))
	parent(nil)
	decoder := json.NewDecoder(&out)
	type record struct {
		Message  string `json:"msg"`
		Resource string `json:"resource"`
		Start    bool   `json:"stage"`
		End      bool   `json:"stage_result"`
		Error    string `json:"error"`
	}
	var records []record
	for decoder.More() {
		var record record
		if err := decoder.Decode(&record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	if len(records) != 4 || !records[0].Start || !records[1].Start || !records[2].End || records[2].Message != "Inspect application" || records[2].Error != "" || !records[3].End || records[3].Message != "Prepare outputs" || records[3].Error == "" {
		t.Fatalf("operation events: %+v", records)
	}
	for _, record := range records {
		if record.Resource != "example" {
			t.Fatalf("lost resource scope: %+v", record)
		}
	}
}
