package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// putRuntimeDocument stores body verbatim as workspace's runtime document
// name, the way a fixture seeds state a run would have written, including
// deliberately malformed or legacy-shaped JSON that the typed savers would
// normalize.
func putRuntimeDocument(t *testing.T, workspace, name string, body []byte) {
	t.Helper()
	ctx := context.Background()
	store, err := runtimestore.Open(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	err = store.PutDocument(ctx, name, body)
	if closeErr := store.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

// writeTaskJournal stores each JSONL line of journal as one task journal
// record, verbatim.
func writeTaskJournal(t *testing.T, workspace, journal string) {
	t.Helper()
	store, err := runtimestore.Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	for _, line := range strings.Split(strings.TrimSpace(journal), "\n") {
		if _, err := store.DB().Exec(`INSERT INTO task_journal(recorded_at, record) VALUES('', ?)`, []byte(line)); err != nil {
			t.Fatal(err)
		}
	}
}
