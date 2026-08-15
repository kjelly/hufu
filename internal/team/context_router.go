package team

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
)

type ContextDecisionReason string

const (
	ContextIncludedRequired      ContextDecisionReason = "required"
	ContextIncludedRelevant      ContextDecisionReason = "relevant"
	ContextOmittedLifecycle      ContextDecisionReason = "lifecycle_ineligible"
	ContextOmittedExpired        ContextDecisionReason = "expired"
	ContextOmittedPhase          ContextDecisionReason = "phase_mismatch"
	ContextOmittedTrigger        ContextDecisionReason = "trigger_mismatch"
	ContextOmittedRole           ContextDecisionReason = "role_mismatch"
	ContextOmittedCapability     ContextDecisionReason = "capability_mismatch"
	ContextOmittedTool           ContextDecisionReason = "tool_mismatch"
	ContextOmittedErrorClass     ContextDecisionReason = "error_class_mismatch"
	ContextOmittedEnvironment    ContextDecisionReason = "environment_mismatch"
	ContextOmittedBelowThreshold ContextDecisionReason = "below_relevance"
	ContextOmittedInjectLimit    ContextDecisionReason = "inject_limit"
	ContextOmittedBudget         ContextDecisionReason = "token_budget"
	ContextOmittedDuplicate      ContextDecisionReason = "duplicate"
)

type ContextRouteDecision struct {
	ContextItemID string                `json:"context_item_id"`
	Included      bool                  `json:"included"`
	Reason        ContextDecisionReason `json:"reason"`
	BaseScore     float64               `json:"base_score,omitempty"`
	FinalScore    float64               `json:"final_score,omitempty"`
}

type ContextRoute struct {
	Request   ContextRequest
	Bundle    CanonicalContextBundle
	Decisions []ContextRouteDecision
}

type ContextRouter interface {
	Route(context.Context, ContextRequest) (ContextRoute, error)
}

type ContextActivation struct {
	Phases       []string
	Triggers     []string
	Roles        []string
	Capabilities []string
	Tools        []string
	ErrorClasses []string
	Environment  []string
}

func parseActivationValues(value string) []string {
	return normalizedRequestTokens(strings.Split(value, ","))
}

func ParseContextActivation(metadata map[string]string) (ContextActivation, error) {
	activation := ContextActivation{}
	if metadata == nil {
		return activation, nil
	}
	allowedKeys := map[string]bool{
		"activation.phases": true, "activation.triggers": true, "activation.roles": true,
		"activation.capabilities": true, "activation.tools": true,
		"activation.error_classes": true, "activation.environment": true,
	}
	for key := range metadata {
		if strings.HasPrefix(key, "activation.") && !allowedKeys[key] {
			return ContextActivation{}, fmt.Errorf("unsupported context activation key %q", key)
		}
	}
	fields := []struct {
		key string
		out *[]string
	}{{"activation.phases", &activation.Phases}, {"activation.triggers", &activation.Triggers}, {"activation.roles", &activation.Roles}, {"activation.capabilities", &activation.Capabilities}, {"activation.tools", &activation.Tools}, {"activation.error_classes", &activation.ErrorClasses}, {"activation.environment", &activation.Environment}}
	for _, field := range fields {
		if raw, ok := metadata[field.key]; ok {
			*field.out = parseActivationValues(raw)
			if len(*field.out) == 0 {
				return ContextActivation{}, fmt.Errorf("%s must contain at least one token", field.key)
			}
		}
	}
	for _, phase := range activation.Phases {
		if !validContextRequestPhase(Phase(strings.ToUpper(phase))) {
			return ContextActivation{}, fmt.Errorf("invalid activation phase %q", phase)
		}
	}
	for _, trigger := range activation.Triggers {
		if !validContextTrigger(ContextTrigger(trigger)) {
			return ContextActivation{}, fmt.Errorf("invalid activation trigger %q", trigger)
		}
	}
	return activation, nil
}

func tokenMatches(allowed []string, actual ...string) bool {
	if len(allowed) == 0 {
		return true
	}
	set := make(map[string]struct{}, len(actual))
	for _, value := range actual {
		set[strings.ToLower(strings.TrimSpace(value))] = struct{}{}
	}
	for _, value := range allowed {
		if _, ok := set[value]; ok {
			return true
		}
	}
	return false
}

func MatchContextActivation(activation ContextActivation, request ContextRequest) (bool, ContextDecisionReason) {
	if !tokenMatches(activation.Phases, string(request.Phase)) {
		return false, ContextOmittedPhase
	}
	if !tokenMatches(activation.Triggers, string(request.Trigger)) {
		return false, ContextOmittedTrigger
	}
	if !tokenMatches(activation.Roles, request.AgentRole) {
		return false, ContextOmittedRole
	}
	if !tokenMatches(activation.Capabilities, request.Capabilities...) {
		return false, ContextOmittedCapability
	}
	toolName, errorClass := "", ""
	if request.Failure != nil {
		toolName = request.Failure.ToolName
		errorClass = request.Failure.ErrorClass
		if errorClass == "" {
			errorClass = string(request.Failure.Class)
		}
	}
	if !tokenMatches(activation.Tools, toolName) {
		return false, ContextOmittedTool
	}
	if !tokenMatches(activation.ErrorClasses, errorClass) {
		return false, ContextOmittedErrorClass
	}
	if !tokenMatches(activation.Environment, request.EnvironmentFingerprint) {
		return false, ContextOmittedEnvironment
	}
	return true, ContextIncludedRelevant
}

func EvaluateContextEligibility(item contextstore.ContextItem, request ContextRequest, runID string, now time.Time) (bool, ContextDecisionReason, error) {
	if item.Lifecycle == contextstore.LifecycleCandidate {
		if runID == "" || item.Metadata["run_id"] != runID {
			return false, ContextOmittedLifecycle, nil
		}
	} else if item.Lifecycle != "" && item.Lifecycle != contextstore.LifecycleConfirmed {
		return false, ContextOmittedLifecycle, nil
	}
	if item.SupersededBy != "" {
		return false, ContextOmittedLifecycle, nil
	}
	if (item.ValidFrom != nil && now.Before(*item.ValidFrom)) || (item.ValidUntil != nil && !now.Before(*item.ValidUntil)) || (item.ExpiresAt != nil && !now.Before(*item.ExpiresAt)) {
		return false, ContextOmittedExpired, nil
	}
	if staleEnvironmentPenalty(item) > 0 {
		return false, ContextOmittedEnvironment, nil
	}
	activation, err := ParseContextActivation(item.Metadata)
	if err != nil {
		return false, "", err
	}
	// Verification is deliberately isolated from generic historical chatter.
	// Legacy metadata-free memory remains compatible with dispatch/retry in
	// other phases, but a verifier only receives memory explicitly activated
	// for VERIFY.
	if request.Phase == PhaseVerify && (len(activation.Phases) == 0 || !tokenMatches(activation.Phases, string(PhaseVerify))) {
		return false, ContextOmittedPhase, nil
	}
	eligible, reason := MatchContextActivation(activation, request)
	if !eligible {
		return false, reason, nil
	}
	if item.MustKeep {
		return true, ContextIncludedRequired, nil
	}
	return true, ContextIncludedRelevant, nil
}

type coordinatorContextRouter struct{ coordinator *Coordinator }

func (c *Coordinator) contextRouter() ContextRouter { return coordinatorContextRouter{coordinator: c} }

type typedActivationReader interface {
	ActivationForItem(context.Context, string) (contextstore.ActivationRecord, error)
}

func activationItemFromRepository(ctx context.Context, repo contextstore.Repository, item contextstore.ContextItem) (contextstore.ContextItem, error) {
	reader, ok := repo.(typedActivationReader)
	if !ok {
		return item, nil
	}
	record, err := reader.ActivationForItem(ctx, item.ID)
	if err != nil {
		return item, err
	}
	metadata := make(map[string]string, len(item.Metadata)+7)
	for key, value := range item.Metadata {
		metadata[key] = value
	}
	for key, value := range map[string]string{"activation.phases": record.Phases, "activation.triggers": record.Triggers, "activation.roles": record.Roles, "activation.capabilities": record.Capabilities, "activation.tools": record.Tools, "activation.error_classes": record.ErrorClasses, "activation.environment": record.Environment} {
		if strings.TrimSpace(value) == "" {
			delete(metadata, key)
		} else {
			metadata[key] = value
		}
	}
	item.Metadata = metadata
	return item, nil
}

func (router coordinatorContextRouter) Route(ctx context.Context, request ContextRequest) (ContextRoute, error) {
	if err := request.Validate(); err != nil {
		return ContextRoute{}, err
	}
	c := router.coordinator
	if c == nil || c.contextRepo == nil || c.session == nil {
		return ContextRoute{Request: request}, nil
	}
	scope := c.contextScope()
	sessionItems, err := c.sharedSessionPromptItems(ctx, scope)
	if err != nil {
		return ContextRoute{}, err
	}
	persistent, err := c.contextRepo.QuerySharedPersistentProjection(ctx, scope)
	if err != nil {
		return ContextRoute{}, err
	}
	route := ContextRoute{Request: request}
	now := time.Now().UTC()
	eligibleSession := make([]contextstore.ContextItem, 0, len(sessionItems))
	for _, item := range sessionItems {
		item, err = activationItemFromRepository(ctx, c.contextRepo, item)
		if err != nil {
			return ContextRoute{}, fmt.Errorf("context item %s activation projection: %w", item.ID, err)
		}
		eligible, reason, eligibilityErr := EvaluateContextEligibility(item, request, c.executionRunID, now)
		if eligibilityErr != nil {
			return ContextRoute{}, fmt.Errorf("context item %s activation: %w", item.ID, eligibilityErr)
		}
		route.Decisions = append(route.Decisions, ContextRouteDecision{ContextItemID: item.ID, Included: eligible, Reason: reason})
		if eligible {
			eligibleSession = append(eligibleSession, item)
		}
	}
	eligiblePersistent := make([]contextstore.ContextItem, 0, len(persistent))
	allowedPersistent := make(map[string]bool, len(persistent))
	for _, item := range persistent {
		item, err = activationItemFromRepository(ctx, c.contextRepo, item)
		if err != nil {
			return ContextRoute{}, fmt.Errorf("context item %s activation projection: %w", item.ID, err)
		}
		eligible, reason, eligibilityErr := EvaluateContextEligibility(item, request, c.executionRunID, now)
		if eligibilityErr != nil {
			return ContextRoute{}, fmt.Errorf("context item %s activation: %w", item.ID, eligibilityErr)
		}
		if !eligible {
			route.Decisions = append(route.Decisions, ContextRouteDecision{ContextItemID: item.ID, Reason: reason})
			continue
		}
		eligiblePersistent = append(eligiblePersistent, item)
		allowedPersistent[item.ID] = true
	}
	selected, scores, finalScores, err := c.rankSharedPersistentMemoryAllowed(ctx, request.RetrievalQuery(), eligiblePersistent, allowedPersistent)
	if err != nil {
		return ContextRoute{}, err
	}
	selectedLookup := make(map[string]bool, len(selected))
	for _, item := range selected {
		selectedLookup[item.ID] = true
	}
	for i := len(eligiblePersistent) - 1; i >= 0; i-- {
		item := eligiblePersistent[i]
		if item.MustKeep && !selectedLookup[item.ID] {
			selected = append([]contextstore.ContextItem{item}, selected...)
			selectedLookup[item.ID] = true
			if scores == nil {
				scores = make(map[string]MemoryScoreParts)
			}
			if finalScores == nil {
				finalScores = make(map[string]float64)
			}
			scores[item.ID] = MemoryScoreParts{BaseRelevance: item.Confidence, Applicability: 1, Freshness: 1, TrustFactor: memoryTrustFactor(item.TrustLevel)}
			finalScores[item.ID] = item.Confidence
		}
	}
	selectedIDs := make(map[string]struct{}, len(selected))
	for _, item := range selected {
		selectedIDs[item.ID] = struct{}{}
		parts := scores[item.ID]
		route.Decisions = append(route.Decisions, ContextRouteDecision{ContextItemID: item.ID, Included: true, Reason: map[bool]ContextDecisionReason{true: ContextIncludedRequired, false: ContextIncludedRelevant}[item.MustKeep], BaseScore: parts.BaseRelevance, FinalScore: finalScores[item.ID]})
	}
	for _, item := range eligiblePersistent {
		if _, ok := selectedIDs[item.ID]; ok {
			continue
		}
		reason := ContextOmittedBelowThreshold
		if len(selected) >= c.effectiveMemoryRankingPolicy().InjectTopK {
			reason = ContextOmittedInjectLimit
		}
		route.Decisions = append(route.Decisions, ContextRouteDecision{ContextItemID: item.ID, Reason: reason})
	}
	sort.SliceStable(route.Decisions, func(i, j int) bool { return route.Decisions[i].ContextItemID < route.Decisions[j].ContextItemID })
	route.Bundle = CanonicalContextBundle{SharedSession: eligibleSession, SharedPersistent: selected, SharedPersistentScores: scores, SharedPersistentFinalScores: finalScores}
	return route, nil
}
