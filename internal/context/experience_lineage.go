package context

import (
	"context"
	"fmt"
)

// PromotedSessionRecordSourceRef is the source ref of a persistent candidate
// that run-end extraction derived from one of the run's session records. Its
// context_item evidence names that record.
const PromotedSessionRecordSourceRef = "AutoExtractLTM"

// ExperienceLineage links a confirmed persistent record to the session record
// it was promoted from. Promotion writes a new record with its own ID, while
// outcome evidence is keyed by the ID a task saw, so without the link the
// evidence a session record earned stays behind and the persistent record
// starts from nothing.
type ExperienceLineage struct {
	SourceID string
	TargetID string
}

// ListExperienceLineage derives every promotion link from canonical rows: a
// confirmed persistent record extracted at run end, and each context_item it
// cites as evidence that exists in the same project. The links come from the
// records themselves, so a rebuild after an upgrade recovers links for
// records promoted before any were recorded. An empty projectID lists every
// project.
func (r *SQLiteRepository) ListExperienceLineage(ctx context.Context, projectID string) ([]ExperienceLineage, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT json_extract(j.value,'$.item_id') AS source_id, p.id
FROM context_items p, json_each(p.evidence_json) j
WHERE (?='' OR p.project_id=?)
  AND p.session_id IS NULL
  AND p.lifecycle='confirmed'
  AND json_extract(p.source_json,'$.type')='shared_memory_candidate'
  AND json_extract(p.source_json,'$.ref')=?
  AND json_extract(j.value,'$.type')='context_item'
  AND COALESCE(json_extract(j.value,'$.item_id'),'')<>''
  AND json_extract(j.value,'$.item_id')<>p.id
  AND EXISTS (SELECT 1 FROM context_items s WHERE s.id=json_extract(j.value,'$.item_id') AND s.project_id=p.project_id)
ORDER BY source_id, p.id`, projectID, projectID, PromotedSessionRecordSourceRef)
	if err != nil {
		return nil, fmt.Errorf("list experience lineage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ExperienceLineage
	for rows.Next() {
		var link ExperienceLineage
		if err := rows.Scan(&link.SourceID, &link.TargetID); err != nil {
			return nil, fmt.Errorf("scan experience lineage: %w", err)
		}
		out = append(out, link)
	}
	return out, rows.Err()
}
