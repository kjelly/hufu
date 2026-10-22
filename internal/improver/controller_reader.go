package improver

import (
	"context"
	json "encoding/json/v2"
	"errors"

	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

// LoadCommittedPlan reads only a creation committed in the requested branch.
// It does not trust a standalone report, an origin label, or a preview file.
// Callers must also compare the plan's build/policy/workspace identities with
// their current immutable runtime factory before attempting resume.
func LoadCommittedPlan(ctx context.Context, journal team.EventJournal, objects Objects, branch, experiment string) (paired.ExperimentPlan, LedgerScope, error) {
	if journal == nil || objects == nil || !opaqueID(branch) || !paired.ValidID(experiment) {
		return paired.ExperimentPlan{}, LedgerScope{}, errors.New("invalid experiment lookup")
	}
	if err := journal.VerifyHashChain(ctx); err != nil {
		return paired.ExperimentPlan{}, LedgerScope{}, err
	}
	events, err := journal.ReadEvents(ctx)
	if err != nil {
		return paired.ExperimentPlan{}, LedgerScope{}, err
	}
	var creation *team.RunEvent
	var payload runCreated
	for _, event := range events {
		if event.BranchID != branch || event.Type != eventCreated {
			continue
		}
		value, err := decodeEvent[runCreated](event)
		if err != nil {
			return paired.ExperimentPlan{}, LedgerScope{}, err
		}
		if value.ExperimentID != experiment {
			continue
		}
		if creation != nil || event.Actor != "improver" || value.SchemaVersion != 1 || value.Plan.Validate("experiment_plan") != nil {
			return paired.ExperimentPlan{}, LedgerScope{}, errors.New("conflicting or invalid experiment creation")
		}
		copy := event
		creation, payload = &copy, value
	}
	if creation == nil {
		return paired.ExperimentPlan{}, LedgerScope{}, errors.New("experiment not found in requested branch")
	}
	raw, err := objects.Read(payload.Plan)
	if err != nil {
		return paired.ExperimentPlan{}, LedgerScope{}, err
	}
	var plan paired.ExperimentPlan
	if err := json.Unmarshal(raw, &plan, json.RejectUnknownMembers(true)); err != nil {
		return paired.ExperimentPlan{}, LedgerScope{}, err
	}
	scope := LedgerScope{ExperimentID: experiment, WorkspaceID: plan.ParentWorkspaceID, BranchID: branch, RunID: creation.RunID, SessionID: creation.SessionID}
	if _, err := ReplayLedger(events, scope, plan); err != nil {
		return paired.ExperimentPlan{}, LedgerScope{}, err
	}
	return plan, scope, nil
}
