package evalharness

import (
	"path/filepath"
	"testing"
)

// Full harness cost includes fresh fake-provider/SQLite setup and teardown.
// The same scope/lifecycle fixture is reused; seed time cannot masquerade as
// selector or compiler latency, which are reported by separate benchmarks.
func BenchmarkContextQueryPairedHarness(b *testing.B) {
	fixture, err := LoadSuiteFixture(filepath.Join("..", "..", "evals", "context-routing", "cases.yaml"))
	if err != nil {
		b.Fatal(err)
	}
	for _, profile := range []string{"legacy", "candidate"} {
		b.Run(profile, func(b *testing.B) {
			c := fixture.Cases[1]
			c.QueryProfileOverride = profile
			b.ReportAllocs()
			for b.Loop() {
				result, err := runCase(b.Context(), fixture, c)
				if err != nil || !result.Passed {
					b.Fatalf("err=%v findings=%+v", err, result.Findings)
				}
				if profile == "candidate" && result.Metrics.ContextDelivery.Requests[0].QueryAttribution == nil {
					b.Fatal("candidate harness did not exercise attribution")
				}
			}
		})
	}
}
