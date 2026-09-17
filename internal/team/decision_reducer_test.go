package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	testLogicalRunID = "ldr_01010101010101010101010101010101"
	testDigestA      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDigestB      = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	testDigestC      = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

func TestLogicalDecisionReducerCrashPrefixes(t *testing.T) {
	events := universalDecisionLifecycleEvents(t)
	wantPhases := []LogicalDecisionPhase{
		LogicalDecisionSupporting,
		LogicalDecisionPrepared,
		LogicalDecisionAdmitted,
		LogicalDecisionAdmitted,
		LogicalDecisionAdmitted,
		LogicalDecisionBound,
		LogicalDecisionSupporting,
	}
	for end, want := range wantPhases {
		t.Run(events[end].Type, func(t *testing.T) {
			got, err := ReplayLogicalDecisionRun(events[:end+1], testLogicalRunID, "main")
			if err != nil {
				t.Fatal(err)
			}
			if got.Phase != want {
				t.Fatalf("phase = %s, want %s", got.Phase, want)
			}
			if got.LastAppliedEventID != events[end].ID {
				t.Fatalf("last event = %s, want %s", got.LastAppliedEventID, events[end].ID)
			}
		})
	}
	got, err := ReplayLogicalDecisionRun(events, testLogicalRunID, "main")
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentGeneration != 2 || got.ActivePrimary != nil || got.Usage.TokensUsed != 42 || got.Usage.GenerationsStarted != 1 {
		t.Fatalf("unexpected final projection: %#v", got)
	}
}

func TestLogicalDecisionReducerRejectsStaleOwnerAndPartialMutation(t *testing.T) {
	events := universalDecisionLifecycleEvents(t)
	reducer := NewLogicalDecisionReducer()
	if err := reducer.Apply(events[0]); err != nil {
		t.Fatal(err)
	}
	before := reducer.Snapshot()
	stale := events[1]
	stale.Payload = replaceJSONField(t, stale.Payload, "owner_epoch", uint64(2))
	if err := reducer.Apply(stale); err == nil || !strings.Contains(err.Error(), "owner fencing") {
		t.Fatalf("stale owner error = %v", err)
	}
	after := reducer.Snapshot()
	if before.LastAppliedEventID != after.LastAppliedEventID || before.Phase != after.Phase || before.CurrentGeneration != after.CurrentGeneration {
		t.Fatalf("failed transition mutated projection: before=%#v after=%#v", before, after)
	}
}

func TestLogicalDecisionReducerAttachAdvancesOwnerEpoch(t *testing.T) {
	open := universalDecisionLifecycleEvents(t)[0]
	reducer := NewLogicalDecisionReducer()
	if err := reducer.Apply(open); err != nil {
		t.Fatal(err)
	}
	attachPayload := DecisionRunAttachedPayload{
		decisionEventCommon:    decisionEventCommon{SchemaVersion: 1, LogicalRunID: testLogicalRunID, BranchID: "main", ExecutionRunID: "run-2", OwnerEpoch: 2},
		PreviousExecutionRunID: "run-1", ResumeFromEventID: open.ID,
	}
	attach := universalDecisionEvent(t, EventDecisionRunAttached, "evt-attach", "run-2", decisionAttachedKey(testLogicalRunID, "run-2"), attachPayload)
	if err := reducer.Apply(attach); err != nil {
		t.Fatal(err)
	}
	got := reducer.Snapshot()
	if got.OwnerEpoch != 2 || len(got.ExecutionRunIDs) != 2 || got.ExecutionRunIDs[1] != "run-2" {
		t.Fatalf("unexpected attach projection: %#v", got)
	}
	prepared := universalDecisionLifecycleEvents(t)[1]
	if err := reducer.Apply(prepared); err == nil || !strings.Contains(err.Error(), "owner fencing") {
		t.Fatalf("old owner was not fenced: %v", err)
	}
}

func TestDecisionEventStrictPayloadValidation(t *testing.T) {
	open := universalDecisionLifecycleEvents(t)[0]
	unknown := replaceJSONField(t, open.Payload, "unexpected", true)
	open.Payload = unknown
	if err := ValidateEventPayload(open); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}

	open = universalDecisionLifecycleEvents(t)[0]
	open.Payload = replaceJSONField(t, open.Payload, "schema_version", 2)
	if err := ValidateEventPayload(open); err == nil || !strings.Contains(err.Error(), "unsupported payload schema") {
		t.Fatalf("unknown version error = %v", err)
	}
}

func TestEventStoreDecisionIdempotencyUsesBusinessPayload(t *testing.T) {
	store, err := NewEventStore(t.TempDir(), "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetBranchID("main")
	original := universalDecisionLifecycleEvents(t)[0]
	original.ID, original.Hash, original.Timestamp = "", "", ""
	first, err := store.AppendPersisted(original)
	if err != nil {
		t.Fatal(err)
	}
	retry := original
	retry.RunID = "run-retry"
	retry.Payload = replaceJSONField(t, retry.Payload, "execution_run_id", "run-retry")
	retry.Payload = replaceJSONField(t, retry.Payload, "owner_epoch", uint64(9))
	got, err := store.AppendPersisted(retry)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Fatalf("transport retry created a second event: %s != %s", got.ID, first.ID)
	}
	conflict := original
	conflict.Payload = replaceJSONField(t, conflict.Payload, "requirement_digest", testDigestC)
	if _, err := store.AppendPersisted(conflict); !errors.Is(err, ErrDecisionIdempotencyConflict) {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestDecisionWriterLeaseExcludesConcurrentOwners(t *testing.T) {
	workspace := t.TempDir()
	first, err := AcquireDecisionWriterLease(context.Background(), workspace, "main")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	if _, err := AcquireDecisionWriterLease(ctx, workspace, "main"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lease error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireDecisionWriterLease(context.Background(), workspace, "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}

func universalDecisionLifecycleEvents(t *testing.T) []RunEvent {
	t.Helper()
	ref := DecisionArtifactRef{ID: "artifact-1", SHA256: testDigestA, MediaType: "application/json", SizeBytes: 10}
	taskID, err := PrimaryDecisionTaskID("main", testLogicalRunID)
	if err != nil {
		t.Fatal(err)
	}
	decisionID, err := PrimaryDecisionID("main", testLogicalRunID, 1)
	if err != nil {
		t.Fatal(err)
	}
	common := decisionEventCommon{SchemaVersion: 1, LogicalRunID: testLogicalRunID, BranchID: "main", ExecutionRunID: "run-1", OwnerEpoch: 1}
	generation := decisionGenerationEventCommon{decisionEventCommon: common, Generation: 1}
	open := DecisionRunOpenedPayload{
		decisionEventCommon: common, TeamID: "team-fixture", TeamDefinitionDigest: testDigestB,
		RequirementRef: ref, RequirementDigest: testDigestA, ProfileBundleRef: ref, AuthoritySnapshotRef: ref,
		EffectiveLimits: DecisionEffectiveLimits{TotalDecisionTokens: 1000, ActiveDecisionDurationMS: 1000, MaxGenerations: 3, MaxStageInvocationAttempts: 2, CleanupTimeoutMS: 100},
	}
	prepared := PrimaryDecisionPreparedPayload{
		decisionGenerationEventCommon: generation, TaskID: taskID, DecisionID: decisionID,
		SupportCursor:         DecisionSupportCursor{EventID: "evt-support", EventHash: testDigestA, LineageDigest: testDigestB},
		SupportRevisionDigest: testDigestC, BaseEvidenceRef: ref, RolePlanRef: ref, PreparationDigest: testDigestB,
	}
	admitted := PrimaryDecisionAdmittedPayload{
		decisionGenerationEventCommon: generation, TaskID: taskID, DecisionID: decisionID,
		OccurrenceRef: ref, AdmissionRef: ref, PreparationDigest: testDigestB,
	}
	started := DecisionRoleCallStartedPayload{
		decisionGenerationEventCommon: generation, DecisionID: decisionID, Role: "judge", Ordinal: 1, Round: 1,
		InvocationAttempt: 1, BindingID: "binding-1", InvocationID: "invocation-1", InputDigest: testDigestA, ReservationRef: ref,
	}
	output := ref
	settled := DecisionRoleCallSettledPayload{
		decisionGenerationEventCommon: generation, InvocationID: "invocation-1", Status: "accepted", ReceiptRef: ref,
		OutputRef: &output, UsageState: "observed", TokensUsed: 42,
	}
	binding := PrimaryBindingV1{
		SchemaVersion: 1, LogicalRunID: testLogicalRunID, BranchID: "main", TaskID: taskID, Generation: 1,
		DecisionID: decisionID, RequirementDigest: testDigestA, SupportRevisionDigest: testDigestC,
		AdmissionRef: ref, BaseEvidenceRef: ref, RolePlanRef: ref, RecordRef: ref, SealedEvidenceHash: testDigestB,
	}
	bound := PrimaryDecisionBoundPayload{decisionGenerationEventCommon: generation, Binding: binding, RecordValidationReceiptRef: ref}
	prior := "evt-bound"
	invalidated := PrimaryDecisionInvalidatedPayload{
		decisionGenerationEventCommon: generation, TaskID: taskID, DecisionID: decisionID, PriorBindingEventID: &prior,
		TriggerEventIDs: []string{"evt-support-2"}, ReasonCode: "material-evidence-changed", NextGeneration: 2,
	}
	return []RunEvent{
		universalDecisionEvent(t, EventDecisionRunOpened, "evt-open", "run-1", decisionOpenedKey(testLogicalRunID), open),
		universalDecisionEvent(t, EventPrimaryDecisionPrepared, "evt-prepared", "run-1", decisionPreparedKey(testLogicalRunID, 1, testDigestB), prepared),
		universalDecisionEvent(t, EventPrimaryDecisionAdmitted, "evt-admitted", "run-1", decisionAdmittedKey(testLogicalRunID, 1), admitted),
		universalDecisionEvent(t, EventDecisionRoleCallStarted, "evt-started", "run-1", decisionCallKey(testLogicalRunID, 1, "judge", 1, 1, 1, "started"), started),
		universalDecisionEvent(t, EventDecisionRoleCallSettled, "evt-settled", "run-1", decisionCallKey(testLogicalRunID, 1, "judge", 1, 1, 1, "settled"), settled),
		universalDecisionEvent(t, EventPrimaryDecisionBound, "evt-bound", "run-1", decisionBoundKey(testLogicalRunID, 1), bound),
		universalDecisionEvent(t, EventPrimaryDecisionInvalidated, "evt-invalidated", "run-1", decisionInvalidatedKey(testLogicalRunID, 1), invalidated),
	}
}

func universalDecisionEvent(t *testing.T, eventType EventType, id, runID, key string, payload any) RunEvent {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return RunEvent{
		SchemaVersion: eventStoreSchemaVersion, ID: id, RunID: runID, SessionID: "session-1", BranchID: "main",
		Actor: "coordinator", Type: eventType.String(), Timestamp: "2026-09-17T00:00:00Z", IdempotencyKey: key,
		Payload: raw, Hash: testDigestA,
	}
}

func replaceJSONField(t *testing.T, raw json.RawMessage, field string, value any) json.RawMessage {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object[field] = value
	result, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
