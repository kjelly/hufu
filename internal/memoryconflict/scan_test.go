package memoryconflict

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
)

type scriptedGenerator struct {
	replies []string
	err     error
	calls   int
}

func (g *scriptedGenerator) GenerateText(context.Context, string) (string, error) {
	g.calls++
	if g.err != nil {
		return "", g.err
	}
	if len(g.replies) == 0 {
		return `{"verdict":"compatible","rationale":""}`, nil
	}
	reply := g.replies[0]
	if len(g.replies) > 1 {
		g.replies = g.replies[1:]
	}
	return reply, nil
}

func openScanRepository(t *testing.T, contents ...string) *contextstore.SQLiteRepository {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	for i, content := range contents {
		item := contextstore.ContextItem{ID: fmt.Sprintf("item-%02d", i), Kind: contextstore.ContextDecision, Content: content, Scope: contextstore.Scope{ProjectID: "p", TeamID: "t"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
		if err = repo.Append(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

var storageMemories = []string{
	"Use SQLite database storage for session state",
	"Use PostgreSQL database storage for session state",
	"Use MySQL database storage for session state",
}

func scanOptions() ScanOptions {
	return ScanOptions{ProjectID: "p", TeamID: "t", JudgeModel: "judge-model", Actor: "operator"}
}

func TestScanPersistsJudgmentsAndSkipsJudgedPairsWithoutModelCalls(t *testing.T) {
	ctx := context.Background()
	repo := openScanRepository(t, storageMemories...)
	generator := &scriptedGenerator{replies: []string{`{"verdict":"contradicts","rationale":"Different engines."}`}}
	report, err := Scan(ctx, repo, JSONJudge{Generator: generator}, scanOptions(), PairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.EligibleItems != 3 || report.CandidatePairs != 3 || report.Judged != 3 || report.Contradictions != 3 || generator.calls != 3 {
		t.Fatalf("first scan = %+v calls=%d", report, generator.calls)
	}
	open, err := repo.ListConflicts(ctx, contextstore.ConflictQuery{ProjectID: "p", TeamID: "t"})
	if err != nil || len(open) != 3 {
		t.Fatalf("open conflicts = %d err=%v", len(open), err)
	}
	again, err := Scan(ctx, repo, JSONJudge{Generator: generator}, scanOptions(), PairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.AlreadyJudged != 3 || again.Judged != 0 || generator.calls != 3 {
		t.Fatalf("second scan = %+v calls=%d, want no model calls", again, generator.calls)
	}
}

func TestScanSkipsOversizedPrompt(t *testing.T) {
	repo := openScanRepository(t, storageMemories[:2]...)
	generator := &scriptedGenerator{}
	opts := scanOptions()
	opts.MaxPromptRunes = 50
	report, err := Scan(context.Background(), repo, JSONJudge{Generator: generator}, opts, PairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Skipped != 1 || report.Judged != 0 || generator.calls != 0 || report.Diagnostics[0].Reason != ReasonInputTooLarge {
		t.Fatalf("report = %+v calls=%d", report, generator.calls)
	}
}

func TestScanPersistsUndeterminedAndStopsAfterThree(t *testing.T) {
	ctx := context.Background()
	repo := openScanRepository(t, append(storageMemories, "Use Oracle database storage for session state")...)
	generator := &scriptedGenerator{replies: []string{"not json"}}
	report, err := Scan(ctx, repo, JSONJudge{Generator: generator}, scanOptions(), PairOptions{})
	if err == nil || !strings.Contains(err.Error(), "3 consecutive pairs") {
		t.Fatalf("err = %v, want consecutive invalid stop", err)
	}
	if report.Undetermined != 3 || generator.calls != 3 {
		t.Fatalf("report = %+v calls=%d", report, generator.calls)
	}
	// Undetermined pairs are not retried unless asked.
	generator = &scriptedGenerator{replies: []string{`{"verdict":"compatible","rationale":""}`}}
	again, err := Scan(ctx, repo, JSONJudge{Generator: generator}, scanOptions(), PairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if again.AlreadyJudged != 3 || again.Judged != 3 {
		t.Fatalf("second scan = %+v", again)
	}
	retry := scanOptions()
	retry.RetryUndetermined = true
	retried, err := Scan(ctx, repo, JSONJudge{Generator: generator}, retry, PairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if retried.Judged != 3 || retried.Undetermined != 0 {
		t.Fatalf("retry scan = %+v", retried)
	}
}

func TestScanStopsOnTransportErrorKeepsCommitted(t *testing.T) {
	ctx := context.Background()
	repo := openScanRepository(t, storageMemories...)
	first := &scriptedGenerator{}
	opts := scanOptions()
	opts.MaxPairs = 1
	if _, err := Scan(ctx, repo, JSONJudge{Generator: first}, opts, PairOptions{}); err != nil {
		t.Fatal(err)
	}
	failing := &scriptedGenerator{err: errors.New("connection refused")}
	report, err := Scan(ctx, repo, JSONJudge{Generator: failing}, scanOptions(), PairOptions{})
	if err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("err = %v, want transport error", err)
	}
	if report.AlreadyJudged != 1 || failing.calls != 1 {
		t.Fatalf("report = %+v calls=%d", report, failing.calls)
	}
	all, err := repo.ListConflicts(ctx, contextstore.ConflictQuery{ProjectID: "p", TeamID: "t", IncludeInactive: true})
	if err != nil || len(all) != 0 {
		t.Fatalf("conflicts = %d err=%v, compatible judgment is not a conflict", len(all), err)
	}
}

func TestScanRespectsMaxPairsAndDryRun(t *testing.T) {
	ctx := context.Background()
	repo := openScanRepository(t, storageMemories...)
	generator := &scriptedGenerator{}
	opts := scanOptions()
	opts.MaxPairs = 2
	opts.DryRun = true
	report, err := Scan(ctx, repo, JSONJudge{Generator: generator}, opts, PairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || report.Judged != 2 || report.Deferred != 1 || generator.calls != 0 {
		t.Fatalf("dry run = %+v calls=%d", report, generator.calls)
	}
	opts.DryRun = false
	report, err = Scan(ctx, repo, JSONJudge{Generator: generator}, opts, PairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Judged != 2 || report.Deferred != 1 || report.AlreadyJudged != 0 || generator.calls != 2 {
		t.Fatalf("capped scan = %+v calls=%d (dry run must not persist)", report, generator.calls)
	}
}

func TestScanReportsSecretLikeMemories(t *testing.T) {
	repo := openScanRepository(t, "Use SQLite database storage for session state")
	// The repository redacts content on write, so a stored secret shows up as
	// a redaction marker and must be excluded from judging.
	if err := repo.Append(context.Background(), contextstore.ContextItem{ID: "secret", Kind: contextstore.ContextDecision, Content: "Connect with api_key=sk-live-abcdefghijklmnopqrstu for storage", Scope: contextstore.Scope{ProjectID: "p", TeamID: "t"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}); err != nil {
		t.Fatal(err)
	}
	report, err := Scan(context.Background(), repo, JSONJudge{Generator: &scriptedGenerator{}}, scanOptions(), PairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.EligibleItems != 1 || len(report.Diagnostics) != 1 || report.Diagnostics[0].ItemID != "secret" || report.Diagnostics[0].Reason != ReasonSecretLikeContent {
		t.Fatalf("report = %+v", report)
	}
}
