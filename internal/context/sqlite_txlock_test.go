package context

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestSQLiteTransactionLockModes pins how a writable handle begins
// transactions. While another handle holds the write lock, a writable
// transaction must wait at BEGIN and then read the committed state, and a
// read-only transaction must read its snapshot without waiting.
func TestSQLiteTransactionLockModes(t *testing.T) {
	cases := []struct {
		name      string
		readOnly  bool
		wantWait  bool
		wantCount int
	}{
		{name: "writable waits for the other writer", wantWait: true, wantCount: 1},
		{name: "read-only reads its snapshot", readOnly: true, wantCount: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "context.sqlite")
			writer, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = writer.Close() }()
			other, err := OpenSQLite(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = other.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := writer.db.ExecContext(ctx, "CREATE TABLE txlock_probe(id TEXT PRIMARY KEY)"); err != nil {
				t.Fatal(err)
			}
			held, err := writer.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = held.Rollback() }()
			if _, err := held.ExecContext(ctx, "INSERT INTO txlock_probe(id) VALUES('writer')"); err != nil {
				t.Fatal(err)
			}

			type probe struct {
				count int
				err   error
			}
			done := make(chan probe, 1)
			go func() {
				tx, err := other.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: tc.readOnly})
				if err != nil {
					done <- probe{err: err}
					return
				}
				defer func() { _ = tx.Rollback() }()
				var count int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM txlock_probe").Scan(&count); err != nil {
					done <- probe{err: err}
					return
				}
				if !tc.readOnly {
					if _, err := tx.ExecContext(ctx, "INSERT INTO txlock_probe(id) VALUES('other')"); err != nil {
						done <- probe{err: err}
						return
					}
				}
				done <- probe{count: count, err: tx.Commit()}
			}()

			var got probe
			finished := false
			select {
			case got = <-done:
				finished = true
			case <-time.After(time.Second):
			}
			if finished == tc.wantWait {
				t.Fatalf("finished while the other handle held the write lock = %v (count %d, err %v), want %v", finished, got.count, got.err, !tc.wantWait)
			}
			if err := held.Commit(); err != nil {
				t.Fatal(err)
			}
			if !finished {
				got = <-done
			}
			if got.err != nil || got.count != tc.wantCount {
				t.Fatalf("got count %d, err %v; want count %d", got.count, got.err, tc.wantCount)
			}
		})
	}
}
