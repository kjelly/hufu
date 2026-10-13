package context

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Inventory policies decide which canonical items a semantic generation
// covers. The policy is part of the generation identity.
const (
	// InventoryPolicyProjectAll covers every item of the project, including
	// rejected and superseded rows. Kept for existing static-table callers.
	InventoryPolicyProjectAll = "project-all-v0"
	// InventoryPolicyRetrievable covers the items retrieval can ever return:
	// candidate or confirmed, not superseded, in any scope of the project.
	// Lifecycle, expiry, activation and scope gates still run per request.
	InventoryPolicyRetrievable = "retrievable-v1"
)

// ValidInventoryPolicy reports whether policy is known.
func ValidInventoryPolicy(policy string) bool {
	return policy == InventoryPolicyProjectAll || policy == InventoryPolicyRetrievable
}

type EmbeddingInventoryItem struct {
	ItemID      string
	Content     string
	ContentHash string
}

// EmbeddingInventory is a consistent snapshot of the items one policy covers.
// Revision is the global canonical revision at snapshot time; it orders
// generations but is not a freshness test (freshness is per item hash).
type EmbeddingInventory struct {
	ProjectID string
	Policy    string
	Revision  int64
	Digest    string
	Items     []EmbeddingInventoryItem
}

type CanonicalEmbeddingInventory interface {
	LoadEmbeddingInventory(context.Context, string) (EmbeddingInventory, error)
	LoadEmbeddingInventoryForPolicy(context.Context, string, string) (EmbeddingInventory, error)
}

// LoadEmbeddingInventory loads the project-all-v0 inventory.
func (r *SQLiteRepository) LoadEmbeddingInventory(ctx context.Context, projectID string) (EmbeddingInventory, error) {
	return r.LoadEmbeddingInventoryForPolicy(ctx, projectID, InventoryPolicyProjectAll)
}

// LoadEmbeddingInventoryForPolicy reads the policy's items under one
// read-only snapshot, ordered bytewise by item ID.
func (r *SQLiteRepository) LoadEmbeddingInventoryForPolicy(ctx context.Context, projectID, policy string) (EmbeddingInventory, error) {
	if strings.TrimSpace(projectID) == "" {
		return EmbeddingInventory{}, errors.New("embedding inventory project ID is required")
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return EmbeddingInventory{}, fmt.Errorf("begin embedding inventory snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	inventory, err := loadEmbeddingInventory(ctx, tx, projectID, policy)
	if err != nil {
		return EmbeddingInventory{}, err
	}
	if err = tx.Commit(); err != nil {
		return EmbeddingInventory{}, fmt.Errorf("commit embedding inventory snapshot: %w", err)
	}
	return inventory, nil
}

func inventoryPredicate(policy string) (string, error) {
	switch policy {
	case InventoryPolicyProjectAll:
		return "", nil
	case InventoryPolicyRetrievable:
		return " AND lifecycle IN ('candidate','confirmed') AND superseded_by IS NULL", nil
	default:
		return "", fmt.Errorf("unknown embedding inventory policy %q", policy)
	}
}

func loadEmbeddingInventory(ctx context.Context, query sqlQueryer, projectID, policy string) (EmbeddingInventory, error) {
	predicate, err := inventoryPredicate(policy)
	if err != nil {
		return EmbeddingInventory{}, err
	}
	inventory := EmbeddingInventory{ProjectID: projectID, Policy: policy}
	if err := query.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence),0) FROM context_events").Scan(&inventory.Revision); err != nil {
		return EmbeddingInventory{}, fmt.Errorf("read canonical revision: %w", err)
	}
	rows, err := query.QueryContext(ctx, `SELECT id,content,LOWER(content_hash)
		FROM context_items WHERE project_id=?`+predicate+` ORDER BY id COLLATE BINARY`, projectID)
	if err != nil {
		return EmbeddingInventory{}, fmt.Errorf("read embedding inventory: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var item EmbeddingInventoryItem
		if err = rows.Scan(&item.ItemID, &item.Content, &item.ContentHash); err != nil {
			return EmbeddingInventory{}, err
		}
		if !validEmbeddingHash(item.ContentHash) {
			return EmbeddingInventory{}, fmt.Errorf("context item %s has invalid content hash", item.ItemID)
		}
		inventory.Items = append(inventory.Items, item)
	}
	if err = rows.Err(); err != nil {
		return EmbeddingInventory{}, err
	}
	inventory.Digest = embeddingInventoryDigest(inventory.Items)
	return inventory, nil
}

// embeddingInventoryDigest hashes (item ID, content hash) pairs in the given
// order; callers pass bytewise-sorted IDs.
func embeddingInventoryDigest(items []EmbeddingInventoryItem) string {
	hash := sha256.New()
	var encoded [binary.MaxVarintLen64]byte
	for _, item := range items {
		length := binary.PutUvarint(encoded[:], uint64(len(item.ItemID)))
		_, _ = hash.Write(encoded[:length])
		_, _ = hash.Write([]byte(item.ItemID))
		contentHash := strings.ToLower(item.ContentHash)
		length = binary.PutUvarint(encoded[:], uint64(len(contentHash)))
		_, _ = hash.Write(encoded[:length])
		_, _ = hash.Write([]byte(contentHash))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

var _ CanonicalEmbeddingInventory = (*SQLiteRepository)(nil)
