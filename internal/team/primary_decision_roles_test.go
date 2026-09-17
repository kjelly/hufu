package team

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestResolvePrimaryDecisionRolesIsDeterministicAndMeetsDiversity(t *testing.T) {
	bundle, err := agent.ResolveBuiltInDecisionProfileBundle(agent.DecisionProfileBuiltinLightV2)
	if err != nil {
		t.Fatal(err)
	}
	constraints := agent.DefaultDecisionRoleConstraintsV1()
	constraints.Diversity.MinDistinctModels = 2
	constraints.Diversity.MinDistinctProviders = 2
	candidates := []DecisionRoleCandidate{
		primaryDecisionRoleTestCandidate("agent-b", "model-b", "provider-b", 10, "decision-analysis"),
		primaryDecisionRoleTestCandidate("agent-a", "model-a", "provider-a", 10, "decision-analysis"),
		primaryDecisionRoleTestCandidate("agent-c", "model-c", "provider-a", 5),
	}
	request := DecisionRoleResolutionRequest{LogicalRunID: "ldr_0123456789abcdef0123456789abcdef", Generation: 1, Bundle: bundle, Constraints: constraints, Candidates: candidates}
	first, err := ResolvePrimaryDecisionRoles(request)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(candidates)
	request.Candidates = candidates
	second, err := ResolvePrimaryDecisionRoles(request)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := CanonicalDecisionJSON(first)
	secondJSON, _ := CanonicalDecisionJSON(second)
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("role plan changed with candidate order\n%s\n%s", firstJSON, secondJSON)
	}
	if first.Diversity.DistinctModels != 2 || first.Diversity.DistinctProviders != 2 || first.Diversity.IsolatedJudgmentSlots != 2 || first.Diversity.StatisticalIndependenceProven {
		t.Fatalf("diversity = %#v", first.Diversity)
	}
	for _, binding := range first.Bindings {
		if binding.ToolPolicy == "none" && len(binding.AllowedToolIDs) != 0 {
			t.Fatalf("tool-less binding contains tools: %#v", binding)
		}
	}
}

func TestResolvePrimaryDecisionRolesFallbackAndHardCapabilityFailures(t *testing.T) {
	bundle, err := agent.ResolveBuiltInDecisionProfileBundle(agent.DecisionProfileBuiltinLightV2)
	if err != nil {
		t.Fatal(err)
	}
	fallback := primaryDecisionRoleRuntimeFallback("model-fallback", "provider-fallback")
	request := DecisionRoleResolutionRequest{
		LogicalRunID: "ldr_0123456789abcdef0123456789abcdef", Generation: 1, Bundle: bundle,
		Constraints: agent.DefaultDecisionRoleConstraintsV1(), Candidates: []DecisionRoleCandidate{fallback},
	}
	plan, err := ResolvePrimaryDecisionRoles(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range plan.Bindings {
		if binding.ExecutionMode != "runtime_reviewer" || binding.AgentID != nil || binding.SelectionReason != "runtime_fallback" || binding.FallbackFrom == nil {
			t.Fatalf("fallback binding = %#v", binding)
		}
	}

	request.Constraints.RequiredCapabilities.Judge = []string{"security-review"}
	_, err = ResolvePrimaryDecisionRoles(request)
	if !errors.Is(err, ErrDecisionProfileUnsatisfied) {
		t.Fatalf("required capability error = %v", err)
	}

	request.Constraints = agent.DefaultDecisionRoleConstraintsV1()
	request.Candidates[0].BackendCapability = DecisionBackendCapabilityV1{}
	_, err = ResolvePrimaryDecisionRoles(request)
	if !errors.Is(err, ErrDecisionBackendUnverified) {
		t.Fatalf("backend capability error = %v", err)
	}
}

func TestAdmitPrimaryDecisionContextsRequiresCommonEvidenceAndNoTools(t *testing.T) {
	bundle, err := agent.ResolveBuiltInDecisionProfileBundle(agent.DecisionProfileBuiltinLightV2)
	if err != nil {
		t.Fatal(err)
	}
	request := DecisionRoleResolutionRequest{
		LogicalRunID: "ldr_0123456789abcdef0123456789abcdef", Generation: 1, Bundle: bundle,
		Constraints: agent.DefaultDecisionRoleConstraintsV1(), Candidates: []DecisionRoleCandidate{primaryDecisionRoleTestCandidate("agent-a", "model-a", "provider-a", 1, "decision-analysis")},
	}
	plan, err := ResolvePrimaryDecisionRoles(request)
	if err != nil {
		t.Fatal(err)
	}
	evidenceHash := strings.Repeat("e", 64)
	prompts := make([]DecisionBindingPromptAdmission, 0, len(plan.Bindings))
	for _, binding := range plan.Bindings {
		prompts = append(prompts, DecisionBindingPromptAdmission{
			BindingID: binding.BindingID, EvidenceHash: evidenceHash, Prompt: []byte("sealed prompt"), CountedInputTokens: 13,
			CountMethod: "utf8_byte_upper_budget", ContextWindow: 1024, MaxOutputTokens: 128, SafetyMarginTokens: 64,
		})
	}
	receipt, err := AdmitPrimaryDecisionContexts(plan, evidenceHash, prompts)
	if err != nil {
		t.Fatal(err)
	}
	if !validDecisionDigest(receipt.Digest) || len(receipt.BindingResults) != len(plan.Bindings) {
		t.Fatalf("admission receipt = %#v", receipt)
	}
	prompts[0].ToolIDs = []string{"bash"}
	if _, err := AdmitPrimaryDecisionContexts(plan, evidenceHash, prompts); !errors.Is(err, ErrDecisionContextAdmission) {
		t.Fatalf("tool admission error = %v", err)
	}
	prompts[0].ToolIDs = nil
	prompts[0].ContextWindow = 0
	if _, err := AdmitPrimaryDecisionContexts(plan, evidenceHash, prompts); !errors.Is(err, ErrDecisionContextAdmission) {
		t.Fatalf("unknown context admission error = %v", err)
	}
}

func TestReservePrimaryDecisionBudgetFailsClosedAndSettlesOnce(t *testing.T) {
	bundle, err := agent.ResolveBuiltInDecisionProfileBundle(agent.DecisionProfileBuiltinLightV2)
	if err != nil {
		t.Fatal(err)
	}
	var ledger budgetLedger
	ledger.setLimits(0, 1000)
	reservation, err := ReservePrimaryDecisionBudget(&ledger, bundle, 0, 800, 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReservePrimaryDecisionBudget(&ledger, bundle, 0, 300, 0, time.Second); !errors.Is(err, ErrDecisionBudgetReservation) {
		t.Fatalf("over-reservation error = %v", err)
	}
	used := uint64(250)
	if !reservation.Settle(&used) || reservation.Settle(&used) || ledger.TokensUsed() != 250 || ledger.Reserved() != 0 {
		t.Fatalf("settlement used=%d reserved=%d", ledger.TokensUsed(), ledger.Reserved())
	}
	if _, err := ReservePrimaryDecisionBudget(&ledger, bundle, bundle.Limits.TotalDecisionTokens, 1, 0, 0); !errors.Is(err, ErrDecisionBudgetReservation) {
		t.Fatalf("profile token limit error = %v", err)
	}
}

func primaryDecisionRoleTestCandidate(agentID, modelID, providerID string, rank int, capabilities ...string) DecisionRoleCandidate {
	digest := strings.Repeat("a", 64)
	definitionDigest := strings.Repeat("b", 64)
	instructions := map[string]DecisionArtifactRef{}
	for _, role := range []string{"proposal", "reference", "judge", "challenge", "premortem"} {
		instructions[role] = primaryDecisionRoleTestRef("instruction-" + role)
	}
	return DecisionRoleCandidate{
		ExecutionMode: "team_agent", AgentID: &agentID, AgentDefinitionDigest: &definitionDigest,
		ExecutionTargetRef: primaryDecisionRoleTestRef("target-" + agentID), ModelIdentity: &modelID, ProviderIdentity: &providerID,
		InvocationPolicyDigest: digest, AuthorizationSnapshotRef: primaryDecisionRoleTestRef("auth-" + agentID), RoleInstructionRefs: instructions,
		Capabilities: slices.Clone(capabilities), Authorized: true, CapabilityRank: rank,
		BackendCapability: DecisionBackendCapabilityV1{SchemaVersion: 1, ToolIsolation: "none_enforced", SessionIsolation: true, DeclarationSource: "adapter"},
	}
}

func primaryDecisionRoleRuntimeFallback(modelID, providerID string) DecisionRoleCandidate {
	candidate := primaryDecisionRoleTestCandidate("unused", modelID, providerID, 0)
	candidate.ExecutionMode = "runtime_reviewer"
	candidate.AgentID = nil
	candidate.AgentDefinitionDigest = nil
	candidate.RuntimeFallback = true
	return candidate
}

func primaryDecisionRoleTestRef(id string) DecisionArtifactRef {
	return DecisionArtifactRef{ID: id, SHA256: strings.Repeat("c", 64), MediaType: "application/json", SizeBytes: 1}
}
