package evalharness

import (
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// This baseline includes fake-provider/SQLite setup and teardown in allocs/op.
// Selection timings come from the diagnostic stages, not benchmark wall time.
func BenchmarkContextDeliveryBaseline(b *testing.B) {
	fixture, err := LoadSuiteFixture(filepath.Join("..", "..", "evals", "context-routing", "cases.yaml"))
	if err != nil {
		b.Fatal(err)
	}
	var measured []EvalSuiteResult
	var selection []float64
	stages := make(map[string][]float64)
	var sourceRows, queryCalls int
	b.ReportAllocs()
	for b.Loop() {
		result, err := RunSuite(b.Context(), fixture, "provider-visible-scope-lifecycle")
		if err != nil || !result.Passed() {
			b.Fatalf("baseline: %+v, %v", result, err)
		}
		measured = append(measured, result)
		for _, request := range result.Cases[0].Metrics.ContextDelivery.Requests {
			if request.SelectionLatencyMS == nil || request.RetrievalCalls == nil {
				b.Fatal("baseline selection measurement unavailable")
			}
			selection = append(selection, *request.SelectionLatencyMS)
			for stage, elapsed := range request.StageLatencyMS {
				stages[stage] = append(stages[stage], elapsed)
			}
			sourceRows += request.RetrievalCalls.CandidateRows
			queryCalls += request.RetrievalCalls.Exact + request.RetrievalCalls.Lexical + request.RetrievalCalls.Vector
		}
	}
	reportPercentiles := func(name string, values []float64) {
		slices.Sort(values)
		for _, percentile := range []struct {
			fraction float64
			label    string
		}{{.5, "p50"}, {.95, "p95"}} {
			index := max(0, int(math.Ceil(percentile.fraction*float64(len(values))))-1)
			b.ReportMetric(values[index], name+"_"+percentile.label+"_ms")
		}
	}
	reportPercentiles("worker_selection", selection)
	for stage, values := range stages {
		reportPercentiles("worker_"+stage, values)
	}
	b.ReportMetric(float64(sourceRows)/float64(len(selection)), "worker_source_rows/op")
	b.ReportMetric(float64(queryCalls)/float64(len(selection)), "worker_retrieval_calls/op")
	if path := os.Getenv("HUFU_CONTEXT_DELIVERY_MEASURED_REPORT"); path != "" {
		file, err := os.Create(path)
		if err != nil {
			b.Fatal(err)
		}
		err = WriteMeasuredJSONReport(file, measured)
		closeErr := file.Close()
		if err != nil {
			b.Fatal(err)
		}
		if closeErr != nil {
			b.Fatal(closeErr)
		}
	}
}
