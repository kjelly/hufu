package context

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkCandidateSettlementQuery(b *testing.B) {
	for _, rows := range []int{10_000, 100_000} {
		b.Run(fmt.Sprintf("rows=%d", rows), func(b *testing.B) {
			repo, err := OpenSQLite(filepath.Join(b.TempDir(), "context.sqlite"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = repo.Close() })
			writeCandidateSettlementBenchmarkFixture(b, repo, rows)

			queries := []struct {
				name  string
				query RepositoryQuery
			}{
				{name: "legacy", query: RepositoryQuery{
					Scope: Scope{ProjectID: "project", TeamID: "team"}, Visibility: VisibilityExact,
					IncludeCandidates: true, Limit: rows + 1,
				}},
				{name: "typed", query: RepositoryQuery{
					Scope: Scope{ProjectID: "project", TeamID: "team"}, Visibility: VisibilityExact,
					Lifecycles: []ContextLifecycle{LifecycleCandidate}, OriginRunID: "target-run",
					SourceTypes: []string{"run_shared_context"}, Limit: rows + 1,
				}},
			}
			for _, benchmark := range queries {
				b.Run(benchmark.name, func(b *testing.B) {
					b.ReportAllocs()
					decoded := 0
					for b.Loop() {
						items, queryErr := repo.Query(b.Context(), benchmark.query)
						if queryErr != nil {
							b.Fatal(queryErr)
						}
						decoded = len(items)
					}
					b.ReportMetric(float64(decoded), "rows_decoded/op")
					b.ReportMetric(float64(decoded), "rows_returned/op")
				})
			}
		})
	}
}

func writeCandidateSettlementBenchmarkFixture(tb testing.TB, repo *SQLiteRepository, rowCount int) {
	tb.Helper()
	tx, err := repo.db.BeginTx(context.Background(), nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	statement, err := tx.PrepareContext(context.Background(), "INSERT INTO context_items ("+itemColumns+") VALUES ("+strings.TrimSuffix(strings.Repeat("?,", 30), ",")+")")
	if err != nil {
		tb.Fatal(err)
	}
	defer func() { _ = statement.Close() }()
	for i := range rowCount {
		id := fmt.Sprintf("item-%06d", i)
		content := "candidate settlement benchmark " + id
		hash := sha256.Sum256([]byte(content))
		lifecycle, runID, sourceType := LifecycleConfirmed, "other-run", "other"
		if i%100 == 0 {
			lifecycle, runID, sourceType = LifecycleCandidate, "target-run", "run_shared_context"
		} else if i%10 == 0 {
			lifecycle, sourceType = LifecycleCandidate, "run_shared_context"
		}
		if _, err := statement.ExecContext(context.Background(),
			id, ContextObservation, content, hex.EncodeToString(hash[:]), "project", "team",
			nil, nil, nil, nil, nil, AuthorityAgent, TrustInternal, i%4, 0, 0, 1.0,
			mustJSON(SourceRef{Type: sourceType}), "[]", "[]", mustJSON(map[string]string{"run_id": runID}),
			int64(i), int64(i), nil, nil, nil, nil, string(lifecycle), "pending", nil,
		); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}
