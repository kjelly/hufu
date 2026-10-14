package main

import (
	"context"
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
