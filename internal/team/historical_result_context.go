package team

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

const historicalResultContextKind = "historical_result"

// HistoricalResultContextAttribution is content-free delivery provenance.
// The original grant event remains the authority for every frozen digest.
type HistoricalResultContextAttribution struct {
	Binding           *HistoricalResultTaskBinding `json:"binding"`
	PolicyFingerprint string                       `json:"policy_fingerprint"`
	Source            HistoricalResultGrant        `json:"source"`
}

func (a *HistoricalResultContextAttribution) Clone() *HistoricalResultContextAttribution {
	if a == nil {
		return nil
	}
	cloned := *a
	cloned.Binding = a.Binding.clone()
	return &cloned
}

type historicalResultContext struct{ items []ContextItem }

func historicalResultContextID(resultID string) string {
	return historicalResultContextKind + ":" + resultID
}

func primaryHistoricalResultRequest(request ContextRequest) bool {
	return primaryDependencyViewRequest(ContextInjectionManifest{TaskID: request.TaskID, Trigger: request.Trigger, Purpose: request.Purpose})
}

// expectedHistoricalResultContext authenticates the grant on the consumer's
// lineage before rebuilding sources in that same lineage. An ordinary fork
// may keep its ancestor-owned grant, but cannot validate source status solely
// on the ancestor or substitute newer content for any frozen digest.
func (c *Coordinator) expectedHistoricalResultContext(ctx context.Context, item *TodoItem) (*historicalResultContext, error) {
	metadataCtx := ctx
	if item.HistoricalResultBinding == nil {
		// A negative metadata lookup must preserve the existing cancellation
		// path for tasks without grants. Required source reads keep ctx below.
		metadataCtx = context.WithoutCancel(ctx)
	}
	if err := c.validateTaskHistoricalResultBinding(metadataCtx, item); err != nil {
		return nil, err
	}
	if item.HistoricalResultBinding == nil {
		return nil, nil
	}
	lineage, err := c.dependencyViewLineage(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateHistoricalResultTaskBindingReplay(lineage, item); err != nil {
		return nil, err
	}
	payload, ref, err := c.activeHistoricalResultGrantEnvelope(ctx)
	if err != nil {
		return nil, err
	}
	if payload == nil || ref == nil || *ref != item.HistoricalResultBinding.Grant {
		return nil, ErrHistoricalResultTaskBinding
	}
	var grants []HistoricalResultGrant
	var requests []string
	for _, grant := range payload.Grants {
		if grant.Recipient == item.HistoricalResultBinding.Recipient {
			grants = append(grants, grant)
			requests = append(requests, grant.Recipient+"="+grant.ResultID)
		}
	}
	prepared, err := c.PrepareHistoricalResultGrants(ctx, requests)
	if err != nil {
		return nil, err
	}
	if prepared.scope.Workspace != payload.Workspace || prepared.scope.Team != payload.Team ||
		prepared.policyFingerprint != payload.PolicyFingerprint || !slices.Equal(prepared.grants, grants) {
		return nil, ErrHistoricalResultGrantDrift
	}
	result := &historicalResultContext{}
	for i, grant := range grants {
		result.items = append(result.items, ContextItem{
			ID: historicalResultContextID(grant.ResultID), Kind: historicalResultContextKind,
			Content: prepared.presentations[i], Source: "runtime:historical_result", Scope: ScopeTask,
			Priority: PriorityDependencyTaskResults, Required: true, Authority: ContextAuthorityHistorical,
			DedupKey: historicalResultContextID(grant.ResultID),
			HistoricalResult: &HistoricalResultContextAttribution{Binding: item.HistoricalResultBinding.clone(),
				PolicyFingerprint: payload.PolicyFingerprint, Source: grant},
		})
	}
	return result, nil
}

func (c *Coordinator) prepareHistoricalResultContext(ctx context.Context, input WorkerContextInput) (WorkerContextInput, error) {
	if input.HistoricalResults != nil {
		return input, fmt.Errorf("historical result context cannot accept a caller override")
	}
	if !primaryHistoricalResultRequest(input.Request) {
		return input, nil
	}
	owner := c.dependencyViewOwner(input.Request.TaskID)
	item := owner.todoItemByID(input.Request.TaskID)
	if item == nil {
		// Legacy/compiler-only callers without a durable task have no grant.
		return input, nil
	}
	view, err := owner.expectedHistoricalResultContext(ctx, item)
	if err != nil || view == nil {
		return input, err
	}
	def, _, err := owner.resolveAgentName(input.Request.AgentName)
	attempt := owner.taskAttempt(item.ID)
	if occurrence, active := owner.activeTaskResultOccurrence(item.ID); active {
		// Route fallback advances the dispatch attempt without incrementing
		// DAG retry statistics. The runtime occurrence owns that identity.
		attempt = occurrence.Attempt
	}
	if err != nil || input.Request.AgentRole != "worker" || strings.ToLower(strings.TrimSpace(def.Name)) != item.HistoricalResultBinding.Recipient ||
		input.Request.Attempt != attempt || input.Request.ModelExecutionID == "" {
		return input, fmt.Errorf("historical result request does not identify its admitted worker attempt")
	}
	if input.AgentDef == nil || !strings.EqualFold(strings.TrimSpace(input.AgentDef.Name), def.Name) ||
		!strings.EqualFold(strings.TrimSpace(input.TaskDef.Agent), strings.TrimSpace(item.Agent)) {
		return input, fmt.Errorf("historical result compiler recipient differs from its task")
	}
	if err := validateFrozenHistoricalResultContexts(item, view); err != nil {
		return input, err
	}
	input.HistoricalResults = view
	return input, nil
}

func historicalResultCompilerItems(view *historicalResultContext) []ContextItem {
	if view == nil {
		return nil
	}
	items := slices.Clone(view.items)
	for i := range items {
		items[i].HistoricalResult = items[i].HistoricalResult.Clone()
	}
	return items
}

func validateHistoricalResultContext(input WorkerContextInput, compiled CompiledContext) error {
	expected := map[string]ContextItem{}
	for _, item := range historicalResultCompilerItems(input.HistoricalResults) {
		expected[item.ID] = item
	}
	for _, item := range compiled.IncludedItems {
		if item.Kind != historicalResultContextKind && item.HistoricalResult == nil {
			continue
		}
		original, ok := expected[item.ID]
		if !ok || !item.Required || item.Compressed || item.Compressible || item.Content != original.Content ||
			item.Source != original.Source || item.Authority != original.Authority || item.Scope != original.Scope ||
			!reflect.DeepEqual(item.HistoricalResult, original.HistoricalResult) || !strings.Contains(compiled.Prompt, original.Content) {
			return fmt.Errorf("required historical result context was added, omitted, compressed or changed")
		}
		delete(expected, item.ID)
	}
	for _, item := range compiled.OmittedItems {
		if item.Kind == historicalResultContextKind || item.HistoricalResult != nil {
			return fmt.Errorf("required historical result context was omitted")
		}
	}
	if len(expected) != 0 {
		return fmt.Errorf("required historical result context is missing")
	}
	return nil
}
