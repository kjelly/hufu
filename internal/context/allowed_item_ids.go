package context

import (
	"encoding/json/v2"
	"errors"
	"slices"
)

// ErrInvalidAllowedItemID rejects malformed lineage constraints without widening them.
var ErrInvalidAllowedItemID = errors.New("allowed item IDs must not contain an empty ID")

const contextItemPrimaryKeyIndex = "sqlite_autoindex_context_items_1"

// SQLite cannot estimate a bound JSON set's cardinality. Without this hint it
// can scan a whole project scope instead of looking up the explicitly allowed
// IDs. Unrestricted requests retain their existing planner choices.
func contextItemsTableForIDs(ids []string) string {
	if ids == nil {
		return "context_items"
	}
	return "context_items INDEXED BY " + contextItemPrimaryKeyIndex
}

func normalizeAllowedItemIDs(ids []string) []string {
	if ids == nil {
		return nil
	}
	normalized := slices.Clone(ids)
	slices.Sort(normalized)
	return slices.Compact(normalized)
}

func validateAllowedItemIDs(ids []string) error {
	if slices.Contains(ids, "") {
		return ErrInvalidAllowedItemID
	}
	return nil
}

// One parameter avoids SQLite's variable ceiling even for a large lineage.
// IN is a set membership predicate: duplicate IDs never duplicate result rows.
func appendAllowedItemIDPredicate(prefix string, where *[]string, args *[]any, allowed []string) error {
	if err := validateAllowedItemIDs(allowed); err != nil {
		return err
	}
	if allowed == nil {
		return nil
	}
	if len(allowed) == 0 {
		*where = append(*where, "1=0")
		return nil
	}
	encoded, err := json.Marshal(normalizeAllowedItemIDs(allowed))
	if err != nil {
		return err
	}
	*where = append(*where, prefix+"id IN (SELECT value FROM json_each(?))")
	*args = append(*args, string(encoded))
	return nil
}
