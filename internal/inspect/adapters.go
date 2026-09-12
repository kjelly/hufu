package inspect

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/team"
)

type DecisionAddress struct {
	DecisionID            string            `json:"decision_id"`
	RunID                 string            `json:"run_id,omitempty"`
	TaskID                string            `json:"task_id,omitempty"`
	RecordRef             ArtifactMetadata  `json:"record_ref"`
	FinalizationResultRef *ArtifactMetadata `json:"finalization_result_ref,omitempty"`
	Resolved              bool              `json:"resolved"`
}

func LoadDecisionAddresses(query InspectQuery) ([]DecisionAddress, error) {
	entries, err := team.LoadDecisionIndexEntriesReadOnly(query.Workspace)
	if err != nil {
		return nil, fmt.Errorf("load decision addresses: %w", err)
	}
	out := make([]DecisionAddress, 0, len(entries))
	for _, entry := range entries {
		if query.RunID != "" && entry.RunID != query.RunID || query.TaskID != "" && entry.TaskID != query.TaskID {
			continue
		}
		address := DecisionAddress{DecisionID: entry.DecisionID, RunID: entry.RunID, TaskID: entry.TaskID, RecordRef: safeArtifactMetadata(entry.EffectiveRecordRef()), Resolved: entry.Resolved()}
		if entry.FinalizationResultRef != nil {
			ref := safeArtifactMetadata(*entry.FinalizationResultRef)
			address.FinalizationResultRef = &ref
		}
		out = append(out, address)
	}
	slices.SortFunc(out, func(left, right DecisionAddress) int { return cmp.Compare(left.DecisionID, right.DecisionID) })
	return out, nil
}

type TerminalFact struct {
	SessionID        string                    `json:"session_id"`
	RunID            string                    `json:"run_id"`
	OwnerTaskID      string                    `json:"owner_task_id"`
	ControllerTaskID string                    `json:"controller_task_id,omitempty"`
	Agent            string                    `json:"agent,omitempty"`
	State            team.TerminalSessionState `json:"state"`
	Running          bool                      `json:"running"`
	ExitCode         *int                      `json:"exit_code,omitempty"`
	OutputRefs       []ArtifactMetadata        `json:"output_refs"`
	CleanupState     team.TerminalCleanupState `json:"cleanup_state,omitempty"`
}

func LoadTerminalFacts(query InspectQuery) ([]TerminalFact, error) {
	sessions, err := team.LoadTerminalSessions(query.Workspace)
	if err != nil {
		return nil, fmt.Errorf("load terminal facts: %w", err)
	}
	out := make([]TerminalFact, 0, len(sessions))
	for _, session := range sessions {
		if query.RunID != "" && session.RunID != query.RunID || query.TaskID != "" && session.OwnerTaskID != query.TaskID && session.ControllerTaskID != query.TaskID {
			continue
		}
		fact := TerminalFact{
			SessionID: session.ID, RunID: session.RunID, OwnerTaskID: session.OwnerTaskID,
			ControllerTaskID: session.ControllerTaskID, Agent: session.Agent, State: session.State,
			Running: session.Running, ExitCode: session.ExitCode, OutputRefs: []ArtifactMetadata{}, CleanupState: session.CleanupState,
		}
		for _, ref := range session.OutputRefs {
			fact.OutputRefs = append(fact.OutputRefs, safeArtifactMetadata(ref))
		}
		out = append(out, fact)
	}
	slices.SortFunc(out, func(left, right TerminalFact) int { return cmp.Compare(left.SessionID, right.SessionID) })
	return out, nil
}
