package team

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"

	"github.com/kjelly/hufu/internal/runtimestore"
)

type HistoricalResultGrantReport struct {
	CycleID      string                `json:"cycle_id"`
	GrantEventID string                `json:"grant_event_id"`
	State        string                `json:"state"`
	Grant        HistoricalResultGrant `json:"grant"`
}

// HistoricalResultGrantReport derives usage from checked canonical events,
// including recipients for which no task was created. It never loads bodies
// into a report or promotes a mutable checkpoint into dispatch evidence.
func (c *Coordinator) HistoricalResultGrantReport(ctx context.Context) ([]HistoricalResultGrantReport, error) {
	if c == nil || c.session == nil || !c.session.Scope.Managed {
		return nil, nil
	}
	payload, ref, lineage, err := c.historicalResultCycleSnapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("historical grant snapshot: %w", err)
	}
	if payload == nil {
		return nil, nil
	}
	tasks, err := ReplayTodoList(lineage)
	if err != nil {
		return nil, fmt.Errorf("historical grant tasks: %w", err)
	}
	byTask := make(map[string]*TodoItem, len(tasks))
	for _, task := range tasks {
		byTask[task.ID] = task
	}
	reports := make([]HistoricalResultGrantReport, len(payload.Grants))
	bySource := make(map[string]int, len(reports))
	for i, grant := range payload.Grants {
		reports[i] = HistoricalResultGrantReport{CycleID: ref.CycleID, GrantEventID: ref.EventID, State: "not_dispatched", Grant: grant}
		bySource[grant.Recipient+"\x00"+grant.ResultID] = i
	}
	for _, event := range lineage {
		if event.Type != "task_context_manifest" {
			continue
		}
		var data struct {
			ID        string                     `json:"id"`
			Manifests []ContextInjectionManifest `json:"context_manifests"`
		}
		if err := json.Unmarshal(event.Payload, &data, jsonv1.DefaultOptionsV1()); err != nil {
			return nil, err
		}
		for _, manifest := range data.Manifests {
			if manifest.SchemaVersion != HistoricalResultManifestSchemaVersion {
				continue
			}
			if err := applyHistoricalGrantReport(reports, bySource, byTask, ref, payload.PolicyFingerprint, lineage, event, data.ID, manifest); err != nil {
				return nil, err
			}
		}
	}
	return reports, nil
}

func applyHistoricalGrantReport(reports []HistoricalResultGrantReport, bySource map[string]int, tasks map[string]*TodoItem,
	ref *runtimestore.HistoricalGrantCycleRef, policy string, lineage []RunEvent, event RunEvent, taskID string, manifest ContextInjectionManifest) error {
	if !slices.ContainsFunc(manifest.Items, func(item ContextManifestItem) bool {
		return item.HistoricalResult != nil && item.HistoricalResult.Binding != nil && item.HistoricalResult.Binding.Grant == *ref
	}) {
		return nil
	}
	if err := validateHistoricalResultManifest(manifest); err != nil {
		return err
	}
	task := tasks[taskID]
	if task == nil || taskID != event.TaskID || manifest.TaskID != taskID || manifest.RunID != event.RunID ||
		event.SchemaVersion != eventStoreSchemaVersion {
		return fmt.Errorf("historical result report has a mismatched dispatch identity")
	}
	if task.HistoricalResultBinding == nil || task.HistoricalResultBinding.Grant != *ref {
		return nil // An earlier cycle's dispatch stays historical.
	}
	if err := validateHistoricalResultTaskBindingReplay(lineage, task); err != nil {
		return err
	}
	for _, item := range manifest.Items {
		a := item.HistoricalResult
		if a == nil || a.Binding.Grant != *ref {
			continue
		}
		if a.PolicyFingerprint != policy || !reflect.DeepEqual(a.Binding, task.HistoricalResultBinding) {
			return ErrHistoricalResultGrantDrift
		}
		index, found := bySource[a.Source.Recipient+"\x00"+a.Source.ResultID]
		if !found || reports[index].Grant != a.Source {
			return ErrHistoricalResultGrantDrift
		}
		if manifest.ModelCalled {
			reports[index].State = "dispatched"
		} else if reports[index].State != "dispatched" {
			reports[index].State = "prepared"
		}
	}
	return nil
}

func (c *Coordinator) historicalResultRoutingMetadata(ctx context.Context) (string, error) {
	payload, _, err := c.activeHistoricalResultGrantEnvelope(ctx)
	if err != nil || payload == nil {
		return "", err
	}
	type routing struct {
		Worker   string `json:"worker"`
		ResultID string `json:"result_id"`
	}
	rows := make([]routing, len(payload.Grants))
	for i, grant := range payload.Grants {
		rows[i] = routing{Worker: grant.Recipient, ResultID: grant.ResultID}
	}
	data, err := json.Marshal(rows, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return "## Operator historical-result routing\n\nThe runtime delivers complete historical reference data to every task of each listed worker in this cycle. Grants are not completion proof, do not require creating tasks, and confer no artifact access. Do not copy source contents into delegation prompts; workers receive them through runtime context.\n" + fenceUntrusted("json", string(data)), nil
}

func (c *Coordinator) historicalResultCycleSnapshot(ctx context.Context) (*HistoricalResultGrantsPayload, *runtimestore.HistoricalGrantCycleRef, []RunEvent, error) {
	scope, err := c.historicalResultGrantScope(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("grant scope: %w", err)
	}
	store := c.EventStore()
	if store == nil {
		store, err = OpenEventStoreReadOnly(scope.Workspace)
		if err != nil {
			return nil, nil, nil, err
		}
		defer func() { _ = store.Close() }()
	}
	events, err := store.ReadEvents()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("grant events: %w", err)
	}
	tree, err := LoadSessionTreeContext(ctx, scope.Workspace)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("grant session tree: %w", err)
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, scope.Branch)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("grant branch lineage: %w", err)
	}
	payload, err := store.validateHistoricalResultGrantProjection(ctx, lineage)
	if err != nil || payload == nil {
		return nil, nil, nil, err
	}
	if payload.Workspace != scope.Workspace || payload.Team != scope.Team {
		return nil, nil, nil, ErrHistoricalResultGrantDrift
	}
	ref, _, err := historicalResultGrantProjection(lineage)
	if err != nil {
		return nil, nil, nil, err
	}
	return payload, ref, lineage, nil
}
