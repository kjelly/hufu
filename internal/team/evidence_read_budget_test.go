package team

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactStoreOpenHonorsReadBudget(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	put, err := store.Put(t.Context(), PutArtifactRequest{
		Kind: "task_transcript", Path: "transcript.txt", Content: []byte("1234"),
		RunID: "run", TaskID: "task", Attempt: 1, Agent: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, budget := WithArtifactReadBudget(t.Context(), 4)
	reader, err := store.Open(ctx, put.ArtifactRef.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(data) != "1234" {
		t.Fatalf("bounded open: %q %v", data, err)
	}
	if err := store.Verify(ctx, put.ArtifactRef); !errors.Is(err, ErrReadLimit) || !errors.Is(budget.Err(), ErrReadLimit) {
		t.Fatalf("open and verify did not share budget: %v", err)
	}
}

func TestArtifactReadBudgetAggregate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("1234"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, budget := WithArtifactReadBudget(t.Context(), 8)
	for range 2 {
		data, err := readArtifactFile(ctx, path)
		if err != nil || string(data) != "1234" {
			t.Fatalf("exact budget rejected: %q %v", data, err)
		}
	}
	if budget.Err() != nil {
		t.Fatal("exact budget was marked exhausted")
	}
	if _, err := readArtifactFile(ctx, path); !errors.Is(err, ErrReadLimit) {
		t.Fatalf("cumulative excess accepted: %v", err)
	}
	if !errors.Is(budget.Err(), ErrReadLimit) {
		t.Fatal("budget failure not retained for audit caller")
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readArtifactFile(ctx, path); !errors.Is(err, ErrReadLimit) {
		t.Fatal("failed request resumed reading")
	}
}

func TestArtifactReadBudgetPreflightAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(40 << 20); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, budget := WithArtifactReadBudget(t.Context(), 32<<20)
	if _, err := readArtifactFile(ctx, path); !errors.Is(err, ErrReadLimit) {
		t.Fatalf("oversize file accepted: %v", err)
	}
	if budget.remaining != 32<<20 {
		t.Fatal("oversize file consumed bytes before admission")
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readArtifactFile(cancelled, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read: %v", err)
	}
}
