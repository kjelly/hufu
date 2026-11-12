package team

import (
	"context"
	"database/sql"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/utils"
)

// TaskDistillationSnapshot binds cleanup to the complete task/branch state,
// including dependencies outside the operator's explicit selection.
type TaskDistillationSnapshot struct {
	BranchID    string               `json:"branch_id"`
	HeadID      string               `json:"head_id"`
	Fingerprint string               `json:"fingerprint"`
	Sources     []taskdistill.Source `json:"sources"`
}

func SnapshotTasksForDistillation(ctx context.Context, workspace string, ids []string) (TaskDistillationSnapshot, error) {
	sd, exists, err := LoadSessionReadOnly(workspace)
	if err != nil {
		return TaskDistillationSnapshot{}, err
	}
	if !exists {
		return TaskDistillationSnapshot{}, errors.New("task distillation requires an existing session")
	}
	st, err := loadDistillationSessionTree(ctx, workspace, sd)
	if err != nil {
		return TaskDistillationSnapshot{}, err
	}
	// A saved TaskPlan can lag a fork's immutable lineage. Include replayed
	// tasks when protecting results used by another branch.
	otherBranchTasks := map[string][]*TodoItem{}
	var observedHead string
	{
		es, err := OpenEventStoreReadOnly(workspace)
		if err != nil {
			return TaskDistillationSnapshot{}, err
		}
		events, readErr := es.ReadEvents()
		closeErr := es.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return TaskDistillationSnapshot{}, err
		}
		if len(events) > 0 {
			observedHead = events[len(events)-1].ID
		}
		lineage, err := ProjectValidatedEventsForBranch(events, st, st.ActiveBranch)
		if err != nil {
			return TaskDistillationSnapshot{}, err
		}
		if err = validateDistillationCheckpoint(sd.Tasks, lineage); err != nil {
			return TaskDistillationSnapshot{}, err
		}
		otherBranchTasks[st.ActiveBranch] = ReduceToTodoList(lineage)
		for branchID := range st.Branches {
			if branchID == st.ActiveBranch {
				continue
			}
			lineage, err := ProjectValidatedEventsForBranch(events, st, branchID)
			if err != nil {
				return TaskDistillationSnapshot{}, err
			}
			otherBranchTasks[branchID] = ReduceToTodoList(lineage)
		}
	}
	out := TaskDistillationSnapshot{BranchID: st.ActiveBranch, HeadID: observedHead, Fingerprint: taskdistill.Digest(struct {
		Session *SessionData
		Tree    *SessionTree
	}{sd, st})}
	wanted := map[string]bool{}
	for _, id := range ids {
		if id == "" || wanted[id] {
			return out, errors.New("task IDs must be explicit, unique and nonempty")
		}
		wanted[id] = true
	}
	for _, item := range sd.Tasks {
		if item == nil || !wanted[item.ID] {
			continue
		}
		raw, err := taskdistill.MarshalSnapshot(item)
		if err != nil {
			return out, err
		}
		protected := distillationTaskProtected(sd, st, item, otherBranchTasks)
		out.Sources = append(out.Sources, taskdistill.Source{ID: item.ID, Content: utils.RedactSecrets(string(raw)), Protected: protected})
		delete(wanted, item.ID)
	}
	if len(wanted) != 0 || len(out.Sources) == 0 {
		return out, errors.New("selected task does not exist in the active session")
	}
	slices.SortFunc(out.Sources, func(a, b taskdistill.Source) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func referencesDistillationTask(item *TodoItem, id string) bool {
	// Inspect structural identities, never task descriptions or result prose.
	raw, _ := taskdistill.MarshalSnapshot(item)
	var value any
	_ = json.Unmarshal(raw, &value)
	var walk func(any, bool) bool
	walk = func(value any, identity bool) bool {
		switch v := value.(type) {
		case string:
			return identity && v == id
		case []any:
			for _, child := range v {
				if walk(child, identity) {
					return true
				}
			}
		case map[string]any:
			for key, child := range v {
				lower := strings.ToLower(key)
				isID := lower == "id" || strings.HasSuffix(lower, "_id") || strings.HasSuffix(lower, "_ids") || slices.Contains([]string{"depends_on", "order_after", "evidence_from", "dependson", "orderafter", "evidencefrom", "parentid", "onfailure", "plantaskid"}, lower)
				if walk(child, isID) {
					return true
				}
			}
		}
		return false
	}
	return walk(value, false)
}

// RemoveDistilledTasks removes only active task/cache projections. Immutable
// events, typed results, receipts, artifacts and historical branches remain
// available for audit/time travel. The caller owns the managed workspace lock
// and has durably saved the reviewed knowledge before entering this boundary.
func RemoveDistilledTasks(ctx context.Context, workspace, planID string, snapshot TaskDistillationSnapshot, draft taskdistill.Draft) error {
	if err := taskdistill.Validate(draft, snapshot.Sources); err != nil {
		return err
	}
	ids := []string{}
	for _, decision := range draft.Decisions {
		if decision.Action != "retain" {
			ids = append(ids, decision.SourceID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	store, err := runtimestore.OpenRead(ctx, workspace)
	if err != nil {
		return err
	}
	appliedSnapshot, applied, err := store.GetDocument(ctx, "task_distillation_applied:"+planID)
	_ = store.Close()
	if err != nil {
		return err
	}
	if applied {
		if string(appliedSnapshot) != taskdistill.Digest(snapshot) {
			return errors.New("task distillation application receipt mismatch")
		}
		return nil
	}
	current, err := SnapshotTasksForDistillation(ctx, workspace, sourceTaskIDs(snapshot.Sources))
	if err != nil {
		return err
	}
	if current.Fingerprint != snapshot.Fingerprint || current.BranchID != snapshot.BranchID || taskdistill.Digest(current.Sources) != taskdistill.Digest(snapshot.Sources) {
		return errors.New("task distillation source changed; create a new preview")
	}
	sd, _, err := LoadSessionReadOnly(workspace)
	if err != nil {
		return err
	}
	st, err := loadDistillationSessionTree(ctx, workspace, sd)
	if err != nil {
		return err
	}
	es, err := OpenEventStore(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = es.Close() }()
	events, err := es.ReadEvents()
	if err != nil {
		return err
	}
	head := current.HeadID
	var removals []RunEvent
	floor := max(sd.WorkflowTaskIDFloor, workflowTaskIDHighWater(events))
	for _, item := range sd.Tasks {
		if item != nil {
			if number, err := strconv.Atoi(item.ID); err == nil {
				floor = max(floor, number)
			}
		}
	}
	for _, id := range ids {
		payload, _ := json.Marshal(map[string]any{"id": id, "status": string(TaskDone), "distillation_plan": planID, "task_id_floor": floor})
		removals = append(removals, RunEvent{Type: string(EventTaskRemoved), BranchID: snapshot.BranchID, TaskID: id, Actor: "maintenance", IdempotencyKey: "distill:" + planID + ":" + id, Payload: payload})
	}
	selected := map[string]*TodoItem{}
	originalSessionHash := taskdistill.Digest(sd)
	sd.WorkflowTaskIDFloor = floor
	for _, item := range sd.Tasks {
		if item != nil && slices.Contains(ids, item.ID) {
			selected[item.ID] = item
		}
	}
	sd.Tasks = slices.DeleteFunc(sd.Tasks, func(item *TodoItem) bool { return item != nil && selected[item.ID] != nil })
	if branch := st.Branches[st.ActiveBranch]; branch != nil {
		branch.State.TaskPlan = sd.Tasks
	}
	staged := newRuntimeBatch(workspace)
	if err := saveSessionTo(staged, sd); err != nil {
		return err
	}
	body := staged.staged[runtimestore.DocumentSession]
	_, err = es.appendEventsWithProjection(ctx, removals, &head, func(ctx context.Context, tx *sql.Tx) error {
		// A session-only writer is fenced too, even if it appended no event.
		stored, ok, err := runtimestore.GetDocument(ctx, tx, runtimestore.DocumentSession)
		if err != nil {
			return fmt.Errorf("read task distillation checkpoint: %w", err)
		}
		if !ok {
			return errors.New("task distillation checkpoint disappeared")
		}
		var live SessionData
		if err = taskdistill.UnmarshalSnapshot(stored, &live); err != nil {
			return err
		}
		if taskdistill.Digest(&live) != originalSessionHash {
			return errors.New("task checkpoint changed")
		}
		if err = runtimestore.PutDocument(ctx, tx, runtimestore.DocumentSession, body); err != nil {
			return err
		}
		if _, err = saveSessionTreeTx(ctx, tx, st); err != nil {
			return err
		}
		for _, item := range selected {
			// Cache records are optional reuse, not evidence. Result/diagnostic
			// records and their canonical receipt bytes are never rewritten.
			if _, err = tx.ExecContext(ctx, `DELETE FROM task_journal WHERE json_extract(record,'$.op') IN ('put','del') AND lower(json_extract(record,'$.agent'))=lower(?) AND json_extract(record,'$.desc')=?`, item.Agent, item.Desc); err != nil {
				return err
			}
		}
		return runtimestore.PutDocument(ctx, tx, "task_distillation_applied:"+planID, []byte(taskdistill.Digest(snapshot)))
	})
	return err
}

func sourceTaskIDs(sources []taskdistill.Source) []string {
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.ID)
	}
	return ids
}

func distillationTaskProtected(sd *SessionData, st *SessionTree, item *TodoItem, otherBranchTasks map[string][]*TodoItem) bool {
	protected := item.Status != TaskDone || sd.RecoveryRequired || sd.PendingTerminalCommit != nil || sd.PendingWrapUp != nil
	// Workflow occurrences participate in acceptance, repair and frozen
	// worksets. Keep them until the complete workspace is retired.
	protected = protected || sd.WorkflowState != "" || len(sd.CriterionResults) > 0 || distillationTaskHasBoundContract(item)
	for _, other := range append(slices.Clone(sd.Tasks), otherBranchTasks[st.ActiveBranch]...) {
		if other != nil && ((other.ID != item.ID && referencesDistillationTask(other, item.ID)) || distillationTaskHasBoundContract(other)) {
			protected = true
		}
	}
	for branchID, branch := range st.Branches {
		if branchID == st.ActiveBranch || branch == nil {
			continue
		}
		for _, other := range append(slices.Clone(branch.State.TaskPlan), otherBranchTasks[branchID]...) {
			if other != nil && (other.ID == item.ID || referencesDistillationTask(other, item.ID)) {
				protected = true
			}
		}
	}
	return protected
}

func distillationTaskHasBoundContract(item *TodoItem) bool {
	return item.Action != nil || item.ResultContract != nil || item.WorksetBinding != nil || item.ContractID != "" || item.PlanID != ""
}
