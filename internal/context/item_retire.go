package context

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

// maxRetireReasonRunes bounds the operator reason kept in metadata and events.
const maxRetireReasonRunes = 500

// RetireConfirmed withdraws current confirmed knowledge that an operator found
// wrong or stale and has no replacement for. It sets expires_at, the expiry
// every read path already honors, so the item leaves prompts, promotion, and
// consolidation sources at once, while the record and the reason stay for
// audit (DeleteExpired, which no runtime path calls, would remove it like any
// expired item). In the same transaction it demotes consolidations derived from the
// item, as supersession does. An item from a reserved workflow is refused:
// retiring a consolidated candidate here would leave its proposal approved.
func (r *SQLiteRepository) RetireConfirmed(ctx context.Context, ids []string, reason string) error {
	reason = strings.TrimSpace(utils.RedactSecrets(strings.TrimSpace(reason)))
	if runes := []rune(reason); len(runes) > maxRetireReasonRunes {
		reason = string(runes[:maxRetireReasonRunes])
	}
	if reason == "" {
		return fmt.Errorf("retire context items: a reason is required")
	}
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return nil
	}
	return r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		now := time.Now()
		for _, id := range unique {
			item, err := getItemQ(ctx, tx, id)
			if err != nil {
				return fmt.Errorf("load context item %q: %w", id, err)
			}
			if IsReservedSourceType(item.Source.Type) {
				return fmt.Errorf("%w: context item %q came from a consolidation proposal; supersede it with a confirmed revision instead", ErrReservedSourceType, id)
			}
			if item.Lifecycle != LifecycleConfirmed || item.SupersededBy != "" || (item.ExpiresAt != nil && !now.Before(*item.ExpiresAt)) {
				return fmt.Errorf("%w: context item %q is not current confirmed knowledge", ErrLifecycleTransition, id)
			}
			if _, err = tx.ExecContext(ctx, "UPDATE context_items SET expires_at=?,updated_at=?,metadata_json=json_set(metadata_json,'$.retired_reason',?,'$.retired_at',?) WHERE id=?", now.UnixMilli(), now.UnixMilli(), reason, now.UTC().Format(time.RFC3339), id); err != nil {
				return err
			}
			if err = insertEvent(ctx, tx, "retire", id, item.Scope, map[string]string{"reason": reason}); err != nil {
				return err
			}
			if err = demoteDerivedConsolidationsTx(ctx, tx, []string{id}, ReasonSourceExpired); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}
