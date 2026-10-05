package engine

import (
	"encoding/json"
	"github.com/woodleighschool/stemma/plugin"
	"strings"
	"testing"
)

func TestUpdateReportsResolutionWithoutClaimingPreparation(t *testing.T) {
	report := Report{Resources: []ResourceReport{{Name: "resolved"}, {Name: "cached", Cached: true}, {Name: "failed", Error: "unavailable"}, {Name: "blocked", BlockedBy: []string{"failed"}}}}
	report.Summarize("update")
	if report.Summary.Resolved != 2 || report.Summary.Prepared != 0 || report.Summary.Cached != 0 || report.Summary.Failed != 1 || report.Summary.Blocked != 1 {
		t.Fatalf("update summary: %+v", report.Summary)
	}
	report.Summarize("prepare")
	if report.Summary.Resolved != 0 || report.Summary.Prepared != 1 || report.Summary.Cached != 1 {
		t.Fatalf("preparation summary: %+v", report.Summary)
	}
}

func TestSummarySeparatesDestinationsFromPublicationOperations(t *testing.T) {
	report := Report{Resources: []ResourceReport{
		{Name: "one", Destinations: []DestinationReport{{Name: "repo", Applied: true, Changes: []plugin.Change{{Action: "set"}}}}},
		{Name: "two", Destinations: []DestinationReport{{Name: "repo", Applied: true, Changes: []plugin.Change{{Action: "set"}}}}},
		{Name: "three", Destinations: []DestinationReport{{Name: "archive"}}},
	}}
	report.Summarize("apply")
	got := report.Summary
	if got.Resources != 3 || got.Destinations != 2 || got.DestinationOperations != 3 || got.Applied != 2 || got.Unchanged != 1 {
		t.Fatalf("counts: %+v", got)
	}
}

func TestIconSummaryDoesNotClaimPreparationOrUnchangedForSkippedWork(t *testing.T) {
	report := Report{Resources: []ResourceReport{{Icon: "created raw"}, {Icon: "unchanged"}, {Icon: "no icon declared"}}}
	report.Summarize("icon")
	got := report.Summary
	if got.Created != 1 || got.Unchanged != 1 || got.Skipped != 1 || got.Prepared != 0 || got.Cached != 0 {
		t.Fatalf("counts: %+v", got)
	}
}

func TestMachineReportSeparatesReconciliationMutationAndCache(t *testing.T) {
	report := Report{Resources: []ResourceReport{{Icon: "unchanged"}}}
	report.Summarize("icon")
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "cached") || strings.Contains(text, "prepared") || !strings.Contains(text, `"created":0`) {
		t.Fatalf("icon projection: %s", text)
	}
	for _, test := range []struct {
		report DestinationReport
		status string
	}{
		{DestinationReport{Applied: true}, "unchanged"},
		{DestinationReport{Changes: []plugin.Change{{Action: "upload"}}}, "planned"},
		{DestinationReport{Applied: true, Changes: []plugin.Change{{Action: "upload"}}}, "applied"},
		{DestinationReport{Error: "upload failed", Changes: []plugin.Change{{Action: "upload"}}}, "failed"},
	} {
		data, err := json.Marshal(test.report)
		if err != nil {
			t.Fatal(err)
		}
		text = string(data)
		if !strings.Contains(text, `"status":"`+test.status+`"`) || strings.Contains(text, `"changes":null`) || strings.Contains(text, `"applied":`) {
			t.Fatalf("destination projection: %s", text)
		}
	}
}
