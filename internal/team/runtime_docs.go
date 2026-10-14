package team

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// runtimeDocs reads and writes a workspace's runtime documents: directly in
// runtime.sqlite (workspaceDocs), or staged in a runtimeBatch that commits
// them in one transaction.
type runtimeDocs interface {
	getDocument(name string) ([]byte, bool, error)
	putDocument(name string, body []byte) error
	deleteDocument(name string) error
}

// workspaceDocs reaches workspace's runtime.sqlite on every call. Reads never
// create the database; writes create it.
type workspaceDocs string

func (w workspaceDocs) getDocument(name string) ([]byte, bool, error) {
	if strings.TrimSpace(string(w)) == "" {
		return nil, false, nil
	}
	store, err := runtimestore.OpenRead(context.Background(), string(w))
	if errors.Is(err, runtimestore.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read runtime document %q: %w", name, err)
	}
	defer func() { _ = store.Close() }()
	return store.GetDocument(context.Background(), name)
}

func (w workspaceDocs) putDocument(name string, body []byte) error {
	return withRuntimeStore(string(w), func(store *runtimestore.Store) error {
		return store.PutDocument(context.Background(), name, body)
	})
}

func (w workspaceDocs) deleteDocument(name string) error {
	if !runtimestore.Exists(string(w)) {
		return nil
	}
	return withRuntimeStore(string(w), func(store *runtimestore.Store) error {
		return store.DeleteDocument(context.Background(), name)
	})
}

// withRuntimeStore runs fn on workspace's writable runtime store.
func withRuntimeStore(workspace string, fn func(*runtimestore.Store) error) error {
	if strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("runtime store: empty workspace")
	}
	store, err := runtimestore.Open(context.Background(), workspace)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	return fn(store)
}

// runtimeBatch stages document writes for one transaction. Reads see staged
// writes first, so a multi-step operation observes its own changes before
// anything is committed. Nothing is written until commit, and nothing holds a
// database connection while the operation computes.
type runtimeBatch struct {
	workspace string
	staged    map[string][]byte // nil body: deleted
	order     []string
}

func newRuntimeBatch(workspace string) *runtimeBatch {
	return &runtimeBatch{workspace: workspace, staged: make(map[string][]byte)}
}

func (b *runtimeBatch) getDocument(name string) ([]byte, bool, error) {
	if body, ok := b.staged[name]; ok {
		return body, body != nil, nil
	}
	return workspaceDocs(b.workspace).getDocument(name)
}

func (b *runtimeBatch) putDocument(name string, body []byte) error {
	if body == nil {
		body = []byte{}
	}
	b.stage(name, body)
	return nil
}

func (b *runtimeBatch) deleteDocument(name string) error {
	b.stage(name, nil)
	return nil
}

func (b *runtimeBatch) stage(name string, body []byte) {
	if _, ok := b.staged[name]; !ok {
		b.order = append(b.order, name)
	}
	b.staged[name] = body
}

// commit writes every staged document and, when tree is non-nil, the session
// tree in one transaction. A failure leaves runtime.sqlite unchanged.
func (b *runtimeBatch) commit(ctx context.Context, tree *SessionTree) error {
	var written map[string]string
	err := withRuntimeStore(b.workspace, func(store *runtimestore.Store) error {
		return store.WithTx(ctx, func(tx *sql.Tx) error {
			for _, name := range b.order {
				body := b.staged[name]
				var err error
				if body == nil {
					err = runtimestore.DeleteDocument(ctx, tx, name)
				} else {
					err = runtimestore.PutDocument(ctx, tx, name, body)
				}
				if err != nil {
					return err
				}
			}
			if err := runtimeBatchCommitStage(); err != nil {
				return err
			}
			if tree == nil {
				return nil
			}
			var err error
			written, err = saveSessionTreeTx(ctx, tx, tree)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("commit runtime state: %w", err)
	}
	if tree != nil {
		tree.markStored(written)
	}
	return nil
}

// runtimeBatchCommitStage lets tests fail a commit between its document and
// session tree writes.
var runtimeBatchCommitStage = func() error { return nil }
