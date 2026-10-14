// Package actionstate is the read-only runtime state Hufu lends to trusted Go
// actions. Those actions run in Hufu's embedded interpreter with the standard
// library only; the interpreter also exposes this package, so an action reads
// state exactly as the runtime stores it without opening the database itself.
package actionstate

import (
	"context"
	"fmt"
	"os"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// SessionCheckpoint returns the session checkpoint of the control workspace
// at root, read without creating or changing any file. A workspace without a
// checkpoint returns an error wrapping os.ErrNotExist.
func SessionCheckpoint(root string) ([]byte, error) {
	store, err := runtimestore.OpenReadOnly(context.Background(), root)
	if err != nil {
		return nil, fmt.Errorf("read session checkpoint: %w", err)
	}
	defer func() { _ = store.Close() }()
	body, exists, err := store.GetDocument(context.Background(), runtimestore.DocumentSession)
	if err != nil {
		return nil, fmt.Errorf("read session checkpoint: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("read session checkpoint: %w", os.ErrNotExist)
	}
	return body, nil
}
