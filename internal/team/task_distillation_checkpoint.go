package team

import (
	"context"
	"errors"
	"strings"

	"github.com/kjelly/hufu/internal/taskdistill"
)

// ensureIDFloor reserves identities retired from the active task projection.
// It changes no task state and therefore does not publish a checkpoint.
func (tl *TodoList) ensureIDFloor(floor int) {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	tl.next = max(tl.next, floor)
}

func loadDistillationSessionTree(ctx context.Context, workspace string, sd *SessionData) (*SessionTree, error) {
	st, exists, err := loadStoredSessionTreeContext(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if !exists {
		st = NewSessionTree()
		// A legacy checkpoint may not have a saved tree. Its synthesized
		// identity must be stable across preview/application, never time.Now.
		st.Branches["main"].CreatedAt = sd.CreatedAt
	}
	if st.ActiveBranch == "" {
		st.ActiveBranch = "main"
	}
	if st.Branches[st.ActiveBranch] == nil {
		return nil, errors.New("active branch is missing; recover before distillation")
	}
	return st, nil
}

// A process can stop between an event-first task transition and its mutable
// checkpoint. Offline maintenance must not mistake the old checkpoint for an
// idle, completed task. Legacy tasks with no lifecycle events remain readable.
func validateDistillationCheckpoint(tasks []*TodoItem, lineage []RunEvent) error {
	seen := map[string]bool{}
	for _, event := range lineage {
		if event.TaskID != "" && strings.HasPrefix(event.Type, "task_") {
			seen[event.TaskID] = true
		}
	}
	canonical, err := ReplayTodoList(lineage)
	if err != nil {
		return err
	}
	byID := map[string]*TodoItem{}
	for _, item := range canonical {
		if item != nil {
			byID[item.ID] = item
		}
	}
	for _, item := range tasks {
		if item == nil || !seen[item.ID] {
			continue
		}
		current := byID[item.ID]
		if current == nil || current.Status != item.Status || current.Output != item.Output || current.OccurrenceRevision != item.OccurrenceRevision || taskdistill.Digest(current.TypedResult) != taskdistill.Digest(item.TypedResult) {
			return errors.New("task checkpoint disagrees with canonical events; recover the session before distillation")
		}
		delete(byID, item.ID)
	}
	if len(byID) > 0 {
		return errors.New("canonical tasks are missing from the checkpoint; recover the session before distillation")
	}
	return nil
}
