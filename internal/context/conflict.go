package context

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

// CurrentConflictJudgePolicyVersion identifies the judge prompt and decision
// rules. Bump it whenever either changes: judgments from older versions stop
// counting as open conflicts, while human dismissals carry over.
const CurrentConflictJudgePolicyVersion = "conflict-judge-v1"

// ErrConflictsUnavailable reports a read-only store that predates the pair
// judgment table (migration 11).
var ErrConflictsUnavailable = errors.New("memory conflicts are unavailable until the context store is upgraded")

const (
	pairRationaleMaxRunes = 512
	conflictLookupChunk   = 500
)

// PairVerdict is the judge's relation between two persistent memories.
type PairVerdict string

const (
	PairVerdictContradicts  PairVerdict = "contradicts"
	PairVerdictCompatible   PairVerdict = "compatible"
	PairVerdictDuplicate    PairVerdict = "duplicate"
	PairVerdictRefines      PairVerdict = "refines"
	PairVerdictUndetermined PairVerdict = "undetermined" // judge output was invalid
)

func validPairVerdict(v PairVerdict) bool {
	switch v {
	case PairVerdictContradicts, PairVerdictCompatible, PairVerdictDuplicate, PairVerdictRefines, PairVerdictUndetermined:
		return true
	default:
		return false
	}
}

// PairJudgmentStatus is stored review state. Only contradictions are open or
// dismissed; every other verdict is not_applicable.
type PairJudgmentStatus string

const (
	PairJudgmentOpen          PairJudgmentStatus = "open"
	PairJudgmentDismissed     PairJudgmentStatus = "dismissed"
	PairJudgmentNotApplicable PairJudgmentStatus = "not_applicable"
)

// ConflictState is derived at read time from the judgment and the current
// state of both memories; it is never stored.
type ConflictState string

const (
	ConflictStateOpen                ConflictState = "open"
	ConflictStateDismissed           ConflictState = "dismissed"
	ConflictStateResolvedBySupersede ConflictState = "resolved_by_supersede"
	ConflictStateInactive            ConflictState = "inactive"
)

// PairJudgment records a model judgment about two existing context items. It
// stores no knowledge of its own.
type PairJudgment struct {
	ID                 string
	ProjectID          string
	TeamID             string // "" when the items have no team
	AgentID            string // "" for shared items
	ItemAID            string // bytewise smaller ID
	ItemBID            string
	ItemAContentHash   string
	ItemBContentHash   string
	Verdict            PairVerdict
	Status             PairJudgmentStatus
	JudgePolicyVersion string
	JudgeModel         string
	Rationale          string
	DismissReason      string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ConflictView is a contradiction judgment with its derived state.
type ConflictView struct {
	PairJudgment
	State     ConflictState
	ItemAKind ContextKind // "" when the item no longer exists
	ItemBKind ContextKind
}

type ConflictQuery struct {
	ProjectID, TeamID string
	// IncludeInactive lists every contradiction judgment instead of only the
	// ones whose derived state is open.
	IncludeInactive bool
}

// ConflictLookup is the runtime surface: which of these items currently have
// an open conflict.
type ConflictLookup interface {
	OpenConflictsForItems(ctx context.Context, projectID, teamID string, itemIDs []string) (map[string][]string, error)
}

// NormalizePairJudgment orders the pair bytewise and computes the ID.
func NormalizePairJudgment(j *PairJudgment) {
	if j.ItemBID < j.ItemAID {
		j.ItemAID, j.ItemBID = j.ItemBID, j.ItemAID
		j.ItemAContentHash, j.ItemBContentHash = j.ItemBContentHash, j.ItemAContentHash
	}
	j.ID = PairJudgmentID(*j)
}

// PairJudgmentID is deterministic over scope, both items and their content
// hashes, and the judge policy version. j must already be normalized.
func PairJudgmentID(j PairJudgment) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{j.ProjectID, j.TeamID, j.AgentID, j.ItemAID, j.ItemAContentHash, j.ItemBID, j.ItemBContentHash, j.JudgePolicyVersion}, "\x00")))
	return "conflict-" + hex.EncodeToString(sum[:12])
}

type conflictQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const pairJudgmentColumns = "id,project_id,team_id,agent_id,item_a_id,item_b_id,item_a_content_hash,item_b_content_hash,verdict,status,judge_policy_version,judge_model,rationale,dismiss_reason,created_at,updated_at"

func scanPairJudgment(row interface{ Scan(...any) error }) (PairJudgment, error) {
	var j PairJudgment
	var created, updated int64
	err := row.Scan(&j.ID, &j.ProjectID, &j.TeamID, &j.AgentID, &j.ItemAID, &j.ItemBID, &j.ItemAContentHash, &j.ItemBContentHash, &j.Verdict, &j.Status, &j.JudgePolicyVersion, &j.JudgeModel, &j.Rationale, &j.DismissReason, &created, &updated)
	if err != nil {
		return j, err
	}
	j.CreatedAt = time.UnixMilli(created).UTC()
	j.UpdatedAt = time.UnixMilli(updated).UTC()
	return j, nil
}

func lookupPairJudgment(ctx context.Context, q conflictQuerier, id string) (PairJudgment, bool, error) {
	j, err := scanPairJudgment(q.QueryRowContext(ctx, "SELECT "+pairJudgmentColumns+" FROM context_pair_judgments WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return PairJudgment{}, false, nil
	}
	if err != nil {
		return PairJudgment{}, false, fmt.Errorf("load pair judgment %s: %w", id, err)
	}
	return j, true, nil
}

// LookupPairJudgment returns the row for id, if any.
func (r *SQLiteRepository) LookupPairJudgment(ctx context.Context, id string) (PairJudgment, bool, error) {
	if !r.schemaAtLeast(schemaVersionPairJudgments) {
		return PairJudgment{}, false, ErrConflictsUnavailable
	}
	return lookupPairJudgment(ctx, r.db, id)
}

func validatePairJudgment(j PairJudgment) error {
	if j.ProjectID == "" || j.ItemAID == "" || j.ItemBID == "" || j.ItemAContentHash == "" || j.ItemBContentHash == "" {
		return errors.New("pair judgment requires project, both item IDs, and both content hashes")
	}
	if j.ItemAID >= j.ItemBID {
		return errors.New("pair judgment items must be distinct and normalized")
	}
	if !validPairVerdict(j.Verdict) {
		return fmt.Errorf("invalid pair verdict %q", j.Verdict)
	}
	if j.JudgePolicyVersion == "" || j.JudgeModel == "" {
		return errors.New("pair judgment requires judge policy version and model")
	}
	if j.ID != PairJudgmentID(j) {
		return errors.New("pair judgment ID does not match its identity")
	}
	return nil
}

// sanitizePairRationale redacts the rationale and keeps at most
// pairRationaleMaxRunes runes; an overlong rationale is cut, never rejected.
func sanitizePairRationale(rationale string) string {
	runes := []rune(strings.TrimSpace(utils.RedactSecrets(rationale)))
	if len(runes) > pairRationaleMaxRunes {
		runes = runes[:pairRationaleMaxRunes]
	}
	return string(runes)
}

func conflictDetectedEvent(j PairJudgment, actor string) (PromotionOutboxEvent, error) {
	payload, err := json.Marshal(map[string]any{"schema_version": 1, "conflict_id": j.ID, "item_ids": []string{j.ItemAID, j.ItemBID}, "content_hashes": []string{j.ItemAContentHash, j.ItemBContentHash}, "judge_policy_version": j.JudgePolicyVersion, "actor": actor})
	if err != nil {
		return PromotionOutboxEvent{}, err
	}
	return PromotionOutboxEvent{IdempotencyKey: j.ID + ":detected", EventType: "memory_conflict_detected", Payload: payload}, nil
}

func conflictDismissedEvent(j PairJudgment, actor string) (PromotionOutboxEvent, error) {
	payload, err := json.Marshal(map[string]any{"schema_version": 1, "conflict_id": j.ID, "item_ids": []string{j.ItemAID, j.ItemBID}, "actor": actor})
	if err != nil {
		return PromotionOutboxEvent{}, err
	}
	return PromotionOutboxEvent{IdempotencyKey: j.ID + ":dismissed", EventType: "memory_conflict_dismissed", Payload: payload}, nil
}

// SavePairJudgment inserts j when its ID is new and reports whether it wrote.
//   - An existing row is returned unchanged (a dismissed conflict stays
//     dismissed), unless it is undetermined and replaceUndetermined is set, in
//     which case it is updated in place.
//   - A new contradiction inherits status dismissed and its reason when any
//     dismissed judgment exists for the same items and content hashes under
//     any judge version; no detected event is written then.
//   - Otherwise a new or replaced contradiction is open and writes a
//     memory_conflict_detected event in the same transaction.
//   - The rationale is redacted and truncated; its length never errors.
func (r *SQLiteRepository) SavePairJudgment(ctx context.Context, j PairJudgment, actor string, replaceUndetermined bool) (PairJudgment, bool, error) {
	if !r.schemaAtLeast(schemaVersionPairJudgments) {
		return PairJudgment{}, false, ErrConflictsUnavailable
	}
	if err := validatePairJudgment(j); err != nil {
		return PairJudgment{}, false, err
	}
	j.Rationale = sanitizePairRationale(j.Rationale)
	j.DismissReason = ""
	j.Status = PairJudgmentNotApplicable
	var stored PairJudgment
	var written bool
	err := r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		existing, found, err := lookupPairJudgment(ctx, tx, j.ID)
		if err != nil {
			return err
		}
		if found && (!replaceUndetermined || existing.Verdict != PairVerdictUndetermined) {
			stored, written = existing, false
			return nil
		}
		now := time.Now().UTC()
		j.UpdatedAt = now
		j.CreatedAt = now
		if found {
			j.CreatedAt = existing.CreatedAt
		}
		var event *PromotionOutboxEvent
		if j.Verdict == PairVerdictContradicts {
			j.Status = PairJudgmentOpen
			var reason string
			err = tx.QueryRowContext(ctx, `SELECT dismiss_reason FROM context_pair_judgments WHERE item_a_id=? AND item_b_id=? AND item_a_content_hash=? AND item_b_content_hash=? AND status='dismissed' ORDER BY created_at, id LIMIT 1`, j.ItemAID, j.ItemBID, j.ItemAContentHash, j.ItemBContentHash).Scan(&reason)
			switch {
			case err == nil:
				j.Status, j.DismissReason = PairJudgmentDismissed, reason
			case errors.Is(err, sql.ErrNoRows):
				detected, eventErr := conflictDetectedEvent(j, actor)
				if eventErr != nil {
					return eventErr
				}
				event = &detected
			default:
				return err
			}
		}
		if found {
			_, err = tx.ExecContext(ctx, `UPDATE context_pair_judgments SET verdict=?,status=?,judge_model=?,rationale=?,dismiss_reason=?,updated_at=? WHERE id=?`, j.Verdict, j.Status, j.JudgeModel, j.Rationale, j.DismissReason, now.UnixMilli(), j.ID)
		} else {
			_, err = tx.ExecContext(ctx, "INSERT INTO context_pair_judgments("+pairJudgmentColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", j.ID, j.ProjectID, j.TeamID, j.AgentID, j.ItemAID, j.ItemBID, j.ItemAContentHash, j.ItemBContentHash, j.Verdict, j.Status, j.JudgePolicyVersion, j.JudgeModel, j.Rationale, j.DismissReason, j.CreatedAt.UnixMilli(), now.UnixMilli())
		}
		if err != nil {
			return fmt.Errorf("save pair judgment %s: %w", j.ID, err)
		}
		if event != nil {
			if err = insertPromotionOutbox(ctx, tx, *event); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		stored, written = j, true
		return nil
	})
	return stored, written, err
}

// conflictItemState is the subset of a context item that derives a
// conflict's state.
type conflictItemState struct {
	Kind         ContextKind
	ContentHash  string
	Lifecycle    ContextLifecycle
	SupersededBy string
	ExpiresAt    sql.NullInt64
}

func loadConflictItemStates(ctx context.Context, q conflictQuerier, ids []string) (map[string]conflictItemState, error) {
	states := make(map[string]conflictItemState, len(ids))
	for start := 0; start < len(ids); start += conflictLookupChunk {
		chunk := ids[start:min(start+conflictLookupChunk, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := q.QueryContext(ctx, "SELECT id,kind,content_hash,lifecycle,COALESCE(superseded_by,''),expires_at FROM context_items WHERE id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+")", args...)
		if err != nil {
			return nil, fmt.Errorf("load conflict item states: %w", err)
		}
		for rows.Next() {
			var id string
			var state conflictItemState
			if err = rows.Scan(&id, &state.Kind, &state.ContentHash, &state.Lifecycle, &state.SupersededBy, &state.ExpiresAt); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan conflict item state: %w", err)
			}
			states[id] = state
		}
		if err = rows.Close(); err != nil {
			return nil, err
		}
	}
	return states, nil
}

// deriveConflictState applies the documented rules in order: dismissed, then
// an outdated judge version is inactive, then open when both items are still
// the same current knowledge, then resolved when either was superseded,
// otherwise inactive.
func deriveConflictState(j PairJudgment, states map[string]conflictItemState, now time.Time) ConflictState {
	if j.Status == PairJudgmentDismissed {
		return ConflictStateDismissed
	}
	if j.JudgePolicyVersion != CurrentConflictJudgePolicyVersion {
		return ConflictStateInactive
	}
	a, aOK := states[j.ItemAID]
	b, bOK := states[j.ItemBID]
	current := func(state conflictItemState, hash string) bool {
		return state.Lifecycle == LifecycleConfirmed && state.SupersededBy == "" && state.ContentHash == hash && (!state.ExpiresAt.Valid || state.ExpiresAt.Int64 > now.UnixMilli())
	}
	if aOK && bOK && current(a, j.ItemAContentHash) && current(b, j.ItemBContentHash) {
		return ConflictStateOpen
	}
	if (aOK && a.SupersededBy != "") || (bOK && b.SupersededBy != "") {
		return ConflictStateResolvedBySupersede
	}
	return ConflictStateInactive
}

func conflictViews(ctx context.Context, q conflictQuerier, judgments []PairJudgment) ([]ConflictView, error) {
	idSet := map[string]struct{}{}
	for _, j := range judgments {
		idSet[j.ItemAID] = struct{}{}
		idSet[j.ItemBID] = struct{}{}
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	states, err := loadConflictItemStates(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	views := make([]ConflictView, 0, len(judgments))
	for _, j := range judgments {
		views = append(views, ConflictView{PairJudgment: j, State: deriveConflictState(j, states, now), ItemAKind: states[j.ItemAID].Kind, ItemBKind: states[j.ItemBID].Kind})
	}
	return views, nil
}

func queryPairJudgments(ctx context.Context, q conflictQuerier, query string, args ...any) ([]PairJudgment, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query pair judgments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []PairJudgment
	for rows.Next() {
		j, scanErr := scanPairJudgment(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan pair judgment: %w", scanErr)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// ListConflicts returns contradiction judgments in the scope, ordered by ID.
func (r *SQLiteRepository) ListConflicts(ctx context.Context, q ConflictQuery) ([]ConflictView, error) {
	if !r.schemaAtLeast(schemaVersionPairJudgments) {
		return nil, ErrConflictsUnavailable
	}
	judgments, err := queryPairJudgments(ctx, r.db, "SELECT "+pairJudgmentColumns+" FROM context_pair_judgments WHERE project_id=? AND team_id=? AND verdict='contradicts' ORDER BY id", q.ProjectID, q.TeamID)
	if err != nil {
		return nil, err
	}
	views, err := conflictViews(ctx, r.db, judgments)
	if err != nil {
		return nil, err
	}
	if q.IncludeInactive {
		return views, nil
	}
	open := views[:0]
	for _, view := range views {
		if view.State == ConflictStateOpen {
			open = append(open, view)
		}
	}
	return open, nil
}

func getConflict(ctx context.Context, q conflictQuerier, id, projectID, teamID string) (ConflictView, error) {
	j, found, err := lookupPairJudgment(ctx, q, id)
	if err != nil {
		return ConflictView{}, err
	}
	if !found || j.ProjectID != projectID || j.TeamID != teamID || j.Verdict != PairVerdictContradicts {
		return ConflictView{}, fmt.Errorf("memory conflict %q not found in project %q team %q", id, projectID, teamID)
	}
	views, err := conflictViews(ctx, q, []PairJudgment{j})
	if err != nil {
		return ConflictView{}, err
	}
	return views[0], nil
}

// GetConflict returns one contradiction judgment in the scope.
func (r *SQLiteRepository) GetConflict(ctx context.Context, id, projectID, teamID string) (ConflictView, error) {
	if !r.schemaAtLeast(schemaVersionPairJudgments) {
		return ConflictView{}, ErrConflictsUnavailable
	}
	return getConflict(ctx, r.db, id, projectID, teamID)
}

// DismissConflict marks an open conflict as not a real contradiction. An
// already dismissed conflict is returned unchanged with dismissed=false; any
// other state is an error. The reason is required and must be secret-free.
func (r *SQLiteRepository) DismissConflict(ctx context.Context, id, projectID, teamID, reason, actor string) (ConflictView, bool, error) {
	if !r.schemaAtLeast(schemaVersionPairJudgments) {
		return ConflictView{}, false, ErrConflictsUnavailable
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ConflictView{}, false, errors.New("dismiss reason is required")
	}
	if utils.RedactSecrets(reason) != reason {
		return ConflictView{}, false, errors.New("dismiss reason contains secret-like material")
	}
	var view ConflictView
	var dismissed bool
	err := r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		current, err := getConflict(ctx, tx, id, projectID, teamID)
		if err != nil {
			return err
		}
		if current.State == ConflictStateDismissed {
			view, dismissed = current, false
			return nil
		}
		if current.State != ConflictStateOpen {
			return fmt.Errorf("memory conflict %s is %s; only open conflicts can be dismissed", id, current.State)
		}
		now := time.Now().UTC()
		if _, err = tx.ExecContext(ctx, `UPDATE context_pair_judgments SET status='dismissed',dismiss_reason=?,updated_at=? WHERE id=?`, reason, now.UnixMilli(), id); err != nil {
			return fmt.Errorf("dismiss memory conflict %s: %w", id, err)
		}
		event, err := conflictDismissedEvent(current.PairJudgment, actor)
		if err != nil {
			return err
		}
		if err = insertPromotionOutbox(ctx, tx, event); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		current.Status, current.DismissReason, current.UpdatedAt, current.State = PairJudgmentDismissed, reason, now, ConflictStateDismissed
		view, dismissed = current, true
		return nil
	})
	return view, dismissed, err
}

// OpenConflictsForItems maps each item ID to the sorted IDs of its conflicts
// whose derived state is open. A read-only store older than migration 11 has
// no conflicts.
func (r *SQLiteRepository) OpenConflictsForItems(ctx context.Context, projectID, teamID string, itemIDs []string) (map[string][]string, error) {
	result := map[string][]string{}
	if !r.schemaAtLeast(schemaVersionPairJudgments) || len(itemIDs) == 0 {
		return result, nil
	}
	requested := make(map[string]struct{}, len(itemIDs))
	for _, id := range itemIDs {
		requested[id] = struct{}{}
	}
	ids := make([]string, 0, len(requested))
	for id := range requested {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	seen := map[string]struct{}{}
	var judgments []PairJudgment
	for start := 0; start < len(ids); start += conflictLookupChunk {
		chunk := ids[start:min(start+conflictLookupChunk, len(ids))]
		marks := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := []any{projectID, teamID, CurrentConflictJudgePolicyVersion}
		for range 2 {
			for _, id := range chunk {
				args = append(args, id)
			}
		}
		found, err := queryPairJudgments(ctx, r.db, "SELECT "+pairJudgmentColumns+" FROM context_pair_judgments WHERE project_id=? AND team_id=? AND verdict='contradicts' AND status='open' AND judge_policy_version=? AND (item_a_id IN ("+marks+") OR item_b_id IN ("+marks+"))", args...)
		if err != nil {
			return nil, err
		}
		for _, j := range found {
			if _, dup := seen[j.ID]; !dup {
				seen[j.ID] = struct{}{}
				judgments = append(judgments, j)
			}
		}
	}
	views, err := conflictViews(ctx, r.db, judgments)
	if err != nil {
		return nil, err
	}
	for _, view := range views {
		if view.State != ConflictStateOpen {
			continue
		}
		for _, id := range []string{view.ItemAID, view.ItemBID} {
			if _, ok := requested[id]; ok {
				result[id] = append(result[id], view.ID)
			}
		}
	}
	for id := range result {
		sort.Strings(result[id])
	}
	return result, nil
}
