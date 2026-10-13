package context

import (
	"context"
	"strings"
)

// ActivationsForItems loads the same projection as ActivationForItem in small
// bound batches. Callers still own item authorization and source-revision
// checks. This read does not hydrate content or mutate canonical records.
func (r *SQLiteRepository) ActivationsForItems(ctx context.Context, ids []string) (map[string]ActivationRecord, error) {
	ids = normalizeAllowedItemIDs(ids)
	result := make(map[string]ActivationRecord, len(ids))
	for len(ids) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count := min(128, len(ids))
		args := make([]any, count)
		marks := make([]string, count)
		for index, id := range ids[:count] {
			args[index], marks[index] = id, "?"
		}
		rows, err := r.db.QueryContext(ctx, `SELECT context_item_id,phases,triggers,roles,capabilities,tools,error_classes,environment FROM context_activation WHERE context_item_id IN (`+strings.Join(marks, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var record ActivationRecord
			if err := rows.Scan(&record.ContextItemID, &record.Phases, &record.Triggers, &record.Roles, &record.Capabilities, &record.Tools, &record.ErrorClasses, &record.Environment); err != nil {
				_ = rows.Close()
				return nil, err
			}
			result[record.ContextItemID] = record
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
		ids = ids[count:]
	}
	return result, nil
}
