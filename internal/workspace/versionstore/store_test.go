package versionstore

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), Options{StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestOpenCreatesOwnerOnlyLayout(t *testing.T) {
	store := openTestStore(t)
	for _, path := range []string{store.layout.root, store.layout.objectsDir(), store.layout.tmpDir()} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s mode = %o, want 700", path, perm)
		}
	}
	info, err := os.Stat(store.layout.dbPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("db mode = %o, want 600", perm)
	}
}

func TestOpenReopenAndReadOnly(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	if _, err := Open(ctx, Options{StateDir: stateDir, ReadOnly: true}); !errors.Is(err, ErrStoreNotFound) {
		t.Fatalf("read-only open of missing store err = %v, want ErrStoreNotFound", err)
	}
	if _, err := os.Stat(StorageRoot(stateDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only open created files: %v", err)
	}
	first, err := Open(ctx, Options{StateDir: stateDir})
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, Options{StateDir: stateDir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	_ = second.Close()
	readOnly, err := Open(ctx, Options{StateDir: stateDir, ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	defer func() { _ = readOnly.Close() }()
	if _, err = readOnly.db.ExecContext(ctx, "DELETE FROM snapshots"); err == nil {
		t.Fatal("read-only store accepted a write")
	}
}

func TestOpenRejectsUnsupportedPlatform(t *testing.T) {
	previous := goos
	goos = "windows"
	t.Cleanup(func() { goos = previous })
	if PlatformSupported() {
		t.Fatal("PlatformSupported() = true for windows")
	}
	if _, err := Open(context.Background(), Options{StateDir: t.TempDir()}); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Open err = %v, want ErrUnsupportedPlatform", err)
	}
}

func insertRawSnapshot(t *testing.T, store *Store, id, branch, state string) {
	t.Helper()
	_, err := store.db.Exec(`INSERT INTO snapshots(id, workspace_id, branch_id, root_tree_hash, manifest_digest,
file_count, logical_bytes, materializable, reason, state, created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, "ws_a", branch, strings.Repeat("a", 64), strings.Repeat("b", 64), 0, 0, 1, "baseline", state, time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("insert snapshot: %v", err)
	}
}

func TestSchemaTriggersEnforceInvariants(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		statement string
		wantErr   string
	}{
		{name: "identity is immutable", state: "published", statement: "UPDATE snapshots SET root_tree_hash='x' WHERE id='wsv_1'", wantErr: "identity is immutable"},
		{name: "pending to published allowed", state: "pending", statement: "UPDATE snapshots SET state='published' WHERE id='wsv_1'"},
		{name: "pending to orphaned allowed", state: "pending", statement: "UPDATE snapshots SET state='orphaned' WHERE id='wsv_1'"},
		{name: "published to pruned allowed", state: "published", statement: "UPDATE snapshots SET state='pruned' WHERE id='wsv_1'"},
		{name: "published back to pending rejected", state: "published", statement: "UPDATE snapshots SET state='pending' WHERE id='wsv_1'", wantErr: "invalid workspace snapshot state transition"},
		{name: "orphaned to published rejected", state: "orphaned", statement: "UPDATE snapshots SET state='published' WHERE id='wsv_1'", wantErr: "invalid workspace snapshot state transition"},
		{name: "head on pending rejected", state: "pending", statement: "INSERT INTO branch_heads VALUES('ws_a','main','wsv_1',1,0)", wantErr: "must reference a published snapshot"},
		{name: "head on other branch rejected", state: "published", statement: "INSERT INTO branch_heads VALUES('ws_a','exp','wsv_1',1,0)", wantErr: "must reference a published snapshot"},
		{name: "head on published allowed", state: "published", statement: "INSERT INTO branch_heads VALUES('ws_a','main','wsv_1',1,0)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := openTestStore(t)
			insertRawSnapshot(t, store, "wsv_1", "main", tt.state)
			_, err := store.db.Exec(tt.statement)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("statement failed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseModeAndBelow(t *testing.T) {
	tests := []struct {
		value   string
		want    Mode
		wantErr bool
	}{
		{value: "", want: ModeOff},
		{value: "off", want: ModeOff},
		{value: "observe", want: ModeObserve},
		{value: "required", want: ModeRequired},
		{value: "Required", wantErr: true},
		{value: "on", wantErr: true},
	}
	for _, tt := range tests {
		got, err := ParseMode(tt.value)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Fatalf("ParseMode(%q) = %q, %v; want %q, err=%v", tt.value, got, err, tt.want, tt.wantErr)
		}
	}
	if !ModeObserve.Below(ModeRequired) || ModeRequired.Below(ModeObserve) || ModeOff.Below(ModeOff) {
		t.Fatal("Mode.Below ordering is wrong")
	}
}

func TestSnapshotIDFormat(t *testing.T) {
	store := openTestStore(t)
	id, err := store.newSnapshotID()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidSnapshotID(id) {
		t.Fatalf("generated id %q is not valid", id)
	}
	for _, bad := range []SnapshotID{"", "wsv_", "wop_" + SnapshotID(strings.Repeat("a", 32)), "wsv_" + SnapshotID(strings.Repeat("A", 32))} {
		if ValidSnapshotID(bad) {
			t.Fatalf("ValidSnapshotID(%q) = true", bad)
		}
	}
}
