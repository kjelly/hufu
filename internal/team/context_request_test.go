package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func validTestContextRequest() ContextRequest {
	r := ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run-1", TaskID: "task-1", Attempt: 1, Goal: "repair ssh", AgentName: "operator-1", AgentRole: "operator", ModelExecutionID: "model-execution-1", Phase: PhaseAudit, Trigger: ContextTriggerTaskDispatch, Capabilities: []string{"SSH", "audit"}}
	r.AssignRequestID()
	return r
}

func TestContextRequestTriggerValidationMatrix(t *testing.T) {
	base := validTestContextRequest()
	cases := []struct {
		name    string
		request ContextRequest
		valid   bool
	}{
		{"coordinator_start", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", Attempt: 1, Goal: "goal", AgentRole: "coordinator", Phase: PhasePrepare, Trigger: ContextTriggerCoordinatorStart}, true},
		{"continuation", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", Attempt: 1, Goal: "goal", AgentRole: "coordinator", Phase: PhaseExecute, Trigger: ContextTriggerContinuation}, true},
		{"dispatch", base, true},
		{"retry", func() ContextRequest {
			r := base
			r.Attempt = 2
			r.Trigger = ContextTriggerRetry
			r.Failure = &ContextFailure{ErrorClass: "timeout"}
			return r
		}(), true},
		{"tool_failure", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", TaskID: "task", Attempt: 1, Phase: PhaseExecute, Trigger: ContextTriggerToolFailure, Failure: &ContextFailure{ToolName: "bash", ErrorClass: "exit", ToolInputHash: "hash"}}, true},
		{"skill_match", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", Attempt: 1, Goal: "goal", AgentRole: "worker", Phase: PhasePrepare, Trigger: ContextTriggerSkillMatch}, true},
		{"guard_review", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", Attempt: 1, AgentName: "worker", AgentRole: "auxiliary", Goal: "review", Phase: PhaseExecute, Purpose: "guard_reviewer", Trigger: ContextTriggerGuardReview, Failure: &ContextFailure{ToolName: "bash", ToolInputHash: "hash"}}, true},
		{"plan_review", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", TaskID: "task", Attempt: 1, Goal: "review", AgentRole: "auxiliary", Phase: PhasePrepare, ActionType: "plan:1", VerificationCriteria: "criteria", Trigger: ContextTriggerPlanReview}, true},
		{"judge", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", TaskID: "task", Attempt: 1, Goal: "judge", AgentRole: "auxiliary", Phase: PhaseVerify, CandidateIDs: []string{"a"}, SelectionContract: "choose", Trigger: ContextTriggerJudge}, true},
		{"skeptic", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", TaskID: "task", Attempt: 1, Goal: "skeptic", AgentRole: "auxiliary", Phase: PhaseVerify, CandidateIDs: []string{"a"}, VerificationCriteria: "criteria", Trigger: ContextTriggerSkeptic}, true},
		{"repair", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", TaskID: "task", Attempt: 1, Goal: "repair", AgentRole: "auxiliary", Phase: PhaseExecute, RecoveryDisposition: "retry", Trigger: ContextTriggerRepair, Failure: &ContextFailure{EvidenceRefs: []string{"receipt:1"}}}, true},
		{"invalid_guard_missing_hash", ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: "run", Attempt: 1, AgentName: "worker", AgentRole: "auxiliary", Goal: "review", Phase: PhaseExecute, Purpose: "guard", Trigger: ContextTriggerGuardReview, Failure: &ContextFailure{ToolName: "bash"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.request.AssignRequestID()
			err := tc.request.Validate()
			if tc.valid && err != nil {
				t.Fatalf("Validate() = %v", err)
			}
			if !tc.valid && err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestContextRequestFingerprintSeparatesModelExecutions(t *testing.T) {
	a := validTestContextRequest()
	b := a
	b.ModelExecutionID = "model-execution-2"
	b.AssignRequestID()
	if a.Fingerprint() == b.Fingerprint() || a.RequestID == b.RequestID {
		t.Fatal("model execution identity did not separate request identities")
	}
}

func TestContextRequestFingerprintSeparatesParentInvocations(t *testing.T) {
	request := validTestContextRequest()
	request.ParentTrigger = ContextTriggerRetry
	request.ParentRequestID = "ctx-parent-a"
	request.ParentManifestFingerprint = "manifest-parent-a"
	request.AssignRequestID()
	other := request
	other.ParentManifestFingerprint = "manifest-parent-b"
	other.AssignRequestID()
	if request.Fingerprint() == other.Fingerprint() || request.RequestID == other.RequestID {
		t.Fatalf("parent invocation identity did not separate child requests: %#v / %#v", request, other)
	}
}

func TestContextRequestJSONNeverPersistsFailureEvidence(t *testing.T) {
	r := validTestContextRequest()
	r.Failure = &ContextFailure{ErrorClass: "tool_error", EvidenceRefs: []string{"raw transcript api_key=secret-value"}, ToolInputHash: "opaque-hash"}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "raw transcript") || strings.Contains(string(data), "secret-value") {
		t.Fatalf("request JSON leaked failure evidence: %s", data)
	}
}

func TestTaskContextRequestUsesIsolatedModelExecutionIdentity(t *testing.T) {
	c := &Coordinator{modelExecutionID: "extra-model-slot-2"}
	r := c.newTaskContextRequest(TaskDef{Goal: "task", Model: "same-model"}, "task-1", 1, ContextTriggerTaskDispatch, "worker", "worker", nil)
	if r.ModelExecutionID != "extra-model-slot-2" {
		t.Fatalf("model execution identity = %q", r.ModelExecutionID)
	}
	if r.Purpose != "task_execution" {
		t.Fatalf("dispatch purpose = %q", r.Purpose)
	}
	retry := c.newTaskContextRequest(TaskDef{Goal: "task", Model: "same-model"}, "task-1", 2, ContextTriggerRetry, "worker", "worker", &ContextFailure{ErrorClass: "timeout"})
	if retry.Purpose != "task_retry" {
		t.Fatalf("retry purpose = %q", retry.Purpose)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorContextRequestUsesExplicitPurpose(t *testing.T) {
	c := &Coordinator{executionRunID: "run-1"}
	start := c.newCoordinatorContextRequest("coordinate", false, 1)
	continuation := c.newCoordinatorContextRequest("continue", true, 2)
	if start.Purpose != "coordinator_start" || continuation.Purpose != "coordinator_continuation" {
		t.Fatalf("coordinator purposes = %q / %q", start.Purpose, continuation.Purpose)
	}
}

func TestContextRequestValidateQueryAndFingerprint(t *testing.T) {
	r := validTestContextRequest()
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	query := r.RetrievalQuery()
	for _, want := range []string{"repair ssh", "phase:audit", "trigger:task_dispatch", "role:operator", "capability:ssh"} {
		if !strings.Contains(query, want) {
			t.Fatalf("query %q missing %q", query, want)
		}
	}
	if r.Fingerprint() != r.Fingerprint() {
		t.Fatal("fingerprint is not deterministic")
	}
	retry := r
	retry.Attempt = 2
	retry.Trigger = ContextTriggerRetry
	retry.Failure = &ContextFailure{ErrorClass: "ssh_timeout", ToolName: "ssh", EvidenceRefs: []string{"api_token=super-secret-token"}}
	retry.AssignRequestID()
	if err := retry.Validate(); err != nil {
		t.Fatal(err)
	}
	if retry.Fingerprint() == r.Fingerprint() {
		t.Fatal("retry fingerprint must differ from dispatch")
	}
	if strings.Contains(retry.RetrievalQuery(), "super-secret-token") {
		t.Fatal("retrieval query leaked secret evidence")
	}
}

func TestContextRequestRejectsInvalidAttemptAndMissingRetryFailure(t *testing.T) {
	r := validTestContextRequest()
	r.Attempt = 0
	if err := r.Validate(); err == nil {
		t.Fatal("zero attempt accepted")
	}
	r = validTestContextRequest()
	r.Trigger = ContextTriggerRetry
	if err := r.Validate(); err == nil {
		t.Fatal("retry without failure accepted")
	}
	r = validTestContextRequest()
	r.RequestID = "api_key=raw-secret"
	if err := r.Validate(); err == nil {
		t.Fatal("noncanonical request ID accepted")
	}
}
