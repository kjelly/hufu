package team

import (
	"fmt"
	"sort"
	"strings"
)

// phaseContractHint describes one static contract of a workflow phase in the
// terms a coordinator dispatches it: the exact contract ID and agent.
type phaseContractHint struct {
	ID       string
	Agent    string
	Optional bool
}

func (h phaseContractHint) describe() string {
	optional := ""
	if h.Optional {
		optional = ", optional"
	}
	return fmt.Sprintf("%s (agent %s%s)", h.ID, h.Agent, optional)
}

// addPhaseContractHint records a contract for dispatch diagnostics.
func (w *runtimeWorkflow) addPhaseContractHint(task TaskDef) {
	if w.phaseContractHints == nil {
		w.phaseContractHints = make(map[Phase][]phaseContractHint)
	}
	hints := append(w.phaseContractHints[task.Phase], phaseContractHint{
		ID: runtimeContractID(task), Agent: strings.ToLower(strings.TrimSpace(task.Agent)),
		Optional: task.Optional,
	})
	sort.Slice(hints, func(i, j int) bool { return hints[i].ID < hints[j].ID })
	w.phaseContractHints[task.Phase] = hints
}

// phaseContractGuideLocked lists every static contract of the current phase so
// a rejected dispatch can be repaired in one step instead of one contract per
// rejection. The list is sorted, so the same mistake always gets the same
// guidance. w.mu must be held.
func (w *runtimeWorkflow) phaseContractGuideLocked() string {
	hints := w.phaseContractHints[w.state]
	if len(hints) == 0 {
		return ""
	}
	parts := make([]string, 0, len(hints))
	for _, hint := range hints {
		parts = append(parts, hint.describe())
	}
	return fmt.Sprintf("; phase %s dispatches these static contracts together in one batch: %s", w.state, strings.Join(parts, "; "))
}

// missingPhaseContractsLocked reports every required contract of the current
// phase that the batch did not dispatch, in contract order. w.mu must be held.
func (w *runtimeWorkflow) missingPhaseContractsLocked(provided map[string]bool) error {
	var missing []string
	for _, hint := range w.phaseContractHints[w.state] {
		if !hint.Optional && !provided[hint.ID] {
			missing = append(missing, hint.describe())
		}
	}
	for contractID := range w.phaseContracts[w.state] {
		if !provided[contractID] && !w.hasPhaseContractHintLocked(contractID) {
			missing = append(missing, contractID)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("workflow phase %s must dispatch static contract %s in the same batch%s",
		w.state, strings.Join(missing, "; "), w.phaseContractGuideLocked())
}

func (w *runtimeWorkflow) hasPhaseContractHintLocked(contractID string) bool {
	for _, hint := range w.phaseContractHints[w.state] {
		if hint.ID == contractID {
			return true
		}
	}
	return false
}
