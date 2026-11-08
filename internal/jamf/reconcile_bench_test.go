package jamf

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkWarmReconcile(b *testing.B) {
	for _, change := range []bool{false, true} {
		b.Run(fmt.Sprintf("metadata=%t", change), func(b *testing.B) {
			server, request := newFixture(b)
			request.Artifact = fixtureArtifact(b, strings.Repeat("payload\n", 1<<20))
			if _, err := Handle(b.Context(), request); err != nil {
				b.Fatal(err)
			}
			before := len(server.operations())
			contentRequests, revision := 0, 0
			b.ReportAllocs()
			for b.Loop() {
				if change {
					request.Metadata = raw(map[string]any{"info": fmt.Sprintf("Revision %d", revision%2)})
					revision++
				}
				current := request
				current.Artifact.Path = ""
				response, err := Handle(b.Context(), current)
				if err == nil && response.Content != nil {
					contentRequests++
					// The fixture supplies a lease without measuring the engine's copy.
					response, err = Handle(b.Context(), request)
				}
				if err != nil {
					b.Fatal(err)
				}
				if (len(response.Changes) != 0) != change {
					b.Fatalf("unexpected changes: %+v", response.Changes)
				}
			}
			b.ReportMetric(float64(len(server.operations())-before)/float64(b.N), "requests/op")
			b.ReportMetric(float64(contentRequests)/float64(b.N), "content_requests/op")
		})
	}
}
