package intune

import (
	"fmt"
	"testing"
)

func BenchmarkWarmReconcile(b *testing.B) {
	for _, change := range []bool{false, true} {
		b.Run(fmt.Sprintf("metadata=%t", change), func(b *testing.B) {
			server, client := newGraphFixture(b)
			request := fixtureRequest(b)
			request.Artifact = treeArtifact(b)
			desired, err := compileRequest(request)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := reconcileFixture(b, client, request, desired); err != nil {
				b.Fatal(err)
			}
			before, lists, commits := len(server.paths), server.appLists, server.commits
			revision := 0
			b.ReportAllocs()
			for b.Loop() {
				if change {
					desired["description"] = fmt.Sprintf("Revision %d", revision%2)
					revision++
				}
				response, err := reconcileFixture(b, client, request, desired)
				if err != nil {
					b.Fatal(err)
				}
				if (len(response.Changes) != 0) != change {
					b.Fatalf("unexpected changes: %+v", response.Changes)
				}
			}
			b.ReportMetric(float64(len(server.paths)-before)/float64(b.N), "requests/op")
			b.ReportMetric(float64(server.appLists-lists)/float64(b.N), "app_lists/op")
			if server.commits != commits {
				b.Fatal("warm reconciliation uploaded unchanged content")
			}
		})
	}
}
