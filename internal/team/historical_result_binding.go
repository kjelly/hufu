package team

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/runtimestore"
)

const historicalResultTaskBindingVersion = 1

var ErrHistoricalResultTaskBinding = errors.New("historical result task binding differs from its canonical cycle grant")

// HistoricalResultTaskBinding is runtime-owned occurrence metadata. The grant
// event contains the immutable source/presentation digests; a task binds only
// its own identity, canonical recipient and the exact authorized result subset.
// Neither this projection nor possession of a ResultID grants artifact access.
type HistoricalResultTaskBinding struct {
	SchemaVersion int                                  `json:"schema_version"`
	TaskID        string                               `json:"task_id"`
	Recipient     string                               `json:"recipient"`
	Grant         runtimestore.HistoricalGrantCycleRef `json:"grant"`
	ResultIDs     []string                             `json:"result_ids"`
}

func (binding *HistoricalResultTaskBinding) clone() *HistoricalResultTaskBinding {
	if binding == nil {
		return nil
	}
	cloned := *binding
	cloned.ResultIDs = slices.Clone(binding.ResultIDs)
	return &cloned
}

func validateHistoricalResultTaskBindingShape(item *TodoItem) error {
	if item == nil || item.HistoricalResultBinding == nil {
		return nil
	}
	binding := item.HistoricalResultBinding
	if binding.SchemaVersion != historicalResultTaskBindingVersion || binding.TaskID != item.ID ||
		!historicalGrantIdentity(binding.TaskID) || !historicalGrantCanonicalName(binding.Recipient) || item.Source == TaskSourceSubagent || IsPrimaryOccurrence(item) ||
		!historicalGrantIdentity(binding.Grant.BranchID) || !historicalGrantIdentity(binding.Grant.CycleID) ||
		!historicalGrantIdentity(binding.Grant.EventID) || !historicalGrantSHA256(binding.Grant.EventHash) ||
		len(binding.ResultIDs) == 0 || len(binding.ResultIDs) > maxHistoricalResultGrants {
		return ErrHistoricalResultTaskBinding
	}
	for i, id := range binding.ResultIDs {
		if !strings.HasPrefix(id, "res-") || !historicalGrantSHA256(strings.TrimPrefix(id, "res-")) ||
			(i > 0 && binding.ResultIDs[i-1] >= id) {
			return ErrHistoricalResultTaskBinding
		}
	}
	return nil
}

// activeHistoricalResultGrantEnvelope resolves authority from the journal,
// not the optional checkpoint reference. The header-only query avoids copying
// unrelated result/transcript bodies for workflows that have never used grants.
func (c *Coordinator) activeHistoricalResultGrantEnvelope(ctx context.Context) (*HistoricalResultGrantsPayload, *runtimestore.HistoricalGrantCycleRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var checkpoint *runtimestore.HistoricalGrantCycleRef
	c.viewSessionData(func(sd *SessionData) { checkpoint = cloneHistoricalGrantCycleRef(sd.HistoricalResultGrants) })
	if !c.hasDurableEventJournal() {
		if checkpoint != nil {
			return nil, nil, ErrHistoricalResultTaskBinding
		}
		return nil, nil, nil
	}
	candidates, err := queryJournalEvents(ctx, c.EventJournal(), EventQuery{Types: []string{string(EventHistoricalResultsGranted)}})
	if err != nil {
		return nil, nil, err
	}
	if len(candidates) == 0 {
		if checkpoint != nil {
			return nil, nil, ErrHistoricalResultTaskBinding
		}
		return nil, nil, nil
	}
	payload, ref, branch, err := c.loadHistoricalResultGrantEnvelope(ctx)
	if err != nil {
		return nil, nil, err
	}
	if checkpoint != nil && !reflect.DeepEqual(checkpoint, ref) {
		return nil, nil, ErrHistoricalResultTaskBinding
	}
	if payload == nil {
		return nil, nil, nil
	}
	if err := c.validateHistoricalResultGrantConsumer(ctx, payload, branch); err != nil {
		return nil, nil, err
	}
	return payload, ref, nil
}

func (c *Coordinator) loadHistoricalResultGrantEnvelope(ctx context.Context) (*HistoricalResultGrantsPayload, *runtimestore.HistoricalGrantCycleRef, string, error) {
	store := c.EventStore()
	if store == nil || c.session == nil {
		return nil, nil, "", fmt.Errorf("historical result task binding requires its canonical event store")
	}
	root := cmp.Or(c.session.Scope.ControlRoot, c.artifactStoreRootPath())
	tree, err := LoadSessionTreeContext(ctx, root)
	if err != nil {
		return nil, nil, "", err
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	branch := cmp.Or(c.recoveryBranch, store.BranchID(), tree.ActiveBranch)
	lineage, err := ProjectValidatedEventsForBranch(events, tree, branch)
	if err != nil {
		return nil, nil, "", err
	}
	payload, err := store.validateHistoricalResultGrantProjection(ctx, lineage)
	if err != nil {
		return nil, nil, "", err
	}
	var ref *runtimestore.HistoricalGrantCycleRef
	if payload != nil {
		ref, _, err = historicalResultGrantProjection(lineage)
		if err != nil {
			return nil, nil, "", err
		}
	}
	return payload, ref, branch, nil
}

func (c *Coordinator) validateHistoricalResultGrantConsumer(ctx context.Context, payload *HistoricalResultGrantsPayload, branch string) error {
	scope, err := c.historicalResultGrantScope(ctx)
	if err != nil {
		return err
	}
	policy, err := c.historicalResultGrantPolicy(ctx, scope)
	if err != nil {
		return err
	}
	if scope.Workspace != payload.Workspace || scope.Team != payload.Team || scope.Branch != branch ||
		policy == nil || policy.ConfigurationHash != payload.PolicyFingerprint {
		return ErrHistoricalResultGrantDrift
	}
	if matches, err := c.executionPolicySnapshotMatchesCurrent(policy); err != nil || !matches {
		return ErrHistoricalResultGrantDrift
	}
	return nil
}

func (c *Coordinator) historicalResultBindingForTask(item *TodoItem, payload *HistoricalResultGrantsPayload, ref *runtimestore.HistoricalGrantCycleRef) (*HistoricalResultTaskBinding, error) {
	if item == nil {
		return nil, ErrHistoricalResultTaskBinding
	}
	if payload == nil || item.Source == TaskSourceSubagent || IsPrimaryOccurrence(item) {
		return nil, nil
	}
	def, _, err := c.resolveAgentName(item.Agent)
	if err != nil {
		return nil, err
	}
	recipient := strings.ToLower(strings.TrimSpace(def.Name))
	var ids []string
	for _, grant := range payload.Grants {
		if grant.Recipient == recipient {
			ids = append(ids, grant.ResultID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	role := strings.ToLower(strings.TrimSpace(def.Role))
	if ref == nil || role == "coordinator" || role == "orchestrator" {
		return nil, ErrHistoricalResultTaskBinding
	}
	binding := &HistoricalResultTaskBinding{SchemaVersion: historicalResultTaskBindingVersion, TaskID: item.ID,
		Recipient: recipient, Grant: *ref, ResultIDs: ids}
	bound := *item
	bound.HistoricalResultBinding = binding
	if err := validateHistoricalResultTaskBindingShape(&bound); err != nil {
		return nil, err
	}
	return binding, nil
}

// Freeze before occurrence/decision digests. Repeated envelope preparation may
// verify an existing binding but cannot replace it. This never creates tasks
// for unused recipients and never interprets task prose or ParentID.
func (c *Coordinator) freezeHistoricalResultTaskBindings(ctx context.Context, specs []TodoSpec, ids []string) error {
	if len(specs) != len(ids) {
		return fmt.Errorf("historical result binding task/identity count mismatch")
	}
	if len(specs) == 0 {
		return ctx.Err()
	}
	payload, ref, err := c.activeHistoricalResultGrantEnvelope(ctx)
	if err != nil {
		return err
	}
	for i := range specs {
		expected, err := c.historicalResultBindingForTask(todoItemFromSpec(specs[i], ids[i]), payload, ref)
		if err != nil {
			return err
		}
		if specs[i].HistoricalResultBinding != nil && !reflect.DeepEqual(specs[i].HistoricalResultBinding, expected) {
			return ErrHistoricalResultTaskBinding
		}
		specs[i].HistoricalResultBinding = expected
	}
	return nil
}

func (c *Coordinator) validateTaskHistoricalResultBinding(ctx context.Context, item *TodoItem) error {
	if item == nil {
		return ErrHistoricalResultTaskBinding
	}
	if err := validateHistoricalResultTaskBindingShape(item); err != nil {
		return err
	}
	if item.Source == TaskSourceSubagent || IsPrimaryOccurrence(item) {
		return ctx.Err()
	}
	// A fanout leaf executes the parent's admitted occurrence. Its temporary
	// worker world and model override cannot change the cycle's grant authority.
	owner := c.dependencyViewOwner(item.ID)
	payload, ref, err := owner.activeHistoricalResultGrantEnvelope(ctx)
	if err != nil {
		return err
	}
	expected, err := owner.historicalResultBindingForTask(item, payload, ref)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(item.HistoricalResultBinding, expected) {
		return ErrHistoricalResultTaskBinding
	}
	return nil
}
