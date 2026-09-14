package improve

import (
	"fmt"
	"path/filepath"
	"testing"
)

var analyticsBatchCandidates = []int{1, 8, 16, 32, 64, 128, 256}

func BenchmarkSQLiteAnalyticsExecutionBatchSizes(b *testing.B) {
	benchmarkSQLiteAnalyticsExecutionBatchSizes(b, analyticsBenchmarkProfile{name: "medium", events: 100_000, runs: 1_000})
}

func BenchmarkSQLiteAnalyticsExecutionBatchSizesTiny(b *testing.B) {
	benchmarkSQLiteAnalyticsExecutionBatchSizes(b, analyticsBenchmarkProfile{name: "tiny", events: 100, runs: 5})
}

func benchmarkSQLiteAnalyticsExecutionBatchSizes(b *testing.B, profile analyticsBenchmarkProfile) {
	workspace, _ := writeAnalyticsBenchmarkFixture(b, profile)
	path := filepath.Join(workspace, eventsPath)
	for _, candidate := range analyticsBatchCandidates {
		b.Run(fmt.Sprintf("batch=%d", candidate), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				session, err := openSQLiteAnalyticsSession(b.Context())
				if err != nil {
					b.Fatal(err)
				}
				session.batchConfig.Execution = candidate
				session.batchConfig.Skills = candidate
				if _, err := session.loadExecutionEvents(b.Context(), path); err != nil {
					b.Fatal(err)
				}
				if err := session.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSQLiteAnalyticsAuditBatchSizes(b *testing.B) {
	profile := analyticsBenchmarkProfile{name: "medium", events: 100_000, runs: 1_000}
	workspace, _ := writeAnalyticsBenchmarkFixture(b, profile)
	path := filepath.Join(workspace, "logs", "audit")
	for _, candidate := range analyticsBatchCandidates {
		b.Run(fmt.Sprintf("batch=%d", candidate), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				session, err := openSQLiteAnalyticsSession(b.Context())
				if err != nil {
					b.Fatal(err)
				}
				session.batchConfig.Audit = candidate
				if _, err := session.loadAuditEvents(b.Context(), path); err != nil {
					b.Fatal(err)
				}
				if err := session.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkSQLiteAnalyticsMemoryBatchSizes(b *testing.B) {
	workspace := b.TempDir()
	writeBenchmarkMemoryEvents(b, workspace, analyticsBenchmarkProfile{name: "memory", events: 10_000_000, runs: 1_000})
	for _, candidate := range analyticsBatchCandidates {
		b.Run(fmt.Sprintf("batch=%d", candidate), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				session, err := openSQLiteAnalyticsSession(b.Context())
				if err != nil {
					b.Fatal(err)
				}
				session.batchConfig.Memory = candidate
				if _, err := session.loadMemoryEvents(b.Context(), workspace); err != nil {
					b.Fatal(err)
				}
				if err := session.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
