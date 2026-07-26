package context

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failThenSucceedRepo lets tests force Append to fail for specific item IDs
// on the first call and succeed afterwards, without needing a real SQLite
// outage.
type failThenSucceedRepo struct {
	*SQLiteRepository
	failOnce map[string]bool
	appended []ContextItem
}

func (f *failThenSucceedRepo) Append(ctx context.Context, items ...ContextItem) error {
	for _, it := range items {
		if f.failOnce[it.ID] {
			f.failOnce[it.ID] = false
			return errors.New("simulated transient store outage")
		}
	}
	f.appended = append(f.appended, items...)
	return f.SQLiteRepository.Append(ctx, items...)
}

func TestAppendPendingWriteThenRepairRecoversAndRedacts(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	fr := &failThenSucceedRepo{SQLiteRepository: repo, failOnce: map[string]bool{"pending-1": true}}

	item := ContextItem{ID: "pending-1", Kind: ContextProgress, Content: "token=eyJhbGciOiJIUzI1NiJ9 while writing stm", Scope: Scope{ProjectID: "p"}, Authority: AuthorityAgent, TrustLevel: TrustInternal}
	appendErr := fr.Append(context.Background(), item)
	if appendErr == nil {
		t.Fatal("expected simulated Append failure")
	}

	pendingPath := filepath.Join(dir, "context-pending.jsonl")
	if err := AppendPendingWrite(pendingPath, item, appendErr); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatalf("pending queue must not retain the secret in cleartext: %s", raw)
	}
	if !strings.Contains(string(raw), "simulated transient store outage") {
		t.Fatalf("pending record should retain the failure cause: %s", raw)
	}

	recovered, remaining, err := RepairPendingWrites(context.Background(), fr, pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 1 || remaining != 0 {
		t.Fatalf("recovered=%d remaining=%d, want 1/0", recovered, remaining)
	}
	got, err := repo.Get(context.Background(), "pending-1")
	if err != nil {
		t.Fatalf("repaired item should now be queryable: %v", err)
	}
	if strings.Contains(got.Content, "eyJhbGciOiJIUzI1NiJ9") {
		t.Fatalf("repaired item leaked secret: %q", got.Content)
	}

	if _, err := os.Stat(pendingPath); err != nil {
		t.Fatal(err)
	}
	remainingRaw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(remainingRaw)) != "" {
		t.Fatalf("pending file should be drained after a successful repair, got: %q", remainingRaw)
	}
}

func TestRepairPendingWritesLeavesStillFailingRecords(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	pendingPath := filepath.Join(dir, "context-pending.jsonl")
	item := ContextItem{ID: "still-broken", Kind: ContextProgress, Content: "irrelevant", Scope: Scope{ProjectID: "p"}}
	if err := AppendPendingWrite(pendingPath, item, errors.New("first failure")); err != nil {
		t.Fatal(err)
	}

	alwaysFail := &failThenSucceedRepo{SQLiteRepository: repo, failOnce: map[string]bool{}}
	// Force every attempt to fail regardless of ID by pre-marking failOnce
	// true right before each Append call the repair loop makes.
	wrapped := alwaysFailingRepairRepo{alwaysFail}
	recovered, remaining, err := RepairPendingWrites(context.Background(), wrapped, pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 0 || remaining != 1 {
		t.Fatalf("recovered=%d remaining=%d, want 0/1", recovered, remaining)
	}
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "still-broken") {
		t.Fatalf("still-failing record should remain in the pending file: %s", raw)
	}
	if !strings.Contains(string(raw), "repair still failing") {
		t.Fatalf("pending record cause should be updated with the latest failure: %s", raw)
	}
}

type alwaysFailingRepairRepo struct{ *failThenSucceedRepo }

func (a alwaysFailingRepairRepo) Append(context.Context, ...ContextItem) error {
	return errors.New("repair still failing")
}

func TestAppendPendingWriteRedactsSecretInFailureCause(t *testing.T) {
	dir := t.TempDir()
	pendingPath := filepath.Join(dir, "context-pending.jsonl")
	item := ContextItem{ID: "cause-secret", Kind: ContextProgress, Content: "harmless content", Scope: Scope{ProjectID: "p"}}

	// Reproduces the follow-up review's exact repro: a driver/repository
	// error can echo the rejected value back verbatim.
	if err := AppendPendingWrite(pendingPath, item, errors.New("retry failed: password=plaintext")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "plaintext") {
		t.Fatalf("failure cause leaked a secret into the pending queue: %s", raw)
	}
	if !strings.Contains(string(raw), "REDACTED") {
		t.Fatalf("expected the redacted cause to still note that something was redacted: %s", raw)
	}
}

func TestRepairPendingWritesRedactsSecretInUpdatedFailureCause(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()

	pendingPath := filepath.Join(dir, "context-pending.jsonl")
	item := ContextItem{ID: "still-broken-secret", Kind: ContextProgress, Content: "irrelevant", Scope: Scope{ProjectID: "p"}}
	if err := AppendPendingWrite(pendingPath, item, errors.New("first failure")); err != nil {
		t.Fatal(err)
	}

	secretFailingRepo := secretLeakingRepairRepo{&failThenSucceedRepo{SQLiteRepository: repo}}
	_, remaining, err := RepairPendingWrites(context.Background(), secretFailingRepo, pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Fatalf("remaining=%d, want 1", remaining)
	}
	raw, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-live-1234567890abcdef") {
		t.Fatalf("repair's updated failure cause leaked a secret into the pending queue: %s", raw)
	}
}

type secretLeakingRepairRepo struct{ *failThenSucceedRepo }

func (s secretLeakingRepairRepo) Append(context.Context, ...ContextItem) error {
	return errors.New(`repository rejected api_key = "sk-live-1234567890abcdef"`)
}

func TestRepairPendingWritesNoFileIsANoop(t *testing.T) {
	dir := t.TempDir()
	repo, err := OpenSQLite(filepath.Join(dir, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	recovered, remaining, err := RepairPendingWrites(context.Background(), repo, filepath.Join(dir, "missing.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if recovered != 0 || remaining != 0 {
		t.Fatalf("recovered=%d remaining=%d, want 0/0", recovered, remaining)
	}
}
