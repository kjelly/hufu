package context

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBatchActivationMatchesSingleProjectionAcrossBoundaries(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	var ids []string
	for index := range 260 {
		id := fmt.Sprintf("item-%03d", index)
		ids = append(ids, id)
		item := ContextItem{ID: id, Content: id, Scope: Scope{ProjectID: "project"}, Kind: ContextPattern, Lifecycle: LifecycleConfirmed, Metadata: map[string]string{"activation.phases": "execute", "activation.roles": "worker", "activation.tools": "view"}}
		if err := repo.Append(t.Context(), item); err != nil {
			t.Fatal(err)
		}
	}
	batch, err := repo.ActivationsForItems(t.Context(), append(ids, ids[0], "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != len(ids) {
		t.Fatal("batch lost or fabricated activation rows")
	}
	for _, id := range ids {
		single, err := repo.ActivationForItem(t.Context(), id)
		if err != nil || !reflect.DeepEqual(single, batch[id]) {
			t.Fatalf("projection mismatch for %s: %v", id, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := repo.ActivationsForItems(ctx, ids); !errors.Is(err, context.Canceled) {
		t.Fatal("batch ignored outer cancellation")
	}
}
