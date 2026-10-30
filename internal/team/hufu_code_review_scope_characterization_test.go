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

func TestHufuCodeReviewChecksStatusAndGapsBeforeHandoff(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	checked := 0
	for _, task := range session.ContractTasks {
		if task.ID != "review-primary-workset" && task.ID != "review-documentation-workset" && task.ID != "review-documentation-escalation" {
			continue
		}
		checked++
		contract := taskResultSubmissionContractForTask(task)
		for _, tc := range []struct {
			status, gaps string
			valid        bool
		}{
			{"success", `[]`, true},
			{"completed_with_gaps", `[{"description":"bounded evidence","missing_evidence":"source"}]`, true},
			{"success", `[{"description":"bounded evidence","missing_evidence":"source"}]`, false},
			{"completed_with_gaps", `[]`, false},
		} {
			candidate := &TaskResult{Status: tc.status, Summary: "reviewed", FilesRead: []FileRef{{Path: "sha256-observed"}},
				StructuredPayload: &ResultPayload{Value: json.RawMessage(`{"record":{"findings":[],"invariant_assessments":[],"gaps":` + tc.gaps + `}}`)}}
			if err := contract.validateFinalizableResult(candidate); (err == nil) != tc.valid {
				t.Fatalf("%s status=%s gaps=%s: %v", task.ID, tc.status, tc.gaps, err)
			}
		}
	}
	if checked != 3 {
		t.Fatalf("checked %d review contracts, want 3", checked)
	}
}

func TestHufuCodeReviewChecksSourceFindingsBeforeHandoff(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	checked := 0
	for _, task := range session.ContractTasks {
		if task.ID != "review-primary-workset" && task.ID != "review-documentation-workset" && task.ID != "review-documentation-escalation" {
			continue
		}
		checked++
		if task.VerifySpec == nil {
			t.Fatalf("%s has no objective verification", task.ID)
		}
		var projection *TaskResultAssertion
		for i := range task.VerifySpec.TaskResultAssertions {
			assertion := &task.VerifySpec.TaskResultAssertions[i]
			if assertion.Op == "equals_projection" && assertion.Pointer == "/findings" {
				projection = assertion
			}
		}
		if projection == nil || projection.Pointer != "/findings" {
			t.Fatalf("%s omitted the findings projection", task.ID)
		}
		policy, err := decodeTaskResultProjection(projection.Value)
		if err != nil || policy.Pointer != "/structured_payload/value/record/findings" || !slices.Equal(policy.Fields, []string{"summary", "detail", "severity"}) || !policy.AllowMissing {
			t.Fatalf("%s projection=%#v error=%v", task.ID, policy, err)
		}
		contract := taskResultSubmissionContractForTask(task)
		candidate := &TaskResult{Status: TaskResultStatusCompletedWithGaps, Summary: "coverage limitation", FilesRead: []FileRef{{Path: "sha256-observed"}},
			Findings:          []Finding{{Summary: "gap", Detail: "cannot verify implementation", Severity: "info"}},
			StructuredPayload: &ResultPayload{Value: json.RawMessage(`{"record":{"gaps":[{"description":"bounded evidence","missing_evidence":"source"}],"findings":[],"invariant_assessments":[]}}`)},
		}
		if err := contract.validateFinalizableResult(candidate); err == nil || !strings.Contains(err.Error(), "exactly 0 items") {
			t.Fatalf("%s accepts unmirrored outer info finding: %v", task.ID, err)
		}
		candidate.Findings = nil
		if err := contract.validateFinalizableResult(candidate); err != nil {
			t.Fatalf("%s rejects a clean review with a coverage gap: %v", task.ID, err)
		}
	}
	if checked != 3 {
		t.Fatalf("checked %d source review contracts, want 3", checked)
	}
}

// TestHufuCodeReviewChecksInvariantCopiesBeforeHandoff pins the per-task
// check that the record repeats the attested invariant claims verbatim. The
// handoff inventory enforces the same equality, but only after reviewers can
// no longer be redispatched, so a reworded summary must be rejected here
// while submit_result can still be corrected.
func TestHufuCodeReviewChecksInvariantCopiesBeforeHandoff(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	var task *TaskDef
	for i := range session.ContractTasks {
		if session.ContractTasks[i].ID == "review-primary-workset" {
			task = &session.ContractTasks[i]
		}
	}
	if task == nil || task.VerifySpec == nil {
		t.Fatal("review-primary-workset has no objective verification")
	}
	contract := taskResultSubmissionContractForTask(*task)
	attested := &InvariantVerificationResult{Assessments: []InvariantAssessment{
		{InvariantID: "inv-a", Status: InvariantPreserved, Summary: "callers (bash.go, ssh.go) route through the same package symbols"},
		{InvariantID: "inv-b", Status: InvariantUnknown, Summary: "destination file not in the snapshot", MissingEvidence: []string{"path.go"}},
	}}
	tests := []struct {
		name    string
		record  string
		wantErr string
	}{
		{name: "verbatim copy", record: `[{"invariant_id":"inv-a","status":"preserved","summary":"callers (bash.go, ssh.go) route through the same package symbols"},{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot","missing_evidence":["path.go"]}]`},
		{name: "preserved claim linked to finding", record: `[{"invariant_id":"inv-a","status":"preserved","summary":"callers (bash.go, ssh.go) route through the same package symbols","finding_index":0},{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot","missing_evidence":["path.go"]}]`, wantErr: `optional field "finding_index"`},
		{name: "changed missing evidence", record: `[{"invariant_id":"inv-a","status":"preserved","summary":"callers (bash.go, ssh.go) route through the same package symbols"},{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot","missing_evidence":["other.go"]}]`, wantErr: `optional field "missing_evidence"`},
		{name: "reworded summary", record: `[{"invariant_id":"inv-a","status":"preserved","summary":"callers route through the same package symbols"},{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot"}]`, wantErr: `field "summary"`},
		{name: "changed status", record: `[{"invariant_id":"inv-a","status":"unknown","summary":"callers (bash.go, ssh.go) route through the same package symbols"},{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot"}]`, wantErr: `field "status"`},
		{name: "spurious zero finding index", record: `[{"invariant_id":"inv-a","status":"preserved","summary":"callers (bash.go, ssh.go) route through the same package symbols","finding_index":0},{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot","missing_evidence":["path.go"]}]`, wantErr: `field "finding_index"`},
		{name: "omitted missing evidence", record: `[{"invariant_id":"inv-a","status":"preserved","summary":"callers (bash.go, ssh.go) route through the same package symbols"},{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot"}]`, wantErr: `field "missing_evidence"`},
		{name: "empty missing evidence cannot replace nonempty", record: `[{"invariant_id":"inv-a","status":"preserved","summary":"callers (bash.go, ssh.go) route through the same package symbols","missing_evidence":[]},{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot","missing_evidence":[]}]`, wantErr: `field "missing_evidence"`},
		{name: "claim order instead of attested order", record: `[{"invariant_id":"inv-b","status":"unknown","summary":"destination file not in the snapshot"},{"invariant_id":"inv-a","status":"preserved","summary":"callers (bash.go, ssh.go) route through the same package symbols"}]`, wantErr: `field "invariant_id"`},
		{name: "omitted claim", record: `[{"invariant_id":"inv-a","status":"preserved","summary":"callers (bash.go, ssh.go) route through the same package symbols"}]`, wantErr: "exactly 1 items"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidate := &TaskResult{Status: TaskResultStatusCompletedWithGaps, Summary: "coverage limitation", FilesRead: []FileRef{{Path: "sha256-observed"}},
				InvariantVerification: attested,
				StructuredPayload:     &ResultPayload{Value: json.RawMessage(`{"record":{"gaps":[{"description":"bounded evidence","missing_evidence":"source"}],"findings":[],"invariant_assessments":` + tt.record + `}}`)},
			}
			err := contract.validateFinalizableResult(candidate)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verbatim invariant copy rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestHufuCodeReviewSequentialCritiquesAndFullSynthesisHandoff(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	session.Workspace = t.TempDir()
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	w.state = PhaseVerify
	w.results[PhaseVerify] = PhaseResult{Status: PhaseStatusSuccess}
	c := &Coordinator{session: session, taskTracker: NewTaskTracker()}
	ids := completedEvidenceSources(c, 11)
	// A successful documentation review by critic must not prevent a distinct
	// critique contract from checking another source in a later call.
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "critic"}})[0].Status = TaskDone
	for _, source := range ids[:2] {
		bound, _, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "critic", ContractID: "critic-review", EvidenceFrom: []string{source}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.validateTasks(bound); err != nil {
			t.Fatalf("single critique dispatch rejected: %v", err)
		}
		if err := c.validateDelegationPolicy(bound); err != nil {
			t.Fatalf("completed critic prevented a new source critique: %v", err)
		}
		if _, err := c.bindEvidenceSources(bound); err != nil {
			t.Fatal(err)
		}
	}
	bound, _, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "synthesizer", ContractID: "synthesize-review", EvidenceFrom: ids}, {Agent: "reviewer", ContractID: "verify-review-report"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.validateTasks(bound); err != nil {
		t.Fatal(err)
	}
	handoff, err := c.bindEvidenceSources(bound)
	if err != nil || !slices.Equal(handoff[0].EvidenceFrom, ids) {
		t.Fatalf("eleven-source synthesis handoff rejected or truncated: %#v %v", handoff, err)
	}
}

func TestHufuCodeReviewRepairExampleSatisfiesResultSchema(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	compiled := session.ResultContracts["schemas/review-v1.json"]
	if compiled == nil {
		t.Fatal("review result contract missing")
	}
	ref := compiled.ref(true)
	info := submitResultToolInfo(taskResultSubmissionContract{
		ResultContract: &ref, resultPayloadSchema: providerVisibleResultPayloadSchema(ref, compiled),
	})
	example := generateCompactToolExample(info)
	raw, err := json.Marshal(example)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateToolArguments(string(raw), info); err != nil {
		t.Fatalf("repair example violates tool schema: %v", err)
	}
	payload, err := json.Marshal(example["structured_payload"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateStructuredResultPayload(compiled, ref, payload); err != nil {
		t.Fatalf("repair example violates compiled result schema: %v; example=%s", err, raw)
	}
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
	if !strings.Contains(definition.Resolver.SemanticGuidance, "named commit") || !strings.Contains(definition.Resolver.SemanticGuidance, "working_tree") {
		t.Fatalf("review scope semantic guidance = %q", definition.Resolver.SemanticGuidance)
	}
	if string(definition.Default) != `{"count":10,"head":"HEAD","history":"first_parent","kind":"last_n"}` {
		t.Fatalf("review scope default = %s", definition.Default)
	}
	var producer, verifier *TaskDef
	for i := range session.ContractTasks {
		switch session.ContractTasks[i].ID {
		case "produce-workset":
			producer = &session.ContractTasks[i]
		case "verify-targeted-go-tests":
			verifier = &session.ContractTasks[i]
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
	if verifier == nil || verifier.Action == nil || verifier.Phase != PhasePrepare || verifier.Action.Capability != "verify-review-tests" {
		t.Fatalf("snapshot Go verifier contract = %#v", verifier)
	}
	if len(verifier.Action.InputBindings) != 1 || verifier.Action.InputBindings[0] != (ActionInputBinding{Input: "review.scope", Target: "/scope"}) {
		t.Fatalf("verifier input bindings = %#v", verifier.Action.InputBindings)
	}
	acceptance := session.Config.AcceptanceSpec
	if acceptance == nil || len(acceptance.Verifications) != 9 ||
		acceptance.Verifications[0].Type != VerifyTaskOutputAssert ||
		acceptance.Verifications[0].WorksetSourceTask != "verify-review-report" ||
		acceptance.Verifications[0].TaskOutputName != "review_report_verification" ||
		acceptance.Verifications[1].Type != VerifyTaskOutputAssert ||
		acceptance.Verifications[2].Type != VerifyTaskOutputAssert ||
		acceptance.Verifications[3].Type != VerifyTaskOutputAssert ||
		acceptance.Verifications[4].Type != VerifyTaskOutputAssert ||
		acceptance.Verifications[5].Type != VerifyWorksetComplete ||
		acceptance.Verifications[6].Type != VerifyWorksetComplete ||
		acceptance.Verifications[7].Type != VerifyWorksetComplete ||
		acceptance.Verifications[8].Type != VerifyTaskOutputAssert ||
		acceptance.Verifications[8].WorksetSourceTask != "inventory-review-handoffs" ||
		acceptance.Verifications[8].TaskOutputName != "review_handoff_inventory" {
		t.Fatalf("acceptance wiring = %#v", acceptance)
	}
	if coordinator := session.Agents["coordinator"]; coordinator == nil || strings.Contains(coordinator.System, "Natural-language scope text cannot override") {
		t.Fatalf("coordinator retained compatibility scope warning: %#v", coordinator)
	}
}

func TestHufuCodeReviewDeterministicResolverDoesNotInferNaturalLanguage(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	for _, capability := range []string{"resolve-review-scope", "produce-workset", "verify-review-tests"} {
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
	commit := "af6205cd743eba5b62412d9f4347952c74f37576"
	invalid, err := resolver.ResolveRunInput(t.Context(), RunInputResolverRequest{
		Type: "resolve_run_input", InputName: "review.scope", Prompt: "Review commit " + commit,
		ExplicitValue: json.RawMessage(`null`), CandidateValue: json.RawMessage(`{"kind":"working_tree","history":"first_parent","head":"` + commit + `"}`),
		SchemaHash: "sha256:fixture", ResolverID: "review-scope-v1",
	})
	if err != nil || invalid.Status != "invalid" || !strings.Contains(invalid.Diagnostic, "requires head HEAD") {
		t.Fatalf("invalid semantic candidate response = %#v, err = %v", invalid, err)
	}
	validValue := json.RawMessage(`{"kind":"last_n","count":1,"history":"first_parent","head":"` + commit + `"}`)
	valid, err := resolver.ResolveRunInput(t.Context(), RunInputResolverRequest{
		Type: "resolve_run_input", InputName: "review.scope", Prompt: "Review commit " + commit,
		ExplicitValue: json.RawMessage(`null`), CandidateValue: validValue,
		SchemaHash: "sha256:fixture", ResolverID: "review-scope-v1",
	})
	if err != nil || valid.Status != "matched" || string(valid.Value) != string(validValue) {
		t.Fatalf("valid semantic candidate response = %#v, err = %v", valid, err)
	}
	if name := session.ProviderRegistry.ProviderName("resolve-review-scope"); !strings.HasPrefix(name, "golang:sha256:") {
		t.Fatalf("provider identity = %q", name)
	}
}

// TestHufuCodeReviewWorkersShareTheReviewRoute pins the team's execution
// targets: every worker inherits the team's hufu.yaml `review` execution
// route (ordered Ollama cloud candidates with provider fallback), so none
// sets its own model or an agent backend, which a route cannot use.
func TestHufuCodeReviewWorkersShareTheReviewRoute(t *testing.T) {
	session := loadHufuCodeReviewTeam(t)
	if session.Config.ExecutionRoute != "review" {
		t.Fatalf("team execution route = %q, want review", session.Config.ExecutionRoute)
	}
	for _, name := range []string{"reviewer", "critic", "documentation-reviewer"} {
		worker := session.Agents[name]
		if worker == nil {
			t.Fatalf("hufu-code-review %s is missing", name)
		}
		if worker.Generation.Model != "" || worker.ExecutionRoute != "" || worker.SubagentProvider != "" {
			t.Fatalf("worker %q = model %q route %q provider %q, want the inherited team route", name, worker.Generation.Model, worker.ExecutionRoute, worker.SubagentProvider)
		}
	}
	if effort := session.Agents["documentation-reviewer"].Generation.ReasoningEffort; effort != "low" {
		t.Fatalf("documentation reviewer reasoning effort = %q, want low", effort)
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
		Agent:      "reviewer",
		Goal:       "Produce workset for the last 7 commits.",
		ContractID: "produce-workset",
		Action:     &Action{Payload: `{"max_commits":7}`},
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

func TestHufuCodeReviewReferenceScopeReachesWorkerPromptAndSubmission(t *testing.T) {
	for _, contractID := range []string{"review-primary-workset", "review-documentation-workset", "review-documentation-escalation"} {
		t.Run(contractID, func(t *testing.T) {
			session := loadHufuCodeReviewTeam(t)
			session.Workspace = t.TempDir()
			var parent TaskDef
			for _, task := range session.ContractTasks {
				if task.ID == contractID {
					parent = task
				}
			}
			if parent.FanOut == nil {
				t.Fatal("missing fan-out contract")
			}
			parent.FanOut = &FanOutSpec{Source: "manifest.json", GoalTemplate: parent.FanOut.GoalTemplate}
			writeWorksetManifest(t, session.Workspace, `{"schema_version":3,"items":[{"key":"unit-0000","bindings":{"key":"unit-0000","lens":"documentation-risk","review_revision":"frozen","reference_candidate_ids":"[]"}},{"key":"unit-0001","bindings":{"key":"unit-0001","lens":"documentation-risk","review_revision":"frozen","reference_candidate_ids":"[\"ref-000000\"]"}}]}`)
			c := &Coordinator{session: session, taskTracker: NewTaskTracker(), executionRunID: "run-scope"}
			children, err := c.expandFanOutTasks([]TaskDef{parent})
			if err != nil {
				t.Fatal(err)
			}
			for i, child := range children {
				ctx, err := CompileWorkerContext(t.Context(), WorkerContextInput{Goal: child.Goal, TaskDef: child, ModelContext: ModelContextSpec{ContextWindow: 16384, MaxOutputTokens: 256, SafetyMarginTokens: 64}})
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(ctx.Prompt, "Assigned reference_candidate_ids="+child.WorksetBinding.Bindings["reference_candidate_ids"]) || strings.Contains(ctx.Prompt, "{reference_candidate_ids}") {
					t.Fatalf("scope missing from actual compiled prompt: %s", ctx.Prompt)
				}
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: child.Agent, Desc: child.Goal, ResultContract: child.ResultContract, WorksetBinding: child.WorksetBinding}})[0]
				makePayload := func(include bool) []byte {
					refs := []any{}
					if include {
						refs = append(refs, map[string]any{"reference_id": "ref-000000", "kind": "deleted", "rationale": "The assigned source records scratch removal", "evidence": []string{"sha256-" + strings.Repeat("a", 64)}, "finding_index": nil, "gap_index": nil})
					}
					data, err := json.Marshal(map[string]any{"record": map[string]any{"kind": "review", "review_revision": "frozen", "workset_key": child.WorksetBinding.ItemKey, "lens": "documentation-risk", "findings": []any{}, "gaps": []any{}, "invariant_assessments": []any{}, "reference_assessments": refs}})
					if err != nil {
						t.Fatal(err)
					}
					return data
				}
				if _, message := c.structuredPayloadForSubmission(item.ID, makePayload(i == 0)); message == "" {
					t.Fatal("wrong item scope passed submission")
				}
				if _, message := c.structuredPayloadForSubmission(item.ID, makePayload(i == 1)); message != "" {
					t.Fatalf("corrected item scope rejected: %s", message)
				}
			}
		})
	}
}
