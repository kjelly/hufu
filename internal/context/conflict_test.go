package context

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openConflictTestRepository(t *testing.T) (*SQLiteRepository, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "context.sqlite")
	repo, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo, path
}

func appendConflictItem(t *testing.T, repo *SQLiteRepository, id, content string) ContextItem {
	t.Helper()
	item := ContextItem{ID: id, Kind: ContextDecision, Content: content, Scope: Scope{ProjectID: "p", TeamID: "t"}, Lifecycle: LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
	if err := repo.Append(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func newTestJudgment(a, b ContextItem, verdict PairVerdict, version string) PairJudgment {
	j := PairJudgment{ProjectID: "p", TeamID: "t", ItemAID: a.ID, ItemBID: b.ID, ItemAContentHash: a.ContentHash, ItemBContentHash: b.ContentHash, Verdict: verdict, JudgePolicyVersion: version, JudgeModel: "judge-model", Rationale: "They disagree."}
	NormalizePairJudgment(&j)
	return j
}

func pendingEventTypes(t *testing.T, repo *SQLiteRepository) []string {
	t.Helper()
	events, err := repo.PendingPromotionEvents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.EventType+":"+event.IdempotencyKey)
	}
	return types
}

func TestMigration11CreatesPairJudgments(t *testing.T) {
	repo, path := openConflictTestRepository(t)
	a := appendConflictItem(t, repo, "a", "Use SQLite for storage.")
	b := appendConflictItem(t, repo, "b", "Use PostgreSQL for storage.")
	j := newTestJudgment(a, b, PairVerdictContradicts, CurrentConflictJudgePolicyVersion)
	if _, _, err := repo.SavePairJudgment(context.Background(), j, "operator", false); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, found, err := reopened.LookupPairJudgment(context.Background(), j.ID); err != nil || !found {
		t.Fatalf("judgment after reopen found=%v err=%v", found, err)
	}
}

func TestSchemaCheckRejectsInvalidVerdictStatus(t *testing.T) {
	repo, _ := openConflictTestRepository(t)
	cases := []struct {
		name    string
		a, b    string
		verdict string
		status  string
		ok      bool
	}{
		{name: "open contradiction", a: "a", b: "b", verdict: "contradicts", status: "open", ok: true},
		{name: "dismissed contradiction", a: "a", b: "c", verdict: "contradicts", status: "dismissed", ok: true},
		{name: "compatible not applicable", a: "a", b: "d", verdict: "compatible", status: "not_applicable", ok: true},
		{name: "contradiction not applicable", a: "a", b: "e", verdict: "contradicts", status: "not_applicable"},
		{name: "compatible open", a: "a", b: "f", verdict: "compatible", status: "open"},
		{name: "unknown verdict", a: "a", b: "g", verdict: "maybe", status: "not_applicable"},
		{name: "unordered pair", a: "z", b: "a", verdict: "compatible", status: "not_applicable"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := repo.db.Exec("INSERT INTO context_pair_judgments("+pairJudgmentColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", fmt.Sprintf("id-%d", i), "p", "t", "", tc.a, tc.b, "ha", "hb", tc.verdict, tc.status, "v", "m", "", "", 1, 1)
			if tc.ok != (err == nil) {
				t.Fatalf("insert err = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestPairJudgmentIDIsDeterministic(t *testing.T) {
	base := PairJudgment{ProjectID: "p", TeamID: "t", ItemAID: "a", ItemBID: "b", ItemAContentHash: "ha", ItemBContentHash: "hb", JudgePolicyVersion: "v1"}
	NormalizePairJudgment(&base)
	cases := []struct {
		name string
		edit func(*PairJudgment)
		same bool
	}{
		{name: "swapped order", edit: func(j *PairJudgment) {
			j.ItemAID, j.ItemBID, j.ItemAContentHash, j.ItemBContentHash = "b", "a", "hb", "ha"
		}, same: true},
		{name: "content changed", edit: func(j *PairJudgment) { j.ItemAContentHash = "ha2" }},
		{name: "judge version changed", edit: func(j *PairJudgment) { j.JudgePolicyVersion = "v2" }},
		{name: "scope changed", edit: func(j *PairJudgment) { j.AgentID = "worker" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := PairJudgment{ProjectID: "p", TeamID: "t", ItemAID: "a", ItemBID: "b", ItemAContentHash: "ha", ItemBContentHash: "hb", JudgePolicyVersion: "v1"}
			tc.edit(&j)
			NormalizePairJudgment(&j)
			if (j.ID == base.ID) != tc.same {
				t.Fatalf("ID %s vs base %s, want same=%v", j.ID, base.ID, tc.same)
			}
		})
	}
}

func TestSavePairJudgmentKeepsExistingRowAndReplacesUndeterminedOnlyWhenAsked(t *testing.T) {
	ctx := context.Background()
	repo, _ := openConflictTestRepository(t)
	a := appendConflictItem(t, repo, "a", "Use SQLite for storage.")
	b := appendConflictItem(t, repo, "b", "Use PostgreSQL for storage.")
	undetermined := newTestJudgment(a, b, PairVerdictUndetermined, CurrentConflictJudgePolicyVersion)
	if _, written, err := repo.SavePairJudgment(ctx, undetermined, "operator", false); err != nil || !written {
		t.Fatalf("first save written=%v err=%v", written, err)
	}
	contradiction := newTestJudgment(a, b, PairVerdictContradicts, CurrentConflictJudgePolicyVersion)
	if stored, written, err := repo.SavePairJudgment(ctx, contradiction, "operator", false); err != nil || written || stored.Verdict != PairVerdictUndetermined {
		t.Fatalf("save without replace = %+v written=%v err=%v", stored, written, err)
	}
	if got := pendingEventTypes(t, repo); len(got) != 0 {
		t.Fatalf("undetermined judgment enqueued events %v", got)
	}
	stored, written, err := repo.SavePairJudgment(ctx, contradiction, "operator", true)
	if err != nil || !written || stored.Verdict != PairVerdictContradicts || stored.Status != PairJudgmentOpen {
		t.Fatalf("replace = %+v written=%v err=%v", stored, written, err)
	}
	if got := pendingEventTypes(t, repo); len(got) != 1 || !strings.HasPrefix(got[0], "memory_conflict_detected:") {
		t.Fatalf("pending events = %v, want one detected event", got)
	}
	if again, written, err := repo.SavePairJudgment(ctx, newTestJudgment(a, b, PairVerdictCompatible, CurrentConflictJudgePolicyVersion), "operator", true); err != nil || written || again.Verdict != PairVerdictContradicts {
		t.Fatalf("determined row must not be replaced: %+v written=%v err=%v", again, written, err)
	}
}

func TestNewContradictionInheritsDismissalAcrossJudgeVersions(t *testing.T) {
	ctx := context.Background()
	repo, _ := openConflictTestRepository(t)
	a := appendConflictItem(t, repo, "a", "Use SQLite for storage.")
	b := appendConflictItem(t, repo, "b", "Use PostgreSQL for storage.")
	first := newTestJudgment(a, b, PairVerdictContradicts, CurrentConflictJudgePolicyVersion)
	if _, _, err := repo.SavePairJudgment(ctx, first, "operator", false); err != nil {
		t.Fatal(err)
	}
	if _, dismissed, err := repo.DismissConflict(ctx, first.ID, "p", "t", "Different deployment targets.", "operator"); err != nil || !dismissed {
		t.Fatalf("dismiss dismissed=%v err=%v", dismissed, err)
	}
	before := len(pendingEventTypes(t, repo))
	rejudged := newTestJudgment(a, b, PairVerdictContradicts, "conflict-judge-v2")
	stored, written, err := repo.SavePairJudgment(ctx, rejudged, "operator", false)
	if err != nil || !written {
		t.Fatalf("rejudge written=%v err=%v", written, err)
	}
	if stored.Status != PairJudgmentDismissed || stored.DismissReason != "Different deployment targets." {
		t.Fatalf("rejudged conflict = %+v, want inherited dismissal", stored)
	}
	if after := len(pendingEventTypes(t, repo)); after != before {
		t.Fatalf("inherited dismissal enqueued %d new events", after-before)
	}
}

func TestSavePairJudgmentSanitizesRationale(t *testing.T) {
	ctx := context.Background()
	repo, _ := openConflictTestRepository(t)
	a := appendConflictItem(t, repo, "a", "Use SQLite for storage.")
	b := appendConflictItem(t, repo, "b", "Use PostgreSQL for storage.")
	j := newTestJudgment(a, b, PairVerdictCompatible, CurrentConflictJudgePolicyVersion)
	j.Rationale = strings.Repeat("x", 2000) + " api_key=sk-live-abcdefghijklmnopqrstu"
	stored, _, err := repo.SavePairJudgment(ctx, j, "operator", false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(stored.Rationale)); n > pairRationaleMaxRunes {
		t.Fatalf("rationale has %d runes, want <= %d", n, pairRationaleMaxRunes)
	}
	j.Rationale = "api_key=sk-live-abcdefghijklmnopqrstu"
	j.ItemBContentHash = "other"
	NormalizePairJudgment(&j)
	stored, _, err = repo.SavePairJudgment(ctx, j, "operator", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.Rationale, "sk-live-abcdefghijklmnopqrstu") {
		t.Fatalf("rationale kept a secret: %q", stored.Rationale)
	}
}

func TestConflictStateDerivation(t *testing.T) {
	now := time.Now()
	current := conflictItemState{ContentHash: "ha", Lifecycle: LifecycleConfirmed}
	currentB := conflictItemState{ContentHash: "hb", Lifecycle: LifecycleConfirmed}
	base := PairJudgment{ItemAID: "a", ItemBID: "b", ItemAContentHash: "ha", ItemBContentHash: "hb", Verdict: PairVerdictContradicts, Status: PairJudgmentOpen, JudgePolicyVersion: CurrentConflictJudgePolicyVersion}
	cases := []struct {
		name   string
		edit   func(*PairJudgment)
		states map[string]conflictItemState
		want   ConflictState
	}{
		{name: "both current", states: map[string]conflictItemState{"a": current, "b": currentB}, want: ConflictStateOpen},
		{name: "dismissed wins", edit: func(j *PairJudgment) { j.Status = PairJudgmentDismissed }, states: map[string]conflictItemState{"a": current, "b": currentB}, want: ConflictStateDismissed},
		{name: "older judge version", edit: func(j *PairJudgment) { j.JudgePolicyVersion = "conflict-judge-v0" }, states: map[string]conflictItemState{"a": current, "b": currentB}, want: ConflictStateInactive},
		{name: "one superseded", states: map[string]conflictItemState{"a": {ContentHash: "ha", Lifecycle: LifecycleConfirmed, SupersededBy: "c"}, "b": currentB}, want: ConflictStateResolvedBySupersede},
		{name: "one deleted", states: map[string]conflictItemState{"a": current}, want: ConflictStateInactive},
		{name: "one expired", states: map[string]conflictItemState{"a": {ContentHash: "ha", Lifecycle: LifecycleConfirmed, ExpiresAt: sql.NullInt64{Int64: now.Add(-time.Hour).UnixMilli(), Valid: true}}, "b": currentB}, want: ConflictStateInactive},
		{name: "content changed", states: map[string]conflictItemState{"a": {ContentHash: "changed", Lifecycle: LifecycleConfirmed}, "b": currentB}, want: ConflictStateInactive},
		{name: "rejected", states: map[string]conflictItemState{"a": {ContentHash: "ha", Lifecycle: LifecycleRejected}, "b": currentB}, want: ConflictStateInactive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := base
			if tc.edit != nil {
				tc.edit(&j)
			}
			if got := deriveConflictState(j, tc.states, now); got != tc.want {
				t.Fatalf("state = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestDismissConflictStateMachine(t *testing.T) {
	ctx := context.Background()
	repo, _ := openConflictTestRepository(t)
	a := appendConflictItem(t, repo, "a", "Use SQLite for storage.")
	b := appendConflictItem(t, repo, "b", "Use PostgreSQL for storage.")
	c := appendConflictItem(t, repo, "c", "Use MySQL for storage.")
	open := newTestJudgment(a, b, PairVerdictContradicts, CurrentConflictJudgePolicyVersion)
	resolved := newTestJudgment(a, c, PairVerdictContradicts, CurrentConflictJudgePolicyVersion)
	for _, j := range []PairJudgment{open, resolved} {
		if _, _, err := repo.SavePairJudgment(ctx, j, "operator", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.MarkSuperseded(ctx, []string{"c"}, "a"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name        string
		id, reason  string
		wantErr     string
		wantWritten bool
	}{
		{name: "empty reason", id: open.ID, reason: "  ", wantErr: "reason is required"},
		{name: "secret reason", id: open.ID, reason: "token=ghp_1234567890abcdef1234567890abcdef", wantErr: "secret-like"},
		{name: "resolved conflict", id: resolved.ID, reason: "Not relevant.", wantErr: "resolved_by_supersede"},
		{name: "wrong scope", id: "conflict-missing", reason: "Not relevant.", wantErr: "not found"},
		{name: "open conflict", id: open.ID, reason: "Different environments.", wantWritten: true},
		{name: "already dismissed is idempotent", id: open.ID, reason: "Again.", wantWritten: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			view, written, err := repo.DismissConflict(ctx, tc.id, "p", "t", tc.reason, "operator")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || written != tc.wantWritten || view.State != ConflictStateDismissed {
				t.Fatalf("view=%+v written=%v err=%v", view, written, err)
			}
		})
	}
	dismissedEvents := 0
	for _, event := range pendingEventTypes(t, repo) {
		if strings.HasPrefix(event, "memory_conflict_dismissed:") {
			dismissedEvents++
		}
	}
	if dismissedEvents != 1 {
		t.Fatalf("dismissed events = %d, want 1", dismissedEvents)
	}
}

func TestListConflictsAndOpenConflictsForItems(t *testing.T) {
	ctx := context.Background()
	repo, _ := openConflictTestRepository(t)
	a := appendConflictItem(t, repo, "a", "Use SQLite for storage.")
	b := appendConflictItem(t, repo, "b", "Use PostgreSQL for storage.")
	c := appendConflictItem(t, repo, "c", "Deploy on Fridays.")
	open := newTestJudgment(a, b, PairVerdictContradicts, CurrentConflictJudgePolicyVersion)
	compatible := newTestJudgment(a, c, PairVerdictCompatible, CurrentConflictJudgePolicyVersion)
	for _, j := range []PairJudgment{open, compatible} {
		if _, _, err := repo.SavePairJudgment(ctx, j, "operator", false); err != nil {
			t.Fatal(err)
		}
	}
	listed, err := repo.ListConflicts(ctx, ConflictQuery{ProjectID: "p", TeamID: "t"})
	if err != nil || len(listed) != 1 || listed[0].ID != open.ID || listed[0].ItemAKind != ContextDecision {
		t.Fatalf("ListConflicts = %+v err=%v", listed, err)
	}
	byItem, err := repo.OpenConflictsForItems(ctx, "p", "t", []string{"a", "c", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byItem) != 1 || len(byItem["a"]) != 1 || byItem["a"][0] != open.ID {
		t.Fatalf("OpenConflictsForItems = %v", byItem)
	}
	if other, err := repo.OpenConflictsForItems(ctx, "p", "other-team", []string{"a"}); err != nil || len(other) != 0 {
		t.Fatalf("other team conflicts = %v err=%v", other, err)
	}
}

func TestOpenConflictsForItemsChunksLargeInput(t *testing.T) {
	ctx := context.Background()
	repo, _ := openConflictTestRepository(t)
	a := appendConflictItem(t, repo, "a", "Use SQLite for storage.")
	b := appendConflictItem(t, repo, "zz", "Use PostgreSQL for storage.")
	j := newTestJudgment(a, b, PairVerdictContradicts, CurrentConflictJudgePolicyVersion)
	if _, _, err := repo.SavePairJudgment(ctx, j, "operator", false); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 1203)
	for i := range 1200 {
		ids = append(ids, fmt.Sprintf("filler-%04d", i))
	}
	ids = append(ids, "zz", "a", "a")
	byItem, err := repo.OpenConflictsForItems(ctx, "p", "t", ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(byItem["a"]) != 1 || len(byItem["zz"]) != 1 {
		t.Fatalf("OpenConflictsForItems over chunks = %v", byItem)
	}
}

func TestReadOnlyRepositoryBeforeMigration11(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "context.sqlite")
	repo, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.db.ExecContext(ctx, `DROP TABLE context_pair_judgments; DELETE FROM schema_migrations WHERE version >= 11`); err != nil {
		t.Fatal(err)
	}
	if err = repo.Close(); err != nil {
		t.Fatal(err)
	}
	readOnly, err := OpenSQLiteReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if _, err = readOnly.ListConflicts(ctx, ConflictQuery{ProjectID: "p", TeamID: "t"}); err != ErrConflictsUnavailable {
		t.Fatalf("ListConflicts err = %v, want ErrConflictsUnavailable", err)
	}
	if _, err = readOnly.GetConflict(ctx, "conflict-x", "p", "t"); err != ErrConflictsUnavailable {
		t.Fatalf("GetConflict err = %v, want ErrConflictsUnavailable", err)
	}
	if byItem, err := readOnly.OpenConflictsForItems(ctx, "p", "t", []string{"a"}); err != nil || len(byItem) != 0 {
		t.Fatalf("OpenConflictsForItems = %v err=%v, want empty", byItem, err)
	}
}
