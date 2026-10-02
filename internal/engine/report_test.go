package engine

import "testing"

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
