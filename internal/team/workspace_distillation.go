package team

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/utils"
)

type RetiredDistillationSnapshot struct {
	Fingerprint string `json:"fingerprint"`
	Content     string `json:"content"`
	Protected   bool   `json:"protected"`
}

// SnapshotRetiredWorkspace captures canonical task results and existing memory.
// Debug logs, arbitrary project files and model call transcripts are not
// knowledge inputs. No source is silently truncated to fit the model budget.
// The caller must own the workspace lifecycle lock.
func SnapshotRetiredWorkspace(ctx context.Context, workspace string, scope contextstore.Scope) (RetiredDistillationSnapshot, error) {
	out := RetiredDistillationSnapshot{}
	store, err := runtimestore.OpenRead(ctx, workspace)
	if err != nil {
		return out, err
	}
	defer func() { _ = store.Close() }()
	eventGroups, eventDigest, err := readRetiredDistillationEvents(ctx, store.DB())
	if err != nil {
		return out, err
	}
	var taskGroups [][]*TodoItem
	for _, branch := range slices.Sorted(maps.Keys(eventGroups)) {
		taskGroups = append(taskGroups, eventGroups[branch])
	}
	sd, _, err := LoadSessionReadOnly(workspace)
	if err != nil {
		return out, err
	}
	var protectedGroups [][]*TodoItem
	if sd == nil {
		protectedGroups = append(protectedGroups, taskGroups...)
	}
	if sd != nil {
		protectedGroups = append(protectedGroups, sd.Tasks)
		taskGroups = append(taskGroups, sd.Tasks)
		out.Protected = sd.RecoveryRequired || sd.PendingTerminalCommit != nil || sd.PendingWrapUp != nil
		if sd.RunResult != nil && sd.RunResult.Outcome != RunOutcomeCompleted {
			out.Protected = true
		}
	}
	st, exists, err := LoadSavedSessionTree(workspace)
	if err != nil {
		return out, err
	}
	if exists {
		for _, branch := range st.ListBranches() {
			// A whole retired workspace includes every historical branch. Their
			// fork checkpoints are evidence, not live task dependencies.
			taskGroups = append(taskGroups, branch.State.TaskPlan)
		}
	}
	out.Protected = out.Protected || retiredCurrentTasksProtected(protectedGroups, st, exists, eventGroups)
	type taskEvidence struct {
		ID, Agent, Description, Status, Output string
		Goal, Constraints, Detail, VerifyMode  string
		Verification                           *VerificationResult
		Result                                 *TaskResult
	}
	var tasks []taskEvidence
	seen := map[string]bool{}
	for _, group := range taskGroups {
		for _, item := range group {
			if item == nil {
				continue
			}
			evidence := taskEvidence{ID: item.ID, Agent: item.Agent, Description: item.Desc, Status: string(item.Status), Output: item.Output, Goal: item.Goal, Constraints: item.Constraints, Detail: item.Detail, VerifyMode: item.VerifyMode, Verification: item.VerifyResult, Result: item.TypedResult}
			key := taskdistill.Digest(evidence)
			if !seen[key] {
				tasks = append(tasks, evidence)
				seen[key] = true
			}
		}
	}
	journal, err := readTaskJournalRecords(ctx, store.DB())
	if err != nil {
		return out, err
	}
	var cache []taskEvidence
	for _, record := range journal {
		var entry journalRecord
		if err = taskdistill.UnmarshalSnapshot(record, &entry); err != nil {
			return out, err
		}
		if entry.Op == "put" || entry.Op == "result" {
			cache = append(cache, taskEvidence{ID: entry.TaskID, Agent: entry.Agent, Description: entry.Desc, Status: entry.Op, Output: entry.Output, VerifyMode: entry.VerifyMode, Verification: entry.Verification, Result: entry.TypedResult})
		}
	}
	memoryEvidence, err := readRetiredMemoryEvidence(ctx, workspace, scope)
	if err != nil {
		return out, err
	}
	out.Protected = out.Protected || memoryEvidence.Protected
	// The complete canonical evidence hash is separate from the redacted
	// model projection. Receipt bytes are never altered in storage.
	out.Fingerprint = taskdistill.Digest(struct {
		Events   string
		Session  *SessionData
		Tree     *SessionTree
		Journal  [][]byte
		Memories []contextstore.ContextItem
	}{eventDigest, sd, st, journal, memoryEvidence.All})
	raw, err := taskdistill.MarshalSnapshot(struct {
		Tasks, Cache []taskEvidence
		Memories     []contextstore.ContextItem
	}{tasks, cache, memoryEvidence.Shared})
	if err != nil {
		return out, err
	}
	out.Content = utils.RedactSecrets(string(raw))
	if len(out.Content) > taskdistill.MaxSourceBytes {
		return out, fmt.Errorf("retired task evidence is %d bytes, exceeding the %d-byte extraction budget; source retained", len(out.Content), taskdistill.MaxSourceBytes)
	}
	if strings.TrimSpace(out.Content) == "" {
		return out, errors.New("empty retired snapshot")
	}
	return out, nil
}

type retiredMemoryEvidence struct {
	All, Shared []contextstore.ContextItem
	Protected   bool
}

func readRetiredMemoryEvidence(ctx context.Context, workspace string, scope contextstore.Scope) (retiredMemoryEvidence, error) {
	result := retiredMemoryEvidence{}
	contextPath := filepath.Join(workspace, "context.sqlite")
	if info, statErr := os.Lstat(contextPath); statErr == nil {
		if !info.Mode().IsRegular() {
			return result, errors.New("source context database must be a regular file")
		}
		if info.Size() > 8<<20 {
			return result, errors.New("source context database exceeds the snapshot budget")
		}
		facts, err := contextstore.InspectSQLiteSnapshot(ctx, contextPath)
		if err != nil {
			return result, err
		}
		for _, projectID := range facts.ProjectIDs {
			if projectID != scope.ProjectID {
				return result, errors.New("source context contains another project scope")
			}
		}
		repo, err := contextstore.OpenSQLiteReadOnly(contextPath)
		if err != nil {
			return result, err
		}
		err = repo.Iterate(ctx, contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: scope.ProjectID}, Visibility: contextstore.VisibilitySubtree, IncludeCandidates: true, IncludeSuperseded: true, IncludeExpired: true}, func(item contextstore.ContextItem) error {
			result.All = append(result.All, item)
			if activeDistillationMemory(item, contextstore.EvaluationTime(ctx)) {
				if !sharedPersistentDistillationMemory(item, scope) {
					// Agent-private, pinned and time-bounded knowledge cannot
					// authorize whole-source cleanup. Session/task scopes are
					// provenance for this explicit, review-gated promotion.
					result.Protected = true
				} else {
					result.Shared = append(result.Shared, item)
				}
			}
			if len(result.All) > 1000 {
				return errors.New("too many source memories for one distillation")
			}
			return nil
		})
		closeErr := repo.Close()
		if err = errors.Join(err, closeErr); err != nil {
			return result, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, statErr
	}
	return result, nil
}

func activeDistillationMemory(item contextstore.ContextItem, now time.Time) bool {
	return item.Lifecycle == contextstore.LifecycleConfirmed && item.SupersededBy == "" && (item.ExpiresAt == nil || item.ExpiresAt.After(now)) && (item.ValidUntil == nil || item.ValidUntil.After(now))
}

func sharedPersistentDistillationMemory(item contextstore.ContextItem, scope contextstore.Scope) bool {
	return scope.ProjectID != "" && scope.TeamID != "" && item.Scope.ProjectID == scope.ProjectID && (item.Scope.TeamID == "" || item.Scope.TeamID == scope.TeamID) && item.Scope.AgentID == "" && item.ExpiresAt == nil && item.ValidFrom == nil && item.ValidUntil == nil && !item.Pinned && !item.MustKeep
}

func retiredCurrentTasksProtected(groups [][]*TodoItem, tree *SessionTree, hasTree bool, eventGroups map[string][]*TodoItem) bool {
	protected := false
	// Legacy histories can contain tasks from earlier, superseded runs. Only
	// the current session checkpoint establishes live work; historical failures remain
	// model evidence and do not reopen work in a completed retired session.
	for _, group := range groups {
		for _, item := range group {
			if item != nil && item.Status != TaskDone && item.Status != TaskSkipped {
				protected = true
			}
		}
	}
	currentIDs := map[string]bool{}
	for _, group := range groups {
		for _, item := range group {
			if item != nil {
				currentIDs[item.ID] = true
			}
		}
	}
	activeBranch := "main"
	if hasTree {
		activeBranch = tree.ActiveBranch
	}
	currentEvents := eventGroups[activeBranch]
	if len(currentEvents) == 0 && activeBranch == "main" {
		currentEvents = eventGroups[""] // pre-branch legacy journals
	}
	for _, item := range currentEvents {
		if item != nil && currentIDs[item.ID] && item.Status != TaskDone && item.Status != TaskSkipped {
			protected = true
		}
	}
	return protected
}
