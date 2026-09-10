package plugin

import (
	"reflect"
	"testing"
)

func TestPublicationOrderTracksDistinctPayloadTransitions(t *testing.T) {
	var history Publications
	history.Record("newer-version")
	history.Record("older-version")
	history.Record("older-version")
	if history.Sequence != 2 {
		t.Fatal("metadata-only reconciliation advanced publication order")
	}
	history.Record("newer-version")
	if got := history.Retained(1); !reflect.DeepEqual(got, map[string]bool{"newer-version": true}) {
		t.Fatalf("republication was not current: %v", got)
	}
	if got := history.Retained(2); len(got) != 2 || history.Sequence != 3 {
		t.Fatalf("distinct payload history: %v, sequence %d", got, history.Sequence)
	}
	if got := (Publications{}).Retained(1); len(got) != 0 {
		t.Fatalf("unknown history invented a retention selection: %v", got)
	}
}
