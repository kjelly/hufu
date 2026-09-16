package team

import (
	"slices"
	"strings"
	"testing"
	"time"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func invariantAttestationFixture(t *testing.T, mode InvariantVerificationMode, definitions ...InvariantDefinition) (*Coordinator, *TodoItem, ContextInjectionManifest) {
	t.Helper()
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{
		PlanTaskID: "verify", Agent: "reviewer", Phase: PhaseVerify, InvariantVerification: mode,
	}})[0]
	c := &Coordinator{
		projectDir:  "/repo",
		taskTracker: tracker,
		session: &TeamSession{
			Config:           agent.TeamConfig{Name: "Review Team"},
			InvariantCatalog: cloneInvariantCatalog(definitions),
		},
	}
	manifest := ContextInjectionManifest{
		SchemaVersion: ContextManifestSchemaVersion,
		RequestID:     "request-1", RequestHash: "request-hash", RunID: "run-1",
		TaskID: item.ID, Attempt: 1, Agent: "reviewer", AgentRole: "worker",
		ModelExecutionID: "model-execution-1", Phase: PhaseVerify, Trigger: ContextTriggerTaskDispatch, Purpose: "task_execution",
		ModelCalled: true, Outcome: "model_call", CreatedAt: time.Unix(100, 0).UTC(),
	}
	for _, definition := range definitions {
		content := canonicalInvariantContent(definition)
		manifest.Items = append(manifest.Items, ContextManifestItem{
			ID: "invariant:review team:" + definition.ID, Kind: string(contextstore.ContextInvariant),
			Source: repositoryInvariantSource, Included: true, Reason: ContextIncludedRequired,
			ContentHash: fullContentHash(content), InvariantSeverity: definition.Severity,
		})
	}
	manifest.Fingerprint = contextManifestFingerprint(manifest)
	if err := tracker.TodoList().SetContextManifest(item.ID, &manifest); err != nil {
		t.Fatal(err)
	}
	return c, tracker.TodoList().Items()[0], manifest
}

func addInvariantRecoveryManifest(t *testing.T, c *Coordinator, item *TodoItem, primary ContextInjectionManifest, requestID string) ContextInjectionManifest {
	t.Helper()
	recovery := *cloneContextInjectionManifest(&primary)
	recovery.RequestID = requestID
	recovery.Trigger = ContextTriggerToolFailure
	recovery.Purpose = "tool_failure_recovery"
	recovery.ParentRequestID = primary.RequestID
	recovery.ParentManifestFingerprint = primary.Fingerprint
	recovery.Fingerprint = contextManifestFingerprint(recovery)
	if err := c.taskTracker.TodoList().SetContextManifest(item.ID, &recovery); err != nil {
		t.Fatal(err)
	}
	return recovery
}

func TestAttestInvariantClaimsBuildsCanonicalSortedEnvelope(t *testing.T) {
	c, item, manifest := invariantAttestationFixture(t, InvariantVerificationGate,
		InvariantDefinition{ID: "zeta", Statement: "preserve zeta", Severity: InvariantSeverityWarning, AppliesTo: []string{"*"}},
		InvariantDefinition{ID: "alpha", Statement: "preserve alpha", Severity: InvariantSeverityError, AppliesTo: []string{"internal/team/"}},
	)
	index := 0
	claims := []InvariantAssessmentClaim{
		{InvariantID: "zeta", Status: InvariantPreserved, Summary: " preserved "},
		{InvariantID: "alpha", Status: InvariantViolated, Summary: "regressed", FindingIndex: &index},
	}
	result := &TaskResult{Findings: []Finding{{Summary: "regression", Severity: FindingSeverityError}}}
	if err := c.attestInvariantClaims(item.ID, 1, manifest.ModelExecutionID, &claims, result); err != nil {
		t.Fatal(err)
	}
	verification := result.InvariantVerification
	if verification == nil || verification.ContextManifestFingerprint != manifest.Fingerprint || len(verification.Assessments) != 2 {
		t.Fatalf("verification = %#v", verification)
	}
	if got := []string{verification.Assessments[0].InvariantID, verification.Assessments[1].InvariantID}; !slices.Equal(got, []string{"alpha", "zeta"}) {
		t.Fatalf("assessment order = %v", got)
	}
	if verification.Assessments[0].ContextItemID != "invariant:review team:alpha" || verification.Assessments[0].InvariantContentHash == "" || verification.Assessments[0].FindingIndex == nil {
		t.Fatalf("canonical alpha assessment = %#v", verification.Assessments[0])
	}
	if verification.Assessments[1].Summary != "preserved" {
		t.Fatalf("trimmed summary = %q", verification.Assessments[1].Summary)
	}
}

func TestAttestInvariantClaimsRequiresExplicitEmptySet(t *testing.T) {
	c, item, manifest := invariantAttestationFixture(t, InvariantVerificationReport)
	result := &TaskResult{}
	empty := []InvariantAssessmentClaim{}
	if err := c.attestInvariantClaims(item.ID, 1, manifest.ModelExecutionID, &empty, result); err != nil {
		t.Fatal(err)
	}
	if result.InvariantVerification == nil || result.InvariantVerification.Assessments == nil || len(result.InvariantVerification.Assessments) != 0 {
		t.Fatalf("empty attestation = %#v, want non-nil empty assessments", result.InvariantVerification)
	}
	if err := c.attestInvariantClaims(item.ID, 1, manifest.ModelExecutionID, nil, &TaskResult{}); err == nil {
		t.Fatal("nil verifier claims accepted")
	}
}

func TestAttestInvariantClaimsRejectsInvalidClaimsAndAttribution(t *testing.T) {
	definition := InvariantDefinition{ID: "safe", Statement: "preserve safety", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}
	tests := []struct {
		name string
		edit func(*Coordinator, *TodoItem, *ContextInjectionManifest, *[]InvariantAssessmentClaim, *TaskResult)
		want string
	}{
		{name: "missing", edit: func(_ *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, claims *[]InvariantAssessmentClaim, _ *TaskResult) {
			*claims = nil
		}, want: "missing [safe]"},
		{name: "extra", edit: func(_ *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, claims *[]InvariantAssessmentClaim, _ *TaskResult) {
			*claims = append(*claims, InvariantAssessmentClaim{InvariantID: "extra", Status: InvariantPreserved, Summary: "ok"})
		}, want: "not included"},
		{name: "duplicate", edit: func(_ *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, claims *[]InvariantAssessmentClaim, _ *TaskResult) {
			*claims = append(*claims, (*claims)[0])
		}, want: "duplicates"},
		{name: "unknown status", edit: func(_ *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, claims *[]InvariantAssessmentClaim, _ *TaskResult) {
			(*claims)[0].Status = "maybe"
		}, want: "unknown value"},
		{name: "unknown evidence absent", edit: func(_ *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, claims *[]InvariantAssessmentClaim, _ *TaskResult) {
			(*claims)[0].Status = InvariantUnknown
		}, want: "requires missing_evidence"},
		{name: "violated index absent", edit: func(_ *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, claims *[]InvariantAssessmentClaim, _ *TaskResult) {
			(*claims)[0].Status = InvariantViolated
		}, want: "requires a non-negative finding_index"},
		{name: "finding severity mismatch", edit: func(_ *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, claims *[]InvariantAssessmentClaim, result *TaskResult) {
			index := 0
			(*claims)[0].Status = InvariantViolated
			(*claims)[0].FindingIndex = &index
			result.Findings = []Finding{{Summary: "bad", Severity: FindingSeverityWarning}}
		}, want: "does not equal definition severity"},
		{name: "unknown finding severity", edit: func(_ *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, _ *[]InvariantAssessmentClaim, result *TaskResult) {
			result.Findings = []Finding{{Severity: "critical"}}
		}, want: "unknown value"},
		{name: "stale catalog", edit: func(c *Coordinator, _ *TodoItem, _ *ContextInjectionManifest, _ *[]InvariantAssessmentClaim, _ *TaskResult) {
			c.session.InvariantCatalog[0].Statement = "changed"
		}, want: "changed since"},
		{name: "wrong model execution", edit: func(_ *Coordinator, _ *TodoItem, manifest *ContextInjectionManifest, _ *[]InvariantAssessmentClaim, _ *TaskResult) {
			manifest.ModelExecutionID = "wrong"
		}, want: "no context manifest"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, item, manifest := invariantAttestationFixture(t, InvariantVerificationGate, definition)
			claims := []InvariantAssessmentClaim{{InvariantID: "safe", Status: InvariantPreserved, Summary: "ok"}}
			result := &TaskResult{}
			tt.edit(c, item, &manifest, &claims, result)
			err := c.attestInvariantClaims(item.ID, 1, manifest.ModelExecutionID, &claims, result)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want substring %q", err, tt.want)
			}
			if result.InvariantVerification != nil {
				t.Fatalf("failed attestation persisted envelope: %#v", result.InvariantVerification)
			}
		})
	}
}

func TestAttestInvariantClaimsOrdinaryTaskRejectsExplicitArray(t *testing.T) {
	c, item, _ := invariantAttestationFixture(t, "")
	empty := []InvariantAssessmentClaim{}
	if err := c.attestInvariantClaims(item.ID, 1, "", &empty, &TaskResult{}); err == nil {
		t.Fatal("ordinary task accepted explicit invariant_assessments array")
	}
	if err := c.attestInvariantClaims(item.ID, 1, "", nil, &TaskResult{}); err != nil {
		t.Fatal(err)
	}
}

func TestCloneTaskResultDeepCopiesInvariantAttestation(t *testing.T) {
	index := 2
	original := &TaskResult{InvariantVerification: &InvariantVerificationResult{Assessments: []InvariantAssessment{{FindingIndex: &index, MissingEvidence: []string{"one"}}}}}
	cloned := cloneTaskResult(original)
	*cloned.InvariantVerification.Assessments[0].FindingIndex = 3
	cloned.InvariantVerification.Assessments[0].MissingEvidence[0] = "changed"
	if *original.InvariantVerification.Assessments[0].FindingIndex != 2 || original.InvariantVerification.Assessments[0].MissingEvidence[0] != "one" {
		t.Fatal("cloneTaskResult aliases invariant attestation")
	}
}

func TestSubmitResultInvariantClaimSchemaIsModeAware(t *testing.T) {
	ordinary := submitResultToolInfo(taskResultSubmissionContract{})
	if _, ok := ordinary.Parameters["invariant_assessments"]; !ok {
		t.Fatal("ordinary schema omitted invariant_assessments description")
	}
	if slices.Contains(ordinary.Required, "invariant_assessments") {
		t.Fatalf("ordinary required fields = %v", ordinary.Required)
	}
	verifier := submitResultToolInfo(taskResultSubmissionContract{InvariantVerification: InvariantVerificationReport})
	if !slices.Contains(verifier.Required, "invariant_assessments") {
		t.Fatalf("verifier required fields = %v", verifier.Required)
	}
	schema, _ := verifier.Parameters["invariant_assessments"].(map[string]any)
	item, _ := schema["items"].(map[string]any)
	if schema["maxItems"] != maxInvariantAssessmentClaims || item["additionalProperties"] != false {
		t.Fatalf("invariant claim schema = %#v", schema)
	}
}

func TestSubmitResultAttestsBeforePublishing(t *testing.T) {
	definition := InvariantDefinition{ID: "safe", Statement: "preserve safety", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}
	c, item, manifest := invariantAttestationFixture(t, InvariantVerificationReport, definition)
	ctx := withInvocationMetadata(occurrenceTestContext(c, item.ID, 1), invocationMetadataFromManifest(&manifest))
	tool := &submitResultTool{coordinator: c, todoID: item.ID}
	response, err := tool.Run(ctx, fantasy.ToolCall{Input: `{"status":"success","summary":"checked","invariant_assessments":[{"invariant_id":"safe","status":"preserved","summary":"preserved"}]}`})
	if err != nil || response.IsError {
		t.Fatalf("valid attested submit_result response=%#v err=%v", response, err)
	}
	stored := c.GetTaskResult(item.ID)
	if stored == nil || stored.InvariantVerification == nil || stored.InvariantVerification.ContextManifestFingerprint != manifest.Fingerprint {
		t.Fatalf("stored result = %#v", stored)
	}

	c2, item2, manifest2 := invariantAttestationFixture(t, InvariantVerificationReport, definition)
	ctx2 := withInvocationMetadata(occurrenceTestContext(c2, item2.ID, 1), invocationMetadataFromManifest(&manifest2))
	response, err = (&submitResultTool{coordinator: c2, todoID: item2.ID}).Run(ctx2, fantasy.ToolCall{Input: `{"status":"success","summary":"checked","invariant_assessments":[]}`})
	if err != nil || !response.IsError {
		t.Fatalf("invalid claims response=%#v err=%v", response, err)
	}
	if stored := c2.GetTaskResult(item2.ID); stored != nil {
		t.Fatalf("failed attestation published result: %#v", stored)
	}
}

func TestSubmitResultUsesTaskExecutionManifestAfterToolFailure(t *testing.T) {
	definition := InvariantDefinition{ID: "safe", Statement: "preserve safety", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}
	c, item, primary := invariantAttestationFixture(t, InvariantVerificationReport, definition)
	addInvariantRecoveryManifest(t, c, item, primary, "request-recovery")

	ctx := withInvocationMetadata(occurrenceTestContext(c, item.ID, 1), invocationMetadataFromManifest(&primary))
	response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Input: `{"status":"success","summary":"checked","invariant_assessments":[{"invariant_id":"safe","status":"preserved","summary":"preserved"}]}`})
	if err != nil || response.IsError {
		t.Fatalf("submit_result after recovery response=%#v err=%v", response, err)
	}
	stored := c.GetTaskResult(item.ID)
	if stored == nil || stored.InvariantVerification == nil || stored.InvariantVerification.ContextManifestFingerprint != primary.Fingerprint {
		t.Fatalf("stored result = %#v, want primary manifest %q", stored, primary.Fingerprint)
	}
}

func TestInvariantRepairInstructionsReuseDurableManifest(t *testing.T) {
	definition := InvariantDefinition{ID: "safe", Statement: "preserve safety", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}
	c, item, manifest := invariantAttestationFixture(t, InvariantVerificationGate, definition)
	item.WorksetBinding = &WorksetBinding{TouchedPaths: []string{"internal/team/invariant_attestation.go"}}
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	prompt, metadata, err := c.invariantRepairInstructions(item.ID, 1, manifest.ModelExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "### safe (error)") || !strings.Contains(prompt, "preserve safety") || metadata.ModelExecutionID != manifest.ModelExecutionID {
		t.Fatalf("repair prompt=%q metadata=%#v", prompt, metadata)
	}
	if !slices.Equal(metadata.TouchedPaths, item.WorksetBinding.TouchedPaths) {
		t.Fatalf("repair touched paths = %#v, want %#v", metadata.TouchedPaths, item.WorksetBinding.TouchedPaths)
	}
	metadata.TouchedPaths[0] = "mutated"
	if item.WorksetBinding.TouchedPaths[0] == "mutated" {
		t.Fatal("repair metadata aliases the durable workset binding")
	}
	c.session.InvariantCatalog[0].Statement = "changed"
	if _, _, err := c.invariantRepairInstructions(item.ID, 1, manifest.ModelExecutionID); err == nil {
		t.Fatal("repair accepted catalog content that no longer matches persisted manifest")
	}
}

func TestInvariantRepairUsesUniquePrimaryAmongRecoveryManifests(t *testing.T) {
	definition := InvariantDefinition{ID: "safe", Statement: "preserve safety", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}
	c, item, primary := invariantAttestationFixture(t, InvariantVerificationGate, definition)
	addInvariantRecoveryManifest(t, c, item, primary, "request-recovery-1")
	addInvariantRecoveryManifest(t, c, item, primary, "request-recovery-2")

	current := c.todoItemByID(item.ID)
	metadata, err := c.invariantRepairIdentity(current, 1)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ParentManifestFingerprint != primary.Fingerprint || metadata.ModelExecutionID != primary.ModelExecutionID || metadata.Purpose != primary.Purpose {
		t.Fatalf("repair metadata = %#v, want primary manifest %#v", metadata, primary)
	}
	prompt, promptMetadata, err := c.invariantRepairInstructions(item.ID, 1, primary.ModelExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "### safe (error)") || promptMetadata.ParentManifestFingerprint != primary.Fingerprint {
		t.Fatalf("repair prompt=%q metadata=%#v", prompt, promptMetadata)
	}
}

func TestInvariantRepairIdentityUsesTaskExecutionManifest(t *testing.T) {
	c, item, primary := invariantAttestationFixture(t, InvariantVerificationReport)
	metadata, err := c.invariantRepairIdentity(item, 1)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ParentManifestFingerprint != primary.Fingerprint || metadata.Purpose != "task_execution" || metadata.Trigger != ContextTriggerTaskDispatch {
		t.Fatalf("repair metadata = %#v, want task execution manifest %#v", metadata, primary)
	}
}

func TestInvariantRepairIdentityUsesRetryManifest(t *testing.T) {
	c, item, retry := invariantAttestationFixture(t, InvariantVerificationReport)
	retry.Attempt = 2
	retry.Trigger = ContextTriggerRetry
	retry.Purpose = "task_retry"
	retry.Fingerprint = contextManifestFingerprint(retry)
	if err := c.taskTracker.TodoList().SetContextManifest(item.ID, &retry); err != nil {
		t.Fatal(err)
	}
	metadata, err := c.invariantRepairIdentity(c.todoItemByID(item.ID), 2)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ParentManifestFingerprint != retry.Fingerprint || metadata.Purpose != "task_retry" || metadata.Trigger != ContextTriggerRetry {
		t.Fatalf("repair metadata = %#v, want retry manifest %#v", metadata, retry)
	}
}

func TestInvariantManifestSelectionFailsClosedWithoutUniquePrimary(t *testing.T) {
	c, item, primary := invariantAttestationFixture(t, InvariantVerificationReport)
	duplicate := *cloneContextInjectionManifest(&primary)
	duplicate.RequestID = "request-primary-duplicate"
	duplicate.Fingerprint = contextManifestFingerprint(duplicate)
	if err := c.taskTracker.TodoList().SetContextManifest(item.ID, &duplicate); err != nil {
		t.Fatal(err)
	}
	if _, err := c.invariantContextManifest(c.todoItemByID(item.ID), 1, primary.ModelExecutionID); err == nil || !strings.Contains(err.Error(), "multiple authoritative") {
		t.Fatalf("duplicate primary error = %v", err)
	}

	c2, item2, primary2 := invariantAttestationFixture(t, InvariantVerificationReport)
	recovery := addInvariantRecoveryManifest(t, c2, item2, primary2, "request-recovery-only")
	withoutPrimary := c2.todoItemByID(item2.ID)
	withoutPrimary.ContextManifests = []ContextInjectionManifest{recovery}
	if _, err := authoritativeTaskContextManifest(withoutPrimary, 1, primary2.ModelExecutionID); err == nil || !strings.Contains(err.Error(), "no context manifest") {
		t.Fatalf("recovery-only error = %v", err)
	}
}

func TestInvariantManifestSelectionSurvivesSessionReload(t *testing.T) {
	c, item, primary := invariantAttestationFixture(t, InvariantVerificationGate)
	item.WorksetBinding = &WorksetBinding{TouchedPaths: []string{"docs/review.md"}}
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	addInvariantRecoveryManifest(t, c, item, primary, "request-recovery")
	workspace := t.TempDir()
	if err := SaveSession(workspace, &SessionData{Tasks: c.taskTracker.TodoList().Items()}); err != nil {
		t.Fatal(err)
	}
	loaded := LoadSession(workspace)
	if loaded == nil {
		t.Fatal("session did not reload")
	}
	resumedTracker := NewTaskTracker()
	resumedTracker.TodoList().Restore(loaded.Tasks)
	resumed := &Coordinator{projectDir: "/repo", taskTracker: resumedTracker, session: c.session}
	resumedItem := resumed.todoItemByID(item.ID)
	metadata, err := resumed.invariantRepairIdentity(resumedItem, 1)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.ParentManifestFingerprint != primary.Fingerprint {
		t.Fatalf("reloaded repair metadata = %#v, want primary manifest %q", metadata, primary.Fingerprint)
	}
	if !slices.Equal(metadata.TouchedPaths, []string{"docs/review.md"}) {
		t.Fatalf("reloaded repair touched paths = %#v", metadata.TouchedPaths)
	}
}

func TestSubmitResultRejectsUnknownFindingSeverity(t *testing.T) {
	tool := &submitResultTool{}
	response, err := tool.Run(t.Context(), fantasy.ToolCall{Input: `{"status":"success","summary":"done","findings":[{"summary":"bad","severity":"critical"}]}`})
	if err != nil || !response.IsError {
		t.Fatalf("unknown severity response=%#v err=%v", response, err)
	}
	response, err = tool.Run(t.Context(), fantasy.ToolCall{Input: `{"status":"success","summary":"done","findings":[{"summary":"legacy"}]}`})
	if err != nil || response.IsError {
		t.Fatalf("empty compatibility severity response=%#v err=%v", response, err)
	}
}
