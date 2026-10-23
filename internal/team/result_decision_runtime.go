package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/decisionrt"
)

type resultDecisionBinding struct {
	Slot          string      `json:"slot"`
	SchemaHash    string      `json:"schema_hash"`
	Collection    string      `json:"collection"`
	RowKey        string      `json:"row_key"`
	Group         string      `json:"group"`
	Outputs       []string    `json:"outputs"`
	SourceTask    string      `json:"source_task"`
	SourceHash    string      `json:"source_hash"`
	SourcePointer string      `json:"source_pointer"`
	SourceIndex   int         `json:"source_index"`
	Context       ArtifactRef `json:"context"`
}

type ResultDecisionProvenance struct {
	RowKey        string `json:"row_key"`
	Group         string `json:"group"`
	EventID       string `json:"event_id"`
	BindingDigest string `json:"binding_digest"`
	RequestDigest string `json:"request_digest"`
}

func resultDecisionSlot(scope, collection, row, group string) string {
	digest, _ := decisionObjectHash([]string{scope, collection, row, group})
	return digest
}

func newResultDecisionBinding(scope string, compiled *CompiledResultContract, plan resultFixedCollectionPlan, row resultFixedCollectionRow, group resultDecisionGroup) resultDecisionBinding {
	return resultDecisionBinding{Slot: resultDecisionSlot(scope, plan.spec.ItemsPointer, row.Key, group.ID), SchemaHash: compiled.SchemaSHA256, Collection: plan.spec.ItemsPointer, RowKey: row.Key, Group: group.ID, Outputs: slices.Clone(group.Outputs), SourceTask: row.SourceTask, SourceHash: row.SourceHash, SourcePointer: plan.spec.SourcePointer, SourceIndex: row.SourceIndex}
}

func sameResultDecisionBinding(expected, actual resultDecisionBinding) bool {
	expected.Context = actual.Context
	return reflect.DeepEqual(expected, actual)
}

func resultDecisionRequestContext(binding resultDecisionBinding, raw []byte) (map[string]any, error) {
	if len(raw) > resultDecisionContextMax || !utf8.Valid(raw) || decisionBytesHash(raw) != binding.Context.SHA256 {
		return nil, fmt.Errorf("decision_context_invalid")
	}
	digest, err := decisionObjectHash(binding)
	if err != nil {
		return nil, err
	}
	values := map[string]any{"binding_digest": digest}
	for i := range 12 {
		size := min(len(raw), 4096)
		for size > 0 && size < len(raw) && !utf8.RuneStart(raw[size]) {
			size--
		}
		values[fmt.Sprintf("context_%02d", i)] = string(raw[:size])
		raw = raw[size:]
	}
	if len(raw) > 0 {
		return nil, fmt.Errorf("decision_context_too_large")
	}
	return values, nil
}

// Scans the verified active lineage once. A started slot without settlement is
// intentionally not replayable: the provider may have received the request.
func findBoundDecision(events []RunEvent, slot string) (*decisionPrimitivePayload, string, bool, error) {
	var started, settled *decisionPrimitivePayload
	eventID := ""
	for _, event := range events {
		if event.Type != string(EventDecisionPrimitiveStarted) && event.Type != string(EventDecisionPrimitiveSettled) {
			continue
		}
		if err := validateDecisionPrimitiveEvent(event); err != nil {
			return nil, "", false, err
		}
		var p decisionPrimitivePayload
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return nil, "", false, err
		}
		if p.Binding == nil || p.Binding.Slot != slot {
			continue
		}
		if event.Type == string(EventDecisionPrimitiveStarted) {
			if started != nil {
				return nil, "", false, fmt.Errorf("decision_slot_conflict")
			}
			copyPayload := p
			started = &copyPayload
		} else {
			if settled != nil || started == nil || p.RequestDigest != started.RequestDigest || p.Scope != started.Scope || p.CatalogHash != started.CatalogHash || !reflect.DeepEqual(p.Binding, started.Binding) {
				return nil, "", false, fmt.Errorf("decision_slot_conflict")
			}
			copyPayload := p
			settled = &copyPayload
			eventID = event.ID
		}
	}
	return settled, eventID, started != nil, nil
}

func (c *Coordinator) validateBoundDecision(ctx context.Context, expected resultDecisionBinding, group resultDecisionGroup, payload decisionPrimitivePayload) error {
	if payload.Binding == nil || !sameResultDecisionBinding(expected, *payload.Binding) || payload.Name != group.CatalogEntry || payload.CatalogHash != c.decisionPrimitives.Hash() {
		return fmt.Errorf("decision_binding_drift")
	}
	raw, err := c.readDecisionArtifact(ctx, payload.Binding.Context, group.Context.MaxBytes)
	if err != nil {
		return err
	}
	values, err := resultDecisionRequestContext(*payload.Binding, raw)
	if err != nil {
		return err
	}
	// Request validation has no model side effect. The actor grant is checked at
	// admission/invocation; replay still validates the exact frozen request.
	entry := c.session.Config.DecisionPrimitives[group.CatalogEntry]
	if len(entry.Agents) == 0 {
		return fmt.Errorf("decision_grant_missing")
	}
	request, _, err := c.decisionPrimitives.Request(group.CatalogEntry, entry.Agents[0], values)
	if err != nil {
		return err
	}
	digest, err := decisionrt.Digest(request)
	if err != nil {
		return err
	}
	if digest != payload.RequestDigest {
		return fmt.Errorf("decision_request_drift")
	}
	if payload.ErrorCode != "" {
		return fmt.Errorf("%s", payload.ErrorCode)
	}
	if payload.Result == nil || payload.Receipt == nil {
		return fmt.Errorf("decision_settlement_invalid")
	}
	return c.decisionPrimitives.ValidatePublication(payload.Name, digest, *payload.Result, *payload.Receipt)
}

func (c *Coordinator) runBoundDecision(ctx context.Context, metadata InvocationMetadata, collection, key, groupID, callID string) (fantasy.ToolResponse, error) {
	reject := func(err error) (fantasy.ToolResponse, error) {
		return fantasy.NewTextErrorResponse(err.Error() + "; report blocked if a new decision is required"), nil
	}
	if err := c.validateDecisionOccurrence(metadata); err != nil {
		return reject(err)
	}
	_, compiled, err := c.boundResultContract(metadata.TaskID)
	if err != nil {
		return reject(err)
	}
	if !hasResultDecisions(compiled) {
		return reject(fmt.Errorf("decision_group_unavailable"))
	}
	if err := validateResultDecisionCatalog(c.session, c.session.Agents[metadata.AgentName], compiled); err != nil {
		return reject(err)
	}
	results := c.dependencyResultsForTask(metadata.TaskID)
	if err := c.validateDependencyPayloads(results); err != nil {
		return reject(err)
	}
	plans, err := c.fixedCollectionPlans(metadata.TaskID)
	if err != nil {
		return reject(err)
	}
	plan, row, group := boundDecisionSlot(plans, collection, key, groupID)
	if row == nil || group == nil {
		return reject(fmt.Errorf("decision_slot_unknown"))
	}
	if !c.hasDurableEventJournal() {
		return reject(fmt.Errorf("decision_journal_unavailable"))
	}
	select {
	case c.decisionPrimitiveGate <- struct{}{}:
		defer func() { <-c.decisionPrimitiveGate }()
	case <-ctx.Done():
		return fantasy.ToolResponse{}, ctx.Err()
	}
	if err := c.EventJournal().VerifyHashChain(ctx); err != nil {
		return fantasy.ToolResponse{}, err
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	scope := decisionPrimitiveScope(events, c.activeBranchID(), metadata)
	binding := newResultDecisionBinding(scope, compiled, *plan, *row, *group)
	previous, _, started, err := findBoundDecision(events, binding.Slot)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if previous != nil {
		if err := c.validateBoundDecision(ctx, binding, *group, *previous); err != nil {
			return reject(err)
		}
		if err := c.recordPrimitiveReuse(ctx, metadata, *previous, events); err != nil {
			return fantasy.ToolResponse{}, err
		}
		return boundDecisionResponse(*previous)
	}
	if started {
		return reject(fmt.Errorf("decision_interrupted"))
	}
	if decisionInferenceRepair(ctx) {
		return reject(fmt.Errorf("decision_repair_inference_denied"))
	}
	if exceeded, _ := c.budgetExceeded(); exceeded {
		return reject(fmt.Errorf("decision_budget_exceeded"))
	}
	raw, err := c.buildDecisionContext(ctx, events, scope, compiled, *row, group.Context.MaxBytes)
	if err != nil {
		return reject(err)
	}
	binding.Context, err = c.putDecisionArtifact(ctx, metadata, "decision_context", raw)
	if err != nil {
		return reject(err)
	}
	values, err := resultDecisionRequestContext(binding, raw)
	if err != nil {
		return reject(err)
	}
	request, limit, err := c.decisionPrimitives.Request(group.CatalogEntry, metadata.AgentName, values)
	if err != nil {
		return reject(err)
	}
	calls := 0
	for _, event := range events {
		if event.Type == string(EventDecisionPrimitiveStarted) {
			var p decisionPrimitivePayload
			if err := json.Unmarshal(event.Payload, &p); err != nil {
				return fantasy.ToolResponse{}, err
			}
			if p.Name == group.CatalogEntry && p.CatalogHash == c.decisionPrimitives.Hash() {
				calls++
			}
		}
	}
	if calls >= limit {
		return reject(fmt.Errorf("decision_call_limit"))
	}
	digest, err := decisionrt.Digest(request)
	if err != nil {
		return reject(err)
	}
	payload := decisionPrimitivePayload{Version: 2, Name: group.CatalogEntry, Scope: scope, CatalogHash: c.decisionPrimitives.Hash(), RequestDigest: digest, CallID: callID, Binding: &binding}
	if err := c.appendDecisionPrimitiveEvent(ctx, metadata, EventDecisionPrimitiveStarted, payload, binding.Slot+":start"); err != nil {
		return fantasy.ToolResponse{}, err
	}
	result, receipt, inferErr := c.decisionPrimitives.Decide(ctx, payload.Name, request)
	if inferErr != nil {
		payload.ErrorCode = decisionPrimitiveErrorCode(inferErr)
	} else {
		if err := c.decisionPrimitives.ValidatePublication(payload.Name, digest, result, receipt); err != nil {
			payload.ErrorCode = "decision_publication_invalid"
		} else {
			payload.Result = &result
			payload.Receipt = &receipt
		}
	}
	if err := c.appendDecisionPrimitiveEvent(context.WithoutCancel(ctx), metadata, EventDecisionPrimitiveSettled, payload, binding.Slot+":settled"); err != nil {
		return fantasy.ToolResponse{}, err
	}
	if ctx.Err() != nil {
		return fantasy.ToolResponse{}, ctx.Err()
	}
	return boundDecisionResponse(payload)
}

func boundDecisionSlot(plans []resultFixedCollectionPlan, collection, key, groupID string) (*resultFixedCollectionPlan, *resultFixedCollectionRow, *resultDecisionGroup) {
	var plan *resultFixedCollectionPlan
	var row *resultFixedCollectionRow
	var group *resultDecisionGroup
	for i := range plans {
		if plans[i].spec.ItemsPointer == collection {
			plan = &plans[i]
			break
		}
	}
	if plan != nil {
		for i := range plan.rows {
			if plan.rows[i].Key == key {
				row = &plan.rows[i]
				break
			}
		}
		for i := range plan.spec.DecisionGroups {
			if plan.spec.DecisionGroups[i].ID == groupID {
				group = &plan.spec.DecisionGroups[i]
				break
			}
		}
	}
	return plan, row, group
}

func boundDecisionResponse(p decisionPrimitivePayload) (fantasy.ToolResponse, error) {
	if p.ErrorCode != "" {
		return fantasy.NewTextErrorResponse(p.ErrorCode + "; report blocked"), nil
	}
	if p.Result == nil || p.Result.Status != decisionrt.StatusDecided {
		return fantasy.NewTextErrorResponse("decision_abstained; report blocked"), nil
	}
	raw, err := json.Marshal(p.Result)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return fantasy.NewTextResponse(string(raw) + "\nRuntime injects this decision. Submit only the remaining row fields."), nil
}

func (c *Coordinator) resolveDecisionCollectionPlans(ctx context.Context, todoID string, plans []resultFixedCollectionPlan) ([]resultFixedCollectionPlan, error) {
	return c.resolveDecisionPlans(ctx, todoID, plans, nil)
}

func (c *Coordinator) resolveDecisionPlans(ctx context.Context, todoID string, plans []resultFixedCollectionPlan, accepted []ResultCollectionBinding) ([]resultFixedCollectionPlan, error) {
	_, compiled, err := c.boundResultContract(todoID)
	if err != nil {
		return nil, err
	}
	if !hasResultDecisions(compiled) {
		return plans, nil
	}
	item := c.todoItemByID(todoID)
	if item == nil || !c.hasDurableEventJournal() {
		return nil, fmt.Errorf("decision_journal_unavailable")
	}
	if err := c.EventJournal().VerifyHashChain(ctx); err != nil {
		return nil, err
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	scope := decisionPrimitiveScope(events, c.activeBranchID(), InvocationMetadata{TaskID: todoID, AgentName: item.Agent})
	for i := range plans {
		plan := &plans[i]
		for j := range plan.rows {
			row := &plan.rows[j]
			row.Fixed = maps.Clone(row.Fixed)
			for _, group := range plan.spec.DecisionGroups {
				rowScope := scope
				if accepted != nil {
					var scopeErr error
					rowScope, scopeErr = acceptedDecisionScope(events, accepted, plan.spec.ItemsPointer, row.Key, group.ID, todoID, item.Agent)
					if scopeErr != nil {
						return nil, scopeErr
					}
				}
				expected := newResultDecisionBinding(rowScope, compiled, *plan, *row, group)
				payload, eventID, _, err := findBoundDecision(events, expected.Slot)
				if err != nil {
					return nil, err
				}
				if payload == nil {
					return nil, fmt.Errorf("decision_missing: %s/%s", row.Key, group.ID)
				}
				if err := c.validateBoundDecision(ctx, expected, group, *payload); err != nil {
					return nil, err
				}
				if payload.Result.Status != decisionrt.StatusDecided {
					return nil, fmt.Errorf("decision_abstained")
				}
				values, ok := group.ValueMap[payload.Result.Value.Choice]
				if !ok {
					return nil, fmt.Errorf("decision_mapping_invalid")
				}
				for field, value := range values {
					row.Fixed[field] = value
				}
				digest, err := decisionObjectHash(payload.Binding)
				if err != nil {
					return nil, err
				}
				plan.binding.Decisions = append(plan.binding.Decisions, ResultDecisionProvenance{RowKey: row.Key, Group: group.ID, EventID: eventID, BindingDigest: digest, RequestDigest: payload.RequestDigest})
			}
		}
	}
	return plans, nil
}

// Accepted ancestor results retain their original branch scope. A new
// submission still uses only the current scope and cannot import a receipt.
func acceptedDecisionScope(events []RunEvent, bindings []ResultCollectionBinding, collection, row, group, task, actor string) (string, error) {
	for _, binding := range bindings {
		if binding.ItemsPointer != collection {
			continue
		}
		for _, provenance := range binding.Decisions {
			if provenance.RowKey != row || provenance.Group != group {
				continue
			}
			for _, event := range events {
				if event.ID == provenance.EventID && event.Type == string(EventDecisionPrimitiveSettled) && event.TaskID == task && event.Actor == actor {
					return decisionPrimitiveScope(events, event.BranchID, InvocationMetadata{TaskID: task, AgentName: actor, RunID: event.RunID}), nil
				}
			}
		}
	}
	return "", fmt.Errorf("accepted decision provenance unavailable in active lineage")
}

func (c *Coordinator) validateDecisionOccurrence(metadata InvocationMetadata) error {
	active, ok := c.activeTaskResultOccurrence(metadata.TaskID)
	identity := c.completeOccurrenceLease(submitResultRuntimeIdentity{RunID: metadata.RunID, TaskID: metadata.TaskID, Agent: metadata.AgentName, Attempt: metadata.Attempt})
	if !ok || !validSubmitResultIdentity(identity) || identity != active {
		return fmt.Errorf("decision_occurrence_stale")
	}
	item := c.todoItemByID(metadata.TaskID)
	if item == nil || item.Agent != metadata.AgentName {
		return fmt.Errorf("decision_identity_missing")
	}
	return nil
}
