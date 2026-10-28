package context

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"modernc.org/sqlite"
)

func BenchmarkLineageRetrieval(b *testing.B) {
	for _, count := range []int{1_000, 10_000, 100_000, 100_002} {
		b.Run(fmt.Sprintf("rows=%d", count), func(b *testing.B) {
			repo := lineageRetrievalFixture(b, count, false)
			allowed := []string{"item-000000", "item-000002", "item-000004", "item-000006"}
			req := SearchRequest{Query: "anchor", Scope: lineageRetrievalScope(), Limit: 4}.WithAllowedItemIDs(allowed)
			index := lineageBenchmarkIndex(b, repo)
			var hydrated atomic.Int64
			observeLineageHydration(b, repo, &hydrated)
			lanes := []struct {
				name   string
				search func(context.Context, SearchRequest) ([]SearchResult, error)
			}{{"exact", repo.SearchExact}, {"fts", repo.SearchLexical}, {"vector", index.SearchVector}}
			for _, lane := range lanes {
				for sample := range 5 {
					b.Run(fmt.Sprintf("%s/sample=%d", lane.name, sample), func(b *testing.B) {
						// Warm the prepared query, SQLite pages and vector snapshot.
						if _, err := lane.search(b.Context(), req); err != nil {
							b.Fatal(err)
						}
						b.ReportAllocs()
						hydrated.Store(0)
						var rows []SearchResult
						for b.Loop() {
							var err error
							rows, err = lane.search(b.Context(), req)
							if err != nil {
								b.Fatal(err)
							}
						}
						b.ReportMetric(float64(len(rows)), "rows_returned/op")
						b.ReportMetric(float64(hydrated.Load())/float64(b.N), "canonical_rows/op")
						b.Logf("ordered IDs: %v", searchResultIDs(rows))
					})
				}
			}
		})
	}
}

// Count successful driver row reads for canonical content queries, including
// rows an old iterator hydrates before its in-memory lineage filter. Metadata
// and embedding projection rows are deliberately excluded. The same observer
// runs in both revisions; no production instrumentation or cache is added.
type lineageCountingConnector struct {
	path  string
	count *atomic.Int64
}

func (c lineageCountingConnector) Driver() driver.Driver { return &sqlite.Driver{} }

func (c lineageCountingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	conn, err := c.Driver().Open(c.path)
	if err != nil {
		return nil, err
	}
	queryer, ok := conn.(driver.QueryerContext)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("SQLite benchmark requires QueryerContext")
	}
	beginner, ok := conn.(driver.ConnBeginTx)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("SQLite benchmark requires ConnBeginTx")
	}
	return lineageCountingConn{Conn: conn, queryer: queryer, beginner: beginner, count: c.count}, nil
}

type lineageCountingConn struct {
	driver.Conn
	queryer  driver.QueryerContext
	beginner driver.ConnBeginTx
	count    *atomic.Int64
}

func (c lineageCountingConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.beginner.BeginTx(ctx, options)
}

func (c lineageCountingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.queryer.QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	columns := rows.Columns()
	if len(columns) >= 3 && columns[0] == "id" && columns[1] == "kind" && columns[2] == "content" {
		return lineageCountingRows{Rows: rows, count: c.count}, nil
	}
	return rows, nil
}

type lineageCountingRows struct {
	driver.Rows
	count *atomic.Int64
}

func (r lineageCountingRows) Next(values []driver.Value) error {
	err := r.Rows.Next(values)
	if err == nil {
		r.count.Add(1)
	}
	return err
}

func observeLineageHydration(tb testing.TB, repo *SQLiteRepository, count *atomic.Int64) {
	tb.Helper()
	if err := repo.db.Close(); err != nil {
		tb.Fatal(err)
	}
	repo.db = sql.OpenDB(lineageCountingConnector{path: repo.path, count: count})
	repo.db.SetMaxOpenConns(1)
}

func lineageBenchmarkIndex(tb testing.TB, repo *SQLiteRepository) *SemanticIndex {
	tb.Helper()
	inventory, err := repo.LoadEmbeddingInventory(tb.Context(), "p")
	if err != nil {
		tb.Fatal(err)
	}
	model := projectionTestModel("lineage-benchmark", "1")
	generation, err := repo.BeginGeneration(tb.Context(), GenerationSpec{ProjectID: "p", Model: model,
		SourceRevision: inventory.Revision, ExpectedCount: len(inventory.Items), ContentDigest: inventory.Digest})
	if err != nil {
		tb.Fatal(err)
	}
	rows := projectionRows(inventory, model.Dimensions)
	for start := 0; start < len(rows); start += maxEmbeddingRowsPerBatch {
		if err := repo.AppendGenerationRows(tb.Context(), generation.GenerationID, rows[start:min(start+maxEmbeddingRowsPerBatch, len(rows))]); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err := repo.ActivateGeneration(tb.Context(), generation.GenerationID, inventory.Revision, inventory.Digest); err != nil {
		tb.Fatal(err)
	}
	index, err := NewSemanticIndex(repo, repo, semanticTestEmbedder{model: model, query: []float32{1, 0}})
	if err != nil {
		tb.Fatal(err)
	}
	return index
}

func lineageRetrievalScope() Scope {
	return Scope{ProjectID: "p", TeamID: "team", SessionID: "sess", BranchID: "main", AgentID: "worker"}
}

func lineageRetrievalFixture(tb testing.TB, count int, allMatch bool) *SQLiteRepository {
	tb.Helper()
	repo, err := OpenSQLite(filepath.Join(tb.TempDir(), "context.sqlite"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = repo.Close() })
	tx, err := repo.db.BeginTx(tb.Context(), nil)
	if err != nil {
		tb.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(tb.Context(), `INSERT INTO context_items (`+itemColumns+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		tb.Fatal(err)
	}
	defer stmt.Close()
	for i := range count {
		id := fmt.Sprintf("item-%06d", i)
		content := "unrelated background " + id
		if allMatch || i < 16 {
			content = "anchor migration " + id
		}
		if _, err := stmt.ExecContext(tb.Context(), id, ContextObservation, content, projectionHash(content), "p", "team", "sess", "main", "worker", nil, nil,
			AuthorityAgent, TrustInternal, PriorityNormal, 0, 0, 1.0, "{}", `[{"type":"file_path","ref":"internal/target.go"}]`, "[]", "{}", int64(i+1), int64(i+1), nil, nil, nil, nil, LifecycleConfirmed, "pending", nil); err != nil {
			tb.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	if err := repo.RebuildLexical(tb.Context()); err != nil {
		tb.Fatal(err)
	}
	return repo
}
