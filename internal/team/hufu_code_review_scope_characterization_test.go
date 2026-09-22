package team

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func hufuCodeReviewTeamDir(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(sourceFile), "..", "..", ".agent-teams", "hufu-code-review")
}

func loadHufuCodeReviewTeam(t *testing.T) *TeamSession {
	t.Helper()
	session, err := LoadTeam(hufuCodeReviewTeamDir(t), nil, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam(hufu-code-review): %v", err)
	}
	return session
}

func TestHufuCodeReviewDeclaresTypedScopeAndBindsProducer(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	if len(session.RunInputDefinitions) != 1 {
		t.Fatalf("run input definitions = %#v", session.RunInputDefinitions)
	}
	definition := session.RunInputDefinitions[0]
	if definition.Name != "review.scope" || definition.Resolver == nil || definition.Resolver.ID != "review-scope-v1" || definition.Resolver.Mode != runInputResolverModeSemanticJSON {
		t.Fatalf("review scope definition = %#v", definition)
	}
	if string(definition.Default) != `{"count":10,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("review scope default = %s", definition.Default)
	}
	var producer *TaskDef
	for i := range session.ContractTasks {
		if session.ContractTasks[i].ID == "produce-workset" {
			producer = &session.ContractTasks[i]
			break
		}
	}
	if producer == nil || producer.Action == nil {
		t.Fatal("hufu-code-review produce-workset static action is missing")
	}
	var payload struct {
		Scope   map[string]any `json:"scope"`
		Routing string         `json:"routing"`
	}
	if err := json.Unmarshal([]byte(producer.Action.Payload), &payload); err != nil {
		t.Fatalf("decode produce-workset payload: %v", err)
	}
	if payload.Scope["kind"] != "last_n" || payload.Scope["count"] != float64(10) {
		t.Fatalf("static scope = %#v, want typed last_n 10", payload.Scope)
	}
	if payload.Routing != "documentation" {
		t.Fatalf("producer routing = %q, want documentation", payload.Routing)
	}
	if len(producer.Action.InputBindings) != 1 || producer.Action.InputBindings[0] != (ActionInputBinding{Input: "review.scope", Target: "/scope"}) {
		t.Fatalf("producer input bindings = %#v", producer.Action.InputBindings)
	}
	acceptance := session.Config.AcceptanceSpec
	if acceptance == nil || len(acceptance.Verifications) != 5 ||
		acceptance.Verifications[0].Type != VerifyTaskOutputAssert ||
		acceptance.Verifications[1].Type != VerifyTaskOutputAssert ||
		acceptance.Verifications[2].Type != VerifyWorksetComplete ||
		acceptance.Verifications[3].Type != VerifyWorksetComplete ||
		acceptance.Verifications[4].Type != VerifyWorksetComplete {
		t.Fatalf("acceptance wiring = %#v", acceptance)
	}
	if coordinator := session.Agents["coordinator"]; coordinator == nil || strings.Contains(coordinator.System, "Natural-language scope text cannot override") {
		t.Fatalf("coordinator retained compatibility scope warning: %#v", coordinator)
	}
}

func TestHufuCodeReviewDeterministicResolverDoesNotInferNaturalLanguage(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	for _, capability := range []string{"resolve-review-scope", "produce-workset"} {
		config := session.Config.ActionProviders[capability]
		if config.Runtime != "golang" || config.Mode != "trusted-static" || config.Source != "./reviewprep" || len(config.Command) != 0 {
			t.Fatalf("action provider %q = %#v, want embedded trusted-static Go runtime without a command", capability, config)
		}
	}
	provider, ok := session.ProviderRegistry.Get("resolve-review-scope")
	if !ok {
		t.Fatal("resolve-review-scope provider is not registered")
	}
	resolver, ok := provider.(RunInputResolverProvider)
	if !ok {
		t.Fatalf("provider %T does not implement RunInputResolverProvider", provider)
	}
	for _, prompt := range []string{"Review the last 7 commits", "審查最近5個的 git commit", "Review the current git diff"} {
		response, err := resolver.ResolveRunInput(t.Context(), RunInputResolverRequest{
			Type: "resolve_run_input", InputName: "review.scope", Prompt: prompt,
			ExplicitValue: json.RawMessage(`null`), SchemaHash: "sha256:fixture", ResolverID: "review-scope-v1",
		})
		if err != nil {
			t.Fatalf("ResolveRunInput(%q): %v", prompt, err)
		}
		if response.Status != "no_match" || len(response.Value) != 0 || len(response.Evidence) != 0 {
			t.Fatalf("deterministic resolver inferred prompt %q: %#v", prompt, response)
		}
	}
	if name := session.ProviderRegistry.ProviderName("resolve-review-scope"); !strings.HasPrefix(name, "golang:sha256:") {
		t.Fatalf("provider identity = %q", name)
	}
}

func TestHufuCodeReviewUsesNativeLowCostDocumentationReviewer(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	documentationReviewer := session.Agents["documentation-reviewer"]
	if documentationReviewer == nil {
		t.Fatal("hufu-code-review documentation-reviewer is missing")
	}
	if documentationReviewer.Generation.Model != "minimax-m2.7:cloud" {
		t.Fatalf("documentation reviewer model = %q, want minimax-m2.7:cloud", documentationReviewer.Generation.Model)
	}
	if documentationReviewer.Generation.ReasoningEffort != "low" {
		t.Fatalf("documentation reviewer reasoning effort = %q, want low", documentationReviewer.Generation.ReasoningEffort)
	}
	if documentationReviewer.SubagentProvider != "" {
		t.Fatalf("documentation reviewer subagent provider = %q, want native default", documentationReviewer.SubagentProvider)
	}
	reviewer := session.Agents["reviewer"]
	if reviewer == nil || reviewer.SubagentProvider != "codex" || reviewer.Generation.Model != "gpt-5.6-sol" {
		t.Fatalf("high-reasoning reviewer execution target = %#v, want codex/gpt-5.6-sol", reviewer)
	}
	critic := session.Agents["critic"]
	if critic == nil || critic.Generation.Model != "" {
		t.Fatalf("high-reasoning critic execution target = %#v, want inherited model", critic)
	}
	coordinator := session.Agents["coordinator"]
	if coordinator == nil || coordinator.Generation.Model != "" {
		t.Fatalf("coordinator execution target = %#v, want inherited model", coordinator)
	}
	if session.Config.CoordinatorModel != "ollama/minimax-m3:cloud" {
		t.Fatalf("team coordinator model = %q, want ollama/minimax-m3:cloud", session.Config.CoordinatorModel)
	}
	for _, name := range []string{"reviewer", "critic"} {
		worker := session.Agents[name]
		if worker == nil || worker.Generation.ReasoningEffort != "high" {
			t.Fatalf("high-risk worker %q = %#v, want high reasoning effort", name, worker)
		}
	}
}

func TestHufuCodeReviewCharacterizesPromptCannotRewriteStaticScope(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	requested := []TaskDef{{
		Agent:  "reviewer",
		Goal:   "Produce workset for the last 7 commits.",
		Action: &Action{Payload: `{"max_commits":7}`},
	}}
	bound, _, err := CompileTaskGoalContracts(session, requested)
	if err != nil {
		t.Fatalf("CompileTaskGoalContracts: %v", err)
	}
	if len(bound) != 1 || bound[0].Action == nil {
		t.Fatalf("bound tasks = %#v", bound)
	}
	var payload struct {
		Scope map[string]any `json:"scope"`
	}
	if err := json.Unmarshal([]byte(bound[0].Action.Payload), &payload); err != nil {
		t.Fatalf("decode bound payload: %v", err)
	}
	if payload.Scope["count"] != float64(10) {
		t.Fatalf("prompt/model payload changed static scope to %#v, want configured default 10", payload.Scope)
	}
}

func TestHufuCodeReviewCompatibilityVarNoLongerControlsScope(t *testing.T) {
	session, err := LoadTeam(hufuCodeReviewTeamDir(t), map[string]string{"review.scope.max_commits": "3"}, nil, DefaultProviderRegistry)
	if err != nil {
		t.Fatalf("LoadTeam(hufu-code-review): %v", err)
	}
	for _, task := range session.ContractTasks {
		if task.ID != "produce-workset" || task.Action == nil {
			continue
		}
		var payload struct {
			Scope map[string]any `json:"scope"`
		}
		if err := json.Unmarshal([]byte(task.Action.Payload), &payload); err != nil {
			t.Fatalf("decode produce-workset payload: %v", err)
		}
		if payload.Scope["count"] != float64(10) {
			t.Fatalf("deprecated compatibility var changed typed scope: %#v", payload.Scope)
		}
		if coordinator := session.Agents["coordinator"]; coordinator == nil || strings.Contains(coordinator.System, "last 3 first-parent") {
			t.Fatalf("deprecated compatibility var leaked into coordinator prompt: %#v", coordinator)
		}
		return
	}
	t.Fatal("hufu-code-review produce-workset static action is missing")
}

func TestHufuCodeReviewGatesRecurringRuntimeInvariants(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)

	var reviewTask *TaskDef
	for index := range session.ContractTasks {
		if session.ContractTasks[index].ID == "review-primary-workset" {
			reviewTask = &session.ContractTasks[index]
			break
		}
	}
	if reviewTask == nil {
		t.Fatal("hufu-code-review review-primary-workset static task is missing")
	}
	if reviewTask.InvariantVerification != InvariantVerificationReport || reviewTask.Optional {
		t.Fatalf("review-primary-workset invariant policy = %q optional=%t, want report", reviewTask.InvariantVerification, reviewTask.Optional)
	}

	required := map[string]string{
		"runtime-owner-boundary":               "closest existing domain owner",
		"task-lifecycle-single-owner":          "Coordinator.executeTask",
		"decision-projection-lifecycle":        "durable decision/assumption occurrence",
		"provider-stream-process-owner":        "CodexRPCClient",
		"workspace-execution-world-boundary":   "ExecutionWorld",
		"resolved-model-identity":              "modelprofile catalog/runtime resolution",
		"durable-contract-provenance":          "durable task contract",
		"compatibility-normalization-boundary": "schema parsers and explicit migration adapters",
	}
	byID := make(map[string]InvariantDefinition, len(session.InvariantCatalog))
	for _, definition := range session.InvariantCatalog {
		byID[definition.ID] = definition
	}
	for invariantID, owner := range required {
		definition, ok := byID[invariantID]
		if !ok {
			t.Errorf("recurring runtime invariant %q is absent from the review gate", invariantID)
			continue
		}
		if definition.Severity != InvariantSeverityError {
			t.Errorf("recurring runtime invariant %q severity = %q, want error", invariantID, definition.Severity)
		}
		for _, requiredText := range []string{"Canonical owner:", owner, "Regression evidence:"} {
			if !strings.Contains(definition.Statement, requiredText) {
				t.Errorf("recurring runtime invariant %q omits %q", invariantID, requiredText)
			}
		}
	}

	hotspots := map[string][]string{
		"internal/team/future_runtime_boundary.go":           {"runtime-owner-boundary"},
		"internal/team/coordinator_task_run.go":              {"task-lifecycle-single-owner"},
		"internal/team/decision_assumptions.go":              {"decision-projection-lifecycle"},
		"internal/team/codex_appserver_client.go":            {"provider-stream-process-owner"},
		"internal/team/workspace_snapshot.go":                {"workspace-execution-world-boundary"},
		"internal/team/model_profile_runtime.go":             {"resolved-model-identity"},
		"internal/team/event_store.go":                       {"durable-contract-provenance"},
		"internal/team/execution_compatibility_migration.go": {"compatibility-normalization-boundary"},
	}
	for path, invariantIDs := range hotspots {
		for _, invariantID := range invariantIDs {
			definition, ok := byID[invariantID]
			if !ok {
				continue
			}
			if !invariantAppliesToTouchedPaths(definition, []string{path}) {
				t.Errorf("hotspot %q is not covered by recurring runtime invariant %q; applies-to=%v", path, invariantID, definition.AppliesTo)
			}
		}
	}

	memory, ok := byID["sqlite-canonical-memory"]
	if !ok || !slices.Contains(memory.AppliesTo, "internal/context/") {
		t.Errorf("canonical memory invariant lost internal/context coverage: %#v", memory)
	}
}
