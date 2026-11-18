package engine

import (
	"encoding/json"
	"testing"

	"github.com/woodleighschool/stemma/internal/lock"
)

func TestStatusesFollowWhatTheCommandDid(t *testing.T) {
	cached := map[string]Prepared{"installer": {Cached: true}}
	built := map[string]Prepared{"installer": {}}
	signed := map[string]Prepared{"installer": {Evidence: map[string]json.RawMessage{"signing": json.RawMessage(`{"observed":[{"state":"unsigned"}]}`)}}}
	for name, test := range map[string]struct {
		method   string
		resource ResourceReport
		want     Status
		reason   string
	}{
		"update with a lock change":    {"update", ResourceReport{Inputs: []lock.InputChange{{Kind: lock.Updated}}}, StatusChanged, ""},
		"update without one":           {"update", ResourceReport{}, StatusUnchanged, ""},
		"prepared now":                 {"prepare", ResourceReport{Artifacts: built}, StatusChanged, ""},
		"prepared before":              {"prepare", ResourceReport{Artifacts: cached}, StatusUnchanged, ""},
		"signers derived":              {"signature", ResourceReport{Artifacts: signed}, StatusChanged, ""},
		"no signing subject":           {"signature", ResourceReport{Artifacts: built}, StatusSkipped, "nothing to declare"},
		"a changed publication":        {"apply", ResourceReport{Publications: []Publication{{Status: StatusUnchanged}, {Status: StatusChanged}}}, StatusChanged, ""},
		"current everywhere":           {"plan", ResourceReport{Publications: []Publication{{Status: StatusUnchanged}}}, StatusUnchanged, ""},
		"failure wins over changes":    {"apply", ResourceReport{Error: "upload failed", Publications: []Publication{{Status: StatusChanged}, {Status: StatusFailed}}}, StatusFailed, ""},
		"blocked wins over everything": {"update", ResourceReport{Error: "requires", BlockedBy: []string{"MacSoftware/a"}}, StatusBlocked, ""},
		"icon outcome stands":          {"icon", ResourceReport{Status: StatusSkipped, Reason: "no artwork"}, StatusSkipped, "no artwork"},
	} {
		resource := test.resource
		resource.status(test.method)
		if resource.Status != test.want || resource.Reason != test.reason {
			t.Errorf("%s: %s %q, want %s %q", name, resource.Status, resource.Reason, test.want, test.reason)
		}
	}
}

func TestSummaryCountsSelectedResourcesByStatus(t *testing.T) {
	report := Report{Resources: []ResourceReport{
		{Status: StatusChanged, Publications: []Publication{{Removed: []string{"1.0"}}, {Removed: []string{"0.9", "0.8"}}}},
		{Status: StatusUnchanged},
		{Status: StatusFailed},
		{Status: StatusBlocked},
		{Status: StatusSkipped},
	}}
	report.Summarize(8)
	want := Summary{Resources: 8, Changed: 1, Unchanged: 1, Failed: 1, Blocked: 1, Skipped: 1, NotReached: 3, Removed: 3}
	if report.Summary != want {
		t.Fatalf("summary = %+v, want %+v", report.Summary, want)
	}
	data, err := json.Marshal(Summary{Resources: 1, Unchanged: 1})
	if err != nil || string(data) != `{"resources":1,"changed":0,"unchanged":1,"failed":0,"blocked":0,"skipped":0,"not_reached":0,"removed":0}` {
		t.Fatalf("summary keys vary by command: %s %v", data, err)
	}
}

// reused reports whether a run reused every output of a resource's preparation.
func reused(resource ResourceReport) bool {
	for _, artifact := range resource.Artifacts {
		if !artifact.Cached {
			return false
		}
	}
	return len(resource.Artifacts) > 0
}
