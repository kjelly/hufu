package runtimestore

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/sqlmigrate"
)

func insertGrantEvent(t *testing.T, q Queryer, sequence int, ref HistoricalGrantCycleRef, kind string) {
	t.Helper()
	_, err := q.ExecContext(t.Context(), `INSERT INTO events(sequence,id,schema_version,run_id,branch_id,type,hash) VALUES(?,?,1,?,?,?,?)`,
		sequence, ref.EventID, ref.CycleID, ref.BranchID, kind, ref.EventHash)
	if err != nil {
		t.Fatal(err)
	}
}

func TestHistoricalGrantCyclePreservesFirstAdmission(t *testing.T) {
	store, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first := HistoricalGrantCycleRef{BranchID: "main", CycleID: "cycle", EventID: "first", EventHash: strings.Repeat("a", 64)}
	second := first
	second.EventID, second.EventHash = "second", strings.Repeat("b", 64)
	sibling := first
	sibling.BranchID, sibling.EventID = "sibling", "sibling-event"
	for i, ref := range []HistoricalGrantCycleRef{first, second, sibling} {
		insertGrantEvent(t, store.DB(), i+1, ref, historicalGrantEventType)
	}
	for _, tc := range []struct {
		name string
		ref  HistoricalGrantCycleRef
		want error
	}{
		{name: "first admission", ref: first},
		{name: "idempotent replay", ref: first},
		{name: "replacement refused", ref: second, want: ErrHistoricalGrantConflict},
		{name: "branch isolation", ref: sibling},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := PutHistoricalGrantCycleRef(t.Context(), store.DB(), tc.ref); !errors.Is(err, tc.want) {
				t.Fatalf("put reference: %v, want %v", err, tc.want)
			}
		})
	}
	got, found, err := HistoricalGrantCycle(t.Context(), store.DB(), "main", "cycle")
	if err != nil || !found || got != first {
		t.Fatalf("first admission changed: %+v found=%v err=%v", got, found, err)
	}
	if _, found, err := HistoricalGrantCycle(t.Context(), store.DB(), "main", "another-cycle"); err != nil || found {
		t.Fatalf("cross-cycle reference: found=%v err=%v", found, err)
	}
}

func TestHistoricalGrantCycleRejectsMismatchedSources(t *testing.T) {
	store, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	good := HistoricalGrantCycleRef{BranchID: "main", CycleID: "cycle", EventID: "grant", EventHash: strings.Repeat("a", 64)}
	insertGrantEvent(t, store.DB(), 1, good, historicalGrantEventType)
	other := good
	other.EventID = "ordinary"
	insertGrantEvent(t, store.DB(), 2, other, "task_created")
	for _, tc := range []struct {
		name   string
		change func(*HistoricalGrantCycleRef)
	}{
		{name: "missing event", change: func(r *HistoricalGrantCycleRef) { r.EventID = "missing" }},
		{name: "different event type", change: func(r *HistoricalGrantCycleRef) { r.EventID = "ordinary" }},
		{name: "source hash", change: func(r *HistoricalGrantCycleRef) { r.EventHash = strings.Repeat("b", 64) }},
		{name: "consumer branch", change: func(r *HistoricalGrantCycleRef) { r.BranchID = "sibling" }},
		{name: "consumer cycle", change: func(r *HistoricalGrantCycleRef) { r.CycleID = "another" }},
		{name: "empty identity", change: func(r *HistoricalGrantCycleRef) { r.BranchID = "" }},
		{name: "invalid hash", change: func(r *HistoricalGrantCycleRef) { r.EventHash = strings.Repeat("z", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := good
			tc.change(&ref)
			if err := PutHistoricalGrantCycleRef(t.Context(), store.DB(), ref); !errors.Is(err, ErrHistoricalGrantSource) {
				t.Fatalf("accepted mismatched source: %v", err)
			}
		})
	}
	if err := PutHistoricalGrantCycleRef(t.Context(), store.DB(), good); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `UPDATE historical_grant_cycles SET source_hash=?`, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := HistoricalGrantCycle(t.Context(), store.DB(), "main", "cycle"); !errors.Is(err, ErrHistoricalGrantSource) {
		t.Fatalf("accepted a tampered index: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := HistoricalGrantCycle(ctx, store.DB(), "main", "cycle"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored cancellation: %v", err)
	}
}

func TestHistoricalGrantCycleUsesFirstJournalAdmission(t *testing.T) {
	store, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	first := HistoricalGrantCycleRef{BranchID: "main", CycleID: "cycle", EventID: "first", EventHash: strings.Repeat("a", 64)}
	second := first
	second.EventID, second.EventHash = "second", strings.Repeat("b", 64)
	insertGrantEvent(t, store.DB(), 1, first, historicalGrantEventType)
	if err := PutHistoricalGrantCycleRef(t.Context(), store.DB(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `DELETE FROM historical_grant_cycles`); err != nil {
		t.Fatal(err)
	}
	insertGrantEvent(t, store.DB(), 2, second, historicalGrantEventType)
	if err := PutHistoricalGrantCycleRef(t.Context(), store.DB(), second); !errors.Is(err, ErrHistoricalGrantConflict) {
		t.Errorf("index loss allowed a later admission to replace the first: %v", err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `DELETE FROM historical_grant_cycles`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(t.Context(), `INSERT INTO historical_grant_cycles(branch_id,cycle_id,source_event_id,source_hash) VALUES(?,?,?,?)`,
		second.BranchID, second.CycleID, second.EventID, second.EventHash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := HistoricalGrantCycle(t.Context(), store.DB(), "main", "cycle"); !errors.Is(err, ErrHistoricalGrantSource) {
		t.Errorf("reader accepted a valid event hash bound to the wrong admission: %v", err)
	}
}

func TestHistoricalGrantCycleRollsBackWithSourceEvent(t *testing.T) {
	store, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ref := HistoricalGrantCycleRef{BranchID: "main", CycleID: "cycle", EventID: "grant", EventHash: strings.Repeat("a", 64)}
	injected := errors.New("injected post-index failure")
	err = store.WithTx(t.Context(), func(tx *sql.Tx) error {
		insertGrantEvent(t, tx, 1, ref, historicalGrantEventType)
		if err := PutHistoricalGrantCycleRef(t.Context(), tx, ref); err != nil {
			return err
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatal(err)
	}
	if _, found, err := HistoricalGrantCycle(t.Context(), store.DB(), "main", "cycle"); err != nil || found {
		t.Fatalf("rollback left a grant: found=%v err=%v", found, err)
	}
	var events int
	if err := store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM events`).Scan(&events); err != nil || events != 0 {
		t.Fatalf("rollback left source events: %d, %v", events, err)
	}
}

func TestHistoricalGrantReadOnlyCompatibilityDoesNotMigrate(t *testing.T) {
	for _, version := range []int{3, 4} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			workspace := t.TempDir()
			db, err := sql.Open("sqlite", Path(workspace))
			if err != nil {
				t.Fatal(err)
			}
			if err := sqlmigrate.Apply(t.Context(), db, "runtime store", migrations[:version], nil); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(Path(workspace))
			if err != nil {
				t.Fatal(err)
			}
			store, err := OpenReadOnly(t.Context(), workspace)
			if err != nil {
				t.Fatal(err)
			}
			if _, found, err := HistoricalGrantCycle(t.Context(), store.DB(), "main", "cycle"); err != nil || found {
				t.Fatalf("invented an old-format grant: found=%v err=%v", found, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(Path(workspace))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("read-only compatibility changed source: %v", err)
			}
			if names := dirNames(t, workspace); len(names) != 1 || names[0] != filepath.Base(Path(workspace)) {
				t.Fatalf("compatibility read left source files: %v", names)
			}
		})
	}
}

func TestHistoricalGrantMigrationRejectsOlderSchemaVerifier(t *testing.T) {
	store, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := sqlmigrate.Verify(t.Context(), store.DB(), "old runtime store", migrations[:4]); err == nil {
		t.Fatal("old schema verifier accepted the upgraded database")
	}
	if _, err := store.DB().ExecContext(t.Context(), `DROP TABLE historical_grant_cycles`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := HistoricalGrantCycle(t.Context(), store.DB(), "main", "cycle"); err == nil {
		t.Fatal("missing new table was treated as an old schema")
	}
}
