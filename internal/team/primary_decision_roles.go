package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
)

const primaryDecisionRoleSearchLimit = 100_000

var (
	ErrDecisionProfileUnsatisfied = errors.New("decision profile unsatisfied")
	ErrDecisionRoleSearchBudget   = errors.New("decision role search budget exhausted")
	ErrDecisionBackendUnverified  = errors.New("decision backend capability unverified")
	ErrDecisionContextAdmission   = errors.New("decision context admission failed")
	ErrDecisionBudgetReservation  = errors.New("decision budget reservation failed")
)

type DecisionBackendCapabilityV1 struct {
	SchemaVersion     int    `json:"schema_version"`
	ToolIsolation     string `json:"tool_isolation"`
	SessionIsolation  bool   `json:"session_isolation"`
	DeclarationSource string `json:"declaration_source"`
}

func (value DecisionBackendCapabilityV1) ValidToolLess() bool {
	return value.SchemaVersion == 1 && value.ToolIsolation == "none_enforced" && value.SessionIsolation && value.DeclarationSource == "adapter"
}

type DecisionRoleCandidate struct {
	ExecutionMode            string
	AgentID                  *string
	AgentDefinitionDigest    *string
	ExecutionTargetRef       DecisionArtifactRef
	ModelIdentity            *string
	ProviderIdentity         *string
	InvocationPolicyDigest   string
	AuthorizationSnapshotRef DecisionArtifactRef
	RoleInstructionRefs      map[string]DecisionArtifactRef
	Capabilities             []string
	AllowedToolIDs           []string
	Authorized               bool
	AuthorizationDenied      bool
	RuntimeFallback          bool
	CapabilityRank           int
	BackendCapability        DecisionBackendCapabilityV1
}

type DecisionRoleBindingV1 struct {
	BindingID                string              `json:"binding_id"`
	Role                     string              `json:"role"`
	Ordinal                  uint32              `json:"ordinal"`
	ExecutionMode            string              `json:"execution_mode"`
	AgentID                  *string             `json:"agent_id"`
	AgentDefinitionDigest    *string             `json:"agent_definition_digest"`
	ExecutionTargetRef       DecisionArtifactRef `json:"execution_target_ref"`
	ModelIdentity            *string             `json:"model_identity"`
	ProviderIdentity         *string             `json:"provider_identity"`
	InvocationPolicyDigest   string              `json:"invocation_policy_digest"`
	AuthorizationSnapshotRef DecisionArtifactRef `json:"authorization_snapshot_ref"`
	RoleInstructionRef       DecisionArtifactRef `json:"role_instruction_ref"`
	ContextPolicy            string              `json:"context_policy"`
	ToolPolicy               string              `json:"tool_policy"`
	AllowedToolIDs           []string            `json:"allowed_tool_ids"`
	SelectionReason          string              `json:"selection_reason"`
	FallbackFrom             *string             `json:"fallback_from"`
	Provenance               string              `json:"provenance"`
	RevalidateOnResume       bool                `json:"revalidate_on_resume"`
}

type DecisionRolePlanDiversityV1 struct {
	DistinctAgents                uint32 `json:"distinct_agents"`
	DistinctModels                uint32 `json:"distinct_models"`
	DistinctProviders             uint32 `json:"distinct_providers"`
	UnknownAgentCount             uint32 `json:"unknown_agent_count"`
	UnknownModelCount             uint32 `json:"unknown_model_count"`
	UnknownProviderCount          uint32 `json:"unknown_provider_count"`
	IsolatedJudgmentSlots         uint32 `json:"isolated_judgment_slots"`
	StatisticalIndependenceProven bool   `json:"statistical_independence_proven"`
}

type DecisionRoleBindingPlanV1 struct {
	SchemaVersion       int                         `json:"schema_version"`
	Kind                string                      `json:"kind"`
	LogicalRunID        string                      `json:"logical_run_id"`
	Generation          uint32                      `json:"generation"`
	PolicyBundleDigest  string                      `json:"policy_bundle_digest"`
	ResolverVersion     string                      `json:"resolver_version"`
	Bindings            []DecisionRoleBindingV1     `json:"bindings"`
	RevisionBindingRule string                      `json:"revision_binding_rule"`
	Finalization        string                      `json:"finalization"`
	Diversity           DecisionRolePlanDiversityV1 `json:"diversity"`
}

type DecisionRoleResolutionRequest struct {
	LogicalRunID string
	Generation   uint32
	Bundle       agent.DecisionProfileBundleV2
	Constraints  agent.DecisionRoleConstraintsV1
	Candidates   []DecisionRoleCandidate
}

type decisionRoleSlot struct {
	role    string
	ordinal uint32
	spec    agent.DecisionProfileRoleSpecV2
}

type rankedDecisionRoleCandidate struct {
	candidate       DecisionRoleCandidate
	selectionReason string
	fallbackFrom    *string
}

// ResolvePrimaryDecisionRoles performs the bounded deterministic search from
// the V2 contract. It never consults wall-clock time or map iteration order.
func ResolvePrimaryDecisionRoles(request DecisionRoleResolutionRequest) (*DecisionRoleBindingPlanV1, error) {
	if err := validateDecisionRoleResolutionRequest(request); err != nil {
		return nil, err
	}
	slots := primaryDecisionRoleSlots(request.Bundle.RoleResolution)
	if len(slots) > 16 {
		return nil, fmt.Errorf("%w: role slots=%d", ErrDecisionProfileUnsatisfied, len(slots))
	}
	if len(request.Candidates) > request.Constraints.CandidateLimit {
		return nil, fmt.Errorf("%w: candidates=%d limit=%d", ErrDecisionProfileUnsatisfied, len(request.Candidates), request.Constraints.CandidateLimit)
	}
	pools := make([][]rankedDecisionRoleCandidate, len(slots))
	for index, slot := range slots {
		pool, err := eligibleDecisionRoleCandidates(slot, request)
		if err != nil {
			return nil, err
		}
		if len(pool) == 0 {
			return nil, fmt.Errorf("%w: no candidate for %s[%d]", ErrDecisionProfileUnsatisfied, slot.role, slot.ordinal)
		}
		pools[index] = pool
	}

	assignment := make([]rankedDecisionRoleCandidate, len(slots))
	expansions := 0
	var search func(int) bool
	search = func(index int) bool {
		if index == len(slots) {
			return decisionRoleDiversitySatisfied(slots, assignment, request)
		}
		ordered := orderDecisionRolePool(pools[index], slots[index], slots[:index], assignment[:index], request.Bundle.RoleResolution.Diversity)
		for _, candidate := range ordered {
			if !slots[index].spec.AllowRepeatedDefinition && repeatsDecisionRoleDefinition(slots[index], candidate, slots[:index], assignment[:index]) {
				continue
			}
			expansions++
			if expansions > primaryDecisionRoleSearchLimit {
				return false
			}
			assignment[index] = candidate
			if decisionRoleDiversityStillFeasible(slots, assignment, index+1, request) && search(index+1) {
				return true
			}
		}
		return false
	}
	if !search(0) {
		if expansions > primaryDecisionRoleSearchLimit {
			return nil, ErrDecisionRoleSearchBudget
		}
		return nil, ErrDecisionProfileUnsatisfied
	}
	return buildDecisionRoleBindingPlan(request, slots, assignment)
}

func repeatsDecisionRoleDefinition(slot decisionRoleSlot, candidate rankedDecisionRoleCandidate, priorSlots []decisionRoleSlot, prior []rankedDecisionRoleCandidate) bool {
	if candidate.candidate.AgentDefinitionDigest == nil {
		return false
	}
	for index, priorSlot := range priorSlots {
		if priorSlot.role == slot.role && prior[index].candidate.AgentDefinitionDigest != nil && *prior[index].candidate.AgentDefinitionDigest == *candidate.candidate.AgentDefinitionDigest {
			return true
		}
	}
	return false
}

func validateDecisionRoleResolutionRequest(request DecisionRoleResolutionRequest) error {
	if !decisionLogicalIDPattern.MatchString(request.LogicalRunID) || request.Generation == 0 || request.Bundle.Ref == "" || !validDecisionDigest(request.Bundle.BundleDigest) {
		return fmt.Errorf("invalid decision role resolution identity")
	}
	if err := request.Constraints.Validate(); err != nil {
		return err
	}
	return nil
}

func primaryDecisionRoleSlots(resolution agent.DecisionProfileRoleResolutionV2) []decisionRoleSlot {
	ordered := []struct {
		name string
		spec agent.DecisionProfileRoleSpecV2
	}{
		{"proposal", resolution.Roles.Proposal}, {"reference", resolution.Roles.Reference}, {"judge", resolution.Roles.Judge},
		{"challenge", resolution.Roles.Challenge}, {"premortem", resolution.Roles.Premortem},
	}
	result := make([]decisionRoleSlot, 0, 16)
	for _, role := range ordered {
		for ordinal := 1; ordinal <= role.spec.Count; ordinal++ {
			result = append(result, decisionRoleSlot{role: role.name, ordinal: uint32(ordinal), spec: role.spec})
		}
	}
	return result
}

func eligibleDecisionRoleCandidates(slot decisionRoleSlot, request DecisionRoleResolutionRequest) ([]rankedDecisionRoleCandidate, error) {
	required := unionDecisionCapabilities(slot.spec.RequiredCapabilities, decisionRoleCapabilitiesFor(request.Constraints.RequiredCapabilities, slot.role))
	preferred := unionDecisionCapabilities(slot.spec.PreferredCapabilities, decisionRoleCapabilitiesFor(request.Constraints.PreferredCapabilities, slot.role))
	teamCandidates := make([]rankedDecisionRoleCandidate, 0)
	runtimeCandidates := make([]rankedDecisionRoleCandidate, 0)
	backendUnverified := false
	for _, candidate := range request.Candidates {
		if !candidate.Authorized || candidate.AuthorizationDenied || !hasAllDecisionCapabilities(candidate.Capabilities, required) {
			continue
		}
		if err := validateDecisionRoleCandidate(candidate, slot); err != nil {
			if errors.Is(err, ErrDecisionBackendUnverified) && !candidate.RuntimeFallback {
				backendUnverified = true
				continue
			}
			return nil, err
		}
		ranked := rankedDecisionRoleCandidate{candidate: candidate, selectionReason: "authorized_generalist"}
		if hasAnyDecisionCapability(candidate.Capabilities, preferred) {
			ranked.selectionReason = "capability_match"
		}
		if candidate.RuntimeFallback {
			reason := "no_authorized_candidate"
			if len(preferred) > 0 {
				reason = "no_capability_match"
			}
			ranked.selectionReason = "runtime_fallback"
			ranked.fallbackFrom = &reason
			runtimeCandidates = append(runtimeCandidates, ranked)
		} else {
			teamCandidates = append(teamCandidates, ranked)
		}
	}
	if len(teamCandidates) > 0 {
		return teamCandidates, nil
	}
	if backendUnverified {
		return nil, ErrDecisionBackendUnverified
	}
	if len(required) > 0 || request.Constraints.Fallback == "forbid" || slot.spec.Fallback != "if-no-match" {
		return nil, nil
	}
	return runtimeCandidates, nil
}

func validateDecisionRoleCandidate(candidate DecisionRoleCandidate, slot decisionRoleSlot) error {
	if candidate.ExecutionMode != "team_agent" && candidate.ExecutionMode != "runtime_reviewer" ||
		validateDecisionArtifactRef(candidate.ExecutionTargetRef) != nil || validateDecisionArtifactRef(candidate.AuthorizationSnapshotRef) != nil ||
		!validDecisionDigest(candidate.InvocationPolicyDigest) {
		return fmt.Errorf("invalid decision role candidate")
	}
	instruction, ok := candidate.RoleInstructionRefs[slot.role]
	if !ok || validateDecisionArtifactRef(instruction) != nil {
		return fmt.Errorf("decision role candidate has no instruction for %s", slot.role)
	}
	if slot.spec.ToolPolicy == "none" {
		if len(candidate.AllowedToolIDs) > 0 || !candidate.BackendCapability.ValidToolLess() {
			return ErrDecisionBackendUnverified
		}
	} else if slot.spec.ToolPolicy != "authorized-read-only" {
		return fmt.Errorf("unsupported decision role tool policy %q", slot.spec.ToolPolicy)
	}
	if candidate.ExecutionMode == "runtime_reviewer" && (candidate.AgentID != nil || candidate.AgentDefinitionDigest != nil) {
		return fmt.Errorf("runtime reviewer cannot claim a team agent identity")
	}
	if candidate.ExecutionMode == "team_agent" && (candidate.AgentID == nil || candidate.AgentDefinitionDigest == nil || !validDecisionIdentifier(*candidate.AgentID) || !validDecisionDigest(*candidate.AgentDefinitionDigest)) {
		return fmt.Errorf("team agent candidate has invalid identity")
	}
	return nil
}

func orderDecisionRolePool(pool []rankedDecisionRoleCandidate, slot decisionRoleSlot, priorSlots []decisionRoleSlot, prior []rankedDecisionRoleCandidate, diversity agent.DecisionProfileDiversityV2) []rankedDecisionRoleCandidate {
	result := slices.Clone(pool)
	usedAgents, usedModels, usedProviders := decisionRoleIdentitySets(priorSlots, prior)
	sort.SliceStable(result, func(i, j int) bool {
		left, right := result[i], result[j]
		if left.selectionReason != right.selectionReason {
			return decisionSelectionReasonRank(left.selectionReason) < decisionSelectionReasonRank(right.selectionReason)
		}
		if slot.role == "judge" {
			leftNovel := decisionRoleNovelty(left.candidate, usedAgents, usedModels, usedProviders, diversity)
			rightNovel := decisionRoleNovelty(right.candidate, usedAgents, usedModels, usedProviders, diversity)
			if leftNovel != rightNovel {
				return leftNovel > rightNovel
			}
		}
		if left.candidate.CapabilityRank != right.candidate.CapabilityRank {
			return left.candidate.CapabilityRank > right.candidate.CapabilityRank
		}
		return decisionRoleCandidateIdentity(left.candidate) < decisionRoleCandidateIdentity(right.candidate)
	})
	return result
}

func decisionRoleNovelty(candidate DecisionRoleCandidate, agents, models, providers map[string]struct{}, diversity agent.DecisionProfileDiversityV2) int {
	result := 0
	if diversity.PreferDistinctAgents && candidate.AgentID != nil {
		if _, exists := agents[*candidate.AgentID]; !exists {
			result += 4
		}
	}
	if diversity.PreferDistinctModels && candidate.ModelIdentity != nil {
		if _, exists := models[*candidate.ModelIdentity]; !exists {
			result += 2
		}
	}
	if diversity.PreferDistinctProviders && candidate.ProviderIdentity != nil {
		if _, exists := providers[*candidate.ProviderIdentity]; !exists {
			result++
		}
	}
	return result
}

func decisionRoleDiversityStillFeasible(slots []decisionRoleSlot, assignment []rankedDecisionRoleCandidate, assigned int, request DecisionRoleResolutionRequest) bool {
	judgeSlots := 0
	for _, slot := range slots[assigned:] {
		if slot.role == "judge" {
			judgeSlots++
		}
	}
	agents, models, providers := decisionRoleIdentitySets(slots[:assigned], assignment[:assigned])
	floors := effectiveDecisionRoleDiversity(request)
	return len(agents)+judgeSlots >= floors.MinDistinctAgents && len(models)+judgeSlots >= floors.MinDistinctModels && len(providers)+judgeSlots >= floors.MinDistinctProviders
}

func decisionRoleDiversitySatisfied(slots []decisionRoleSlot, assignment []rankedDecisionRoleCandidate, request DecisionRoleResolutionRequest) bool {
	agents, models, providers := decisionRoleIdentitySets(slots, assignment)
	floors := effectiveDecisionRoleDiversity(request)
	return len(agents) >= floors.MinDistinctAgents && len(models) >= floors.MinDistinctModels && len(providers) >= floors.MinDistinctProviders
}

func effectiveDecisionRoleDiversity(request DecisionRoleResolutionRequest) agent.DecisionRoleDiversityConstraintsV1 {
	bundle := request.Bundle.RoleResolution.Diversity
	extra := request.Constraints.Diversity
	return agent.DecisionRoleDiversityConstraintsV1{
		MinDistinctAgents:    max(bundle.MinDistinctAgents, extra.MinDistinctAgents),
		MinDistinctModels:    max(bundle.MinDistinctModels, extra.MinDistinctModels),
		MinDistinctProviders: max(bundle.MinDistinctProviders, extra.MinDistinctProviders),
	}
}

func decisionRoleIdentitySets(slots []decisionRoleSlot, assignment []rankedDecisionRoleCandidate) (map[string]struct{}, map[string]struct{}, map[string]struct{}) {
	agents, models, providers := make(map[string]struct{}), make(map[string]struct{}), make(map[string]struct{})
	for index, slot := range slots {
		if slot.role != "judge" || index >= len(assignment) {
			continue
		}
		candidate := assignment[index].candidate
		if candidate.AgentID != nil {
			agents[*candidate.AgentID] = struct{}{}
		}
		if candidate.ModelIdentity != nil {
			models[*candidate.ModelIdentity] = struct{}{}
		}
		if candidate.ProviderIdentity != nil {
			providers[*candidate.ProviderIdentity] = struct{}{}
		}
	}
	return agents, models, providers
}

func buildDecisionRoleBindingPlan(request DecisionRoleResolutionRequest, slots []decisionRoleSlot, assignment []rankedDecisionRoleCandidate) (*DecisionRoleBindingPlanV1, error) {
	bindings := make([]DecisionRoleBindingV1, 0, len(slots))
	for index, slot := range slots {
		selected := assignment[index]
		instruction := selected.candidate.RoleInstructionRefs[slot.role]
		bindingID, err := decisionRoleBindingID(request.LogicalRunID, request.Generation, slot.role, slot.ordinal, selected.candidate)
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, DecisionRoleBindingV1{
			BindingID: bindingID, Role: slot.role, Ordinal: slot.ordinal, ExecutionMode: selected.candidate.ExecutionMode,
			AgentID: cloneStringPointer(selected.candidate.AgentID), AgentDefinitionDigest: cloneStringPointer(selected.candidate.AgentDefinitionDigest),
			ExecutionTargetRef: selected.candidate.ExecutionTargetRef, ModelIdentity: cloneStringPointer(selected.candidate.ModelIdentity), ProviderIdentity: cloneStringPointer(selected.candidate.ProviderIdentity),
			InvocationPolicyDigest: selected.candidate.InvocationPolicyDigest, AuthorizationSnapshotRef: selected.candidate.AuthorizationSnapshotRef,
			RoleInstructionRef: instruction, ContextPolicy: decisionRoleContextPolicy(slot.role), ToolPolicy: slot.spec.ToolPolicy,
			AllowedToolIDs: sortedUniquePrimaryEvidenceStrings(selected.candidate.AllowedToolIDs), SelectionReason: selected.selectionReason,
			FallbackFrom: cloneStringPointer(selected.fallbackFrom), Provenance: "runtime_bound", RevalidateOnResume: true,
		})
	}
	diversity := calculateDecisionRolePlanDiversity(bindings)
	plan := &DecisionRoleBindingPlanV1{
		SchemaVersion: 1, Kind: "decision_role_binding_plan", LogicalRunID: request.LogicalRunID, Generation: request.Generation,
		PolicyBundleDigest: request.Bundle.BundleDigest, ResolverVersion: "decision-role-resolver@v2", Bindings: bindings,
		RevisionBindingRule: "reuse_original_judge_binding", Finalization: "deterministic_aggregate", Diversity: diversity,
	}
	if err := ValidateDecisionRoleBindingPlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func decisionRoleBindingID(logicalRunID string, generation uint32, role string, ordinal uint32, candidate DecisionRoleCandidate) (string, error) {
	digest, err := DecisionContractDigest("hufu/decision-role-binding/v1", map[string]any{
		"execution_target_ref": candidate.ExecutionTargetRef, "generation": generation, "logical_run_id": logicalRunID,
		"ordinal": ordinal, "role": role,
	})
	if err != nil {
		return "", err
	}
	return "drb_" + digest, nil
}

func decisionRoleContextPolicy(role string) string {
	if role == "reference" {
		return "reference_read_only"
	}
	return "sealed_packet"
}

func calculateDecisionRolePlanDiversity(bindings []DecisionRoleBindingV1) DecisionRolePlanDiversityV1 {
	agents, models, providers := make(map[string]struct{}), make(map[string]struct{}), make(map[string]struct{})
	result := DecisionRolePlanDiversityV1{}
	for _, binding := range bindings {
		if binding.Role != "judge" {
			continue
		}
		result.IsolatedJudgmentSlots++
		if binding.AgentID == nil {
			result.UnknownAgentCount++
		} else {
			agents[*binding.AgentID] = struct{}{}
		}
		if binding.ModelIdentity == nil {
			result.UnknownModelCount++
		} else {
			models[*binding.ModelIdentity] = struct{}{}
		}
		if binding.ProviderIdentity == nil {
			result.UnknownProviderCount++
		} else {
			providers[*binding.ProviderIdentity] = struct{}{}
		}
	}
	result.DistinctAgents, result.DistinctModels, result.DistinctProviders = uint32(len(agents)), uint32(len(models)), uint32(len(providers))
	return result
}

func ValidateDecisionRoleBindingPlan(plan *DecisionRoleBindingPlanV1) error {
	if plan == nil || plan.SchemaVersion != 1 || plan.Kind != "decision_role_binding_plan" || !decisionLogicalIDPattern.MatchString(plan.LogicalRunID) || plan.Generation == 0 || !validDecisionDigest(plan.PolicyBundleDigest) || plan.ResolverVersion != "decision-role-resolver@v2" || plan.RevisionBindingRule != "reuse_original_judge_binding" || plan.Finalization != "deterministic_aggregate" {
		return fmt.Errorf("invalid decision role binding plan envelope")
	}
	seen := make(map[string]struct{}, len(plan.Bindings))
	for _, binding := range plan.Bindings {
		if err := validateDecisionRoleBinding(binding); err != nil {
			return err
		}
		if _, duplicate := seen[binding.BindingID]; duplicate {
			return fmt.Errorf("duplicate decision role binding %q", binding.BindingID)
		}
		seen[binding.BindingID] = struct{}{}
	}
	if plan.Diversity != calculateDecisionRolePlanDiversity(plan.Bindings) {
		return fmt.Errorf("decision role plan diversity summary mismatch")
	}
	return nil
}

func validateDecisionRoleBinding(binding DecisionRoleBindingV1) error {
	validRole := binding.Role == "proposal" || binding.Role == "reference" || binding.Role == "judge" || binding.Role == "challenge" || binding.Role == "premortem"
	validReason := binding.SelectionReason == "capability_match" || binding.SelectionReason == "authorized_generalist" || binding.SelectionReason == "runtime_fallback"
	if !validDecisionIdentifier(binding.BindingID) || !validRole || binding.Ordinal == 0 || !validReason || binding.Provenance != "runtime_bound" || !binding.RevalidateOnResume ||
		validateDecisionArtifactRef(binding.ExecutionTargetRef) != nil || validateDecisionArtifactRef(binding.AuthorizationSnapshotRef) != nil || validateDecisionArtifactRef(binding.RoleInstructionRef) != nil || !validDecisionDigest(binding.InvocationPolicyDigest) {
		return fmt.Errorf("invalid decision role binding %q", binding.BindingID)
	}
	if binding.ToolPolicy == "none" && len(binding.AllowedToolIDs) != 0 || binding.ToolPolicy != "none" && binding.ToolPolicy != "authorized-read-only" {
		return fmt.Errorf("invalid tool policy for decision role binding %q", binding.BindingID)
	}
	if binding.ExecutionMode == "runtime_reviewer" && (binding.AgentID != nil || binding.AgentDefinitionDigest != nil) {
		return fmt.Errorf("runtime reviewer binding claims an agent identity")
	}
	if binding.ExecutionMode == "team_agent" && (binding.AgentID == nil || binding.AgentDefinitionDigest == nil) {
		return fmt.Errorf("team agent binding has no agent identity")
	}
	if binding.SelectionReason == "runtime_fallback" && binding.FallbackFrom == nil || binding.SelectionReason != "runtime_fallback" && binding.FallbackFrom != nil {
		return fmt.Errorf("decision role binding has inconsistent fallback attribution")
	}
	return nil
}

func decisionRoleCapabilitiesFor(value agent.DecisionRoleCapabilitiesV1, role string) []string {
	switch role {
	case "proposal":
		return value.Proposal
	case "reference":
		return value.Reference
	case "judge":
		return value.Judge
	case "challenge":
		return value.Challenge
	case "premortem":
		return value.Premortem
	default:
		return nil
	}
}

func unionDecisionCapabilities(left, right []string) []string {
	result := append(slices.Clone(left), right...)
	slices.Sort(result)
	return slices.Compact(result)
}

func hasAllDecisionCapabilities(have, required []string) bool {
	set := sliceSet(have)
	for _, capability := range required {
		if _, ok := set[capability]; !ok {
			return false
		}
	}
	return true
}

func hasAnyDecisionCapability(have, preferred []string) bool {
	set := sliceSet(have)
	for _, capability := range preferred {
		if _, ok := set[capability]; ok {
			return true
		}
	}
	return false
}

func decisionSelectionReasonRank(reason string) int {
	switch reason {
	case "capability_match":
		return 0
	case "authorized_generalist":
		return 1
	default:
		return 2
	}
}

func decisionRoleCandidateIdentity(candidate DecisionRoleCandidate) string {
	values := []string{candidate.ExecutionMode, candidate.ExecutionTargetRef.SHA256}
	for _, value := range []*string{candidate.AgentID, candidate.ModelIdentity, candidate.ProviderIdentity} {
		if value == nil {
			values = append(values, "")
		} else {
			values = append(values, *value)
		}
	}
	return strings.Join(values, "\x00")
}

// PersistDecisionRoleBindingPlan stores the immutable plan used by prepared
// and admitted correctness events.
func PersistDecisionRoleBindingPlan(ctx context.Context, store ArtifactStore, plan *DecisionRoleBindingPlanV1) (DecisionArtifactRef, error) {
	if err := ValidateDecisionRoleBindingPlan(plan); err != nil {
		return DecisionArtifactRef{}, err
	}
	data, err := CanonicalDecisionJSON(plan)
	if err != nil {
		return DecisionArtifactRef{}, err
	}
	return putPrimaryEvidenceArtifact(ctx, store, "decision-role-plan", "application/json", data)
}
