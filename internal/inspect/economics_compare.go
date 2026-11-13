package inspect

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"

	"github.com/kjelly/hufu/internal/team"
)

type EconomicsComparisonData struct {
	SchemaVersion        int                            `json:"schema_version"`
	ComparisonScope      string                         `json:"comparison_scope"`
	WorkloadID           string                         `json:"workload_id"`
	WorkloadSource       string                         `json:"workload_source"`
	SnapshotObservations []EconomicsSnapshotObservation `json:"snapshot_observations"`
	Runs                 []EconomicsComparedRun         `json:"runs"`
	Groups               []EconomicsComparisonGroup     `json:"groups"`
}

type EconomicsSnapshotRefs struct {
	PolicyHash        string   `json:"policy_hash,omitempty"`
	InputSnapshotID   string   `json:"input_snapshot_id,omitempty"`
	InputSnapshotHash string   `json:"input_snapshot_hash,omitempty"`
	InputSchemaHash   string   `json:"input_schema_hash,omitempty"`
	Unavailable       []string `json:"unavailable"`
}

type EconomicsSnapshotObservation struct {
	Field               string   `json:"field"`
	ObservedValues      []string `json:"observed_values"`
	Different           bool     `json:"different"`
	UnavailableRunCount int      `json:"unavailable_run_count"`
}

// InspectEconomicsComparison binds each selection once. Both amounts and
// snapshot references are projected exclusively from that frozen lineage.
// Group names and workload identity are operator declarations, not evidence
// of equivalent workloads or causal savings.
func InspectEconomicsComparison(ctx context.Context, manifestPath string) (*Envelope, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	manifest, err := loadEconomicsComparisonManifest(ctx, manifestPath)
	if err != nil {
		return nil, err
	}
	data := EconomicsComparisonData{SchemaVersion: 1, ComparisonScope: "operator_declared_groups_observed", WorkloadID: manifest.WorkloadID, WorkloadSource: "operator_declared", Runs: []EconomicsComparedRun{}}
	for _, selection := range manifest.Runs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		query, bound, err := bindObservationTarget(ctx, InspectQuery{Workspace: selection.Workspace, BranchID: selection.BranchID, RunID: selection.RunID})
		if err != nil {
			return nil, fmt.Errorf("selection %s/%s: %w", selection.Group, selection.RunID, err)
		}
		if bound.Scope.BranchID != selection.BranchID {
			return nil, fmt.Errorf("%w: branch_id must be a full canonical branch ID", ErrInvalidQuery)
		}
		run, err := projectEconomicsComparisonRun(ctx, selection, query, bound)
		if err != nil {
			return nil, err
		}
		data.Runs = append(data.Runs, run)
	}
	data.Groups, err = aggregateEconomicsComparisons(data.Runs)
	if err != nil {
		return nil, err
	}
	data.SnapshotObservations = observeEconomicsSnapshots(data.Runs)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return envelope(KindEconomicsCompare, InspectQuery{}, "", data), nil
}

func projectEconomicsComparisonRun(ctx context.Context, selection EconomicsRunSelection, query InspectQuery, bound BoundReadTarget) (EconomicsComparedRun, error) {
	data, err := projectEconomics(ctx, query, bound.Lineage)
	if err != nil {
		return EconomicsComparedRun{}, err
	}
	events, err := observationEvents(bound.Lineage, query)
	if err != nil {
		return EconomicsComparedRun{}, err
	}
	run := EconomicsComparedRun{Selection: selection, Scope: bound.Scope, Economics: data, CostReasons: []string{}, Snapshots: EconomicsSnapshotRefs{Unavailable: []string{}}}
	// no_samples qualifies the per-verified-task denominator, not cost coverage.
	// Preserve every other existing completeness check and its ordering.
	for _, reason := range data.CostPerVerifiedTaskReasons {
		if reason != "no_samples" {
			run.CostReasons = append(run.CostReasons, reason)
		}
	}
	snapshots := map[string]team.RunInputSnapshot{}
	var selectedInput *team.RunInputSnapshot
	for _, indexed := range events {
		if err := ctx.Err(); err != nil {
			return run, err
		}
		event := indexed.Event
		switch team.EventType(event.Type) {
		case team.EventExecutionPolicySnapshot:
			var policy team.ExecutionPolicySnapshot
			if err := json.Unmarshal(event.Payload, &policy); err != nil {
				return run, fmt.Errorf("%w: comparison policy: %v", ErrIntegrity, err)
			}
			run.Snapshots.PolicyHash = policy.ConfigurationHash
		case team.EventRunInputsResolved:
			var snapshot team.RunInputSnapshot
			if err := json.Unmarshal(event.Payload, &snapshot); err != nil || team.ValidateRunInputSnapshot(&snapshot) != nil || snapshot.RunID != event.RunID {
				return run, fmt.Errorf("%w: comparison input snapshot", ErrIntegrity)
			}
			snapshots[snapshot.ID] = snapshot
			if event.RunID == query.RunID {
				selectedInput = &snapshot
			}
		case team.EventRunFinished:
			if event.RunID != query.RunID {
				continue
			}
			run.Terminal = true
			if len(event.Payload) == 0 {
				continue
			}
			var terminal team.LifecycleEventPayload
			if err := json.Unmarshal(event.Payload, &terminal); err != nil {
				return run, fmt.Errorf("%w: comparison terminal inputs: %v", ErrIntegrity, err)
			}
			if terminal.RunInputs != nil {
				source, ok := snapshots[terminal.RunInputs.ID]
				origin := terminal.RunInputOriginRunID
				if origin == "" {
					origin = query.RunID
				}
				if !ok || source.SnapshotHash != terminal.RunInputs.SnapshotHash || source.RunID != origin {
					return run, fmt.Errorf("%w: comparison terminal input binding", ErrIntegrity)
				}
				selectedInput = &source
			}
		}
	}
	if selectedInput != nil {
		run.Snapshots.InputSnapshotID = selectedInput.ID
		run.Snapshots.InputSnapshotHash = selectedInput.SnapshotHash
		run.Snapshots.InputSchemaHash = selectedInput.SchemaHash
	}
	for field, value := range snapshotRefValues(run.Snapshots) {
		if value == "" {
			run.Snapshots.Unavailable = append(run.Snapshots.Unavailable, field)
		}
	}
	slices.Sort(run.Snapshots.Unavailable)
	return run, nil
}

func snapshotRefValues(refs EconomicsSnapshotRefs) map[string]string {
	return map[string]string{"policy_hash": refs.PolicyHash, "input_snapshot_id": refs.InputSnapshotID, "input_snapshot_hash": refs.InputSnapshotHash, "input_schema_hash": refs.InputSchemaHash}
}

func observeEconomicsSnapshots(runs []EconomicsComparedRun) []EconomicsSnapshotObservation {
	observations := []EconomicsSnapshotObservation{}
	for _, field := range slices.Sorted(maps.Keys(snapshotRefValues(EconomicsSnapshotRefs{}))) {
		values := map[string]bool{}
		observation := EconomicsSnapshotObservation{Field: field}
		for _, run := range runs {
			value := snapshotRefValues(run.Snapshots)[field]
			if value == "" {
				observation.UnavailableRunCount++
			} else {
				values[value] = true
			}
		}
		observation.ObservedValues = slices.Sorted(maps.Keys(values))
		observation.Different = len(values) > 1
		observations = append(observations, observation)
	}
	return observations
}
