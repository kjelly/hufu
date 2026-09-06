package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/utils"
)

// DecisionFinalizationSchemaVersion versions the small, runtime-owned result
// written between the aggregate stages and decision_finalized.  A result is
// intentionally separate from DecisionRecord so a crash after finalization
// cannot cause a second model call.
const DecisionFinalizationSchemaVersion = 1

const (
	FinalizationIdentityAggregate   = "aggregate"
	FinalizationIdentityCoordinator = "coordinator"
	FinalizationOutcomeSelected     = "selected"
	FinalizationOutcomeOverride     = "override"
	FinalizationOutcomeLegacy       = "legacy"
	FinalizationResultMediaType     = "application/vnd.hufu.decision-finalization+json;v=1"
	FinalizationResultKind          = "decision_finalization_result"
	FinalizationResultRole          = "decision_finalization"
)

// FinalizationWireResult is the complete model-facing protocol. Runtime
// identity, mode, evidence and lifecycle fields are deliberately absent.
// Unknown JSON fields are rejected by decodeFinalizationWireResult.
type FinalizationWireResult struct {
	OptionID string `json:"option_id"`
	Reason   string `json:"reason,omitempty"`
}

// DecisionFinalizationResult is the canonical runtime-owned result. It is
// safe to persist because all fields except OptionID and Reason are assigned
// by the runtime after validating the sealed packet.
type DecisionFinalizationResult struct {
	SchemaVersion int      `json:"schema_version"`
	DecisionID    string   `json:"decision_id"`
	EvidenceHash  string   `json:"evidence_hash"`
	Mode          string   `json:"mode"`
	Identity      string   `json:"identity"`
	OptionID      string   `json:"option_id"`
	Reason        string   `json:"reason,omitempty"`
	Stale         bool     `json:"stale,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
	Outcome       string   `json:"outcome"`
}

// FinalizationResult is retained as the concise public name used by callers
// of the decision engine.
type FinalizationResult = DecisionFinalizationResult

// CoordinatorFinalizationRequest and JudgeFinalizationRequest are distinct by
// construction. Neither request contains opinions, conversation history, or
// a model-supplied identity.
type CoordinatorFinalizationRequest struct {
	DecisionID string
	Packet     DecisionEvidencePacket
	Aggregates []DecisionAggregate
	Challenges []DecisionChallenge
	Revisions  []DecisionRevision
}

type JudgeFinalizationRequest struct {
	DecisionID string
	JudgeID    string
	Packet     DecisionEvidencePacket
	Aggregates []DecisionAggregate
	Challenges []DecisionChallenge
	Revisions  []DecisionRevision
}

type CoordinatorFinalizer interface {
	RunCoordinatorFinalization(context.Context, CoordinatorFinalizationRequest) (FinalizationWireResult, error)
}

type NamedJudgeFinalizer interface {
	RunJudgeFinalization(context.Context, JudgeFinalizationRequest) (FinalizationWireResult, error)
}

// FinalizerRunner is a convenience interface for production runners that can
// provide both finalization modes. The engine uses the narrower interfaces so
// tests and alternate transports cannot accidentally receive the wrong mode.
type FinalizerRunner interface {
	CoordinatorFinalizer
	NamedJudgeFinalizer
}

func (r DecisionFinalizationResult) Validate(packet DecisionEvidencePacket, policy DecisionPolicy) error {
	if r.SchemaVersion != DecisionFinalizationSchemaVersion {
		return fmt.Errorf("finalization result has unsupported schema version %d", r.SchemaVersion)
	}
	if !packet.Sealed || strings.TrimSpace(packet.Hash) == "" {
		return fmt.Errorf("%s: finalization requires sealed evidence", ReasonDecisionEvidenceNotSealed)
	}
	if strings.TrimSpace(r.DecisionID) == "" || strings.TrimSpace(r.EvidenceHash) == "" || r.EvidenceHash != packet.Hash {
		return fmt.Errorf("finalization result evidence or decision identity is invalid")
	}
	if r.Mode != agent.FinalizationAggregate && r.Mode != agent.FinalizationCoordinator && r.Mode != agent.FinalizationJudge {
		return fmt.Errorf("finalization result mode %q is unsupported", r.Mode)
	}
	if r.Mode != policy.EffectiveFinalization() {
		return fmt.Errorf("finalization result mode %q does not match admitted mode %q", r.Mode, policy.EffectiveFinalization())
	}
	wantIdentity := FinalizationIdentityAggregate
	switch r.Mode {
	case agent.FinalizationCoordinator:
		wantIdentity = FinalizationIdentityCoordinator
	case agent.FinalizationJudge:
		wantIdentity = strings.TrimSpace(policy.Finalization.JudgeID)
	}
	if strings.TrimSpace(wantIdentity) == "" || r.Identity != wantIdentity {
		return fmt.Errorf("finalization result identity %q does not match mode %q", r.Identity, r.Mode)
	}
	if strings.TrimSpace(r.OptionID) == "" || !packet.HasOption(r.OptionID) {
		return fmt.Errorf("finalization result chose option %q outside sealed options", r.OptionID)
	}
	if r.Outcome != FinalizationOutcomeSelected && r.Outcome != FinalizationOutcomeOverride {
		return fmt.Errorf("finalization result outcome %q is unsupported", r.Outcome)
	}
	return nil
}

// ValidateFinalizationResult validates a result against the actual aggregate
// and policy, including the mandatory reason iff a finalizer diverges.
func ValidateFinalizationResult(result DecisionFinalizationResult, packet DecisionEvidencePacket, aggregate DecisionAggregate, policy DecisionPolicy) error {
	if err := result.Validate(packet, policy); err != nil {
		return err
	}
	if strings.TrimSpace(aggregate.PreferredOption) == "" || !packet.HasOption(aggregate.PreferredOption) {
		return fmt.Errorf("aggregate preferred option is not sealed")
	}
	diverged := result.OptionID != aggregate.PreferredOption
	if diverged && strings.TrimSpace(result.Reason) == "" {
		return fmt.Errorf("finalization result diverges from aggregate without a mandatory override reason")
	}
	if !diverged && strings.TrimSpace(result.Reason) != "" {
		return fmt.Errorf("finalization result supplied an override reason without diverging from aggregate")
	}
	wantOutcome := FinalizationOutcomeSelected
	if diverged {
		wantOutcome = FinalizationOutcomeOverride
	}
	if result.Outcome != wantOutcome {
		return fmt.Errorf("finalization result outcome %q does not match selected option", result.Outcome)
	}
	if result.Mode == agent.FinalizationAggregate && diverged {
		return fmt.Errorf("aggregate finalization cannot diverge from its preferred option")
	}
	return nil
}

// LegacyFinalizationResult derives a display-only result for schema-v1
// DecisionRecords written before the durable result event existed.
func LegacyFinalizationResult(record DecisionRecord) (DecisionFinalizationResult, bool) {
	if strings.TrimSpace(record.FinalOption) == "" || strings.TrimSpace(record.FinalizationMode) == "" {
		return DecisionFinalizationResult{}, false
	}
	identity := record.FinalizationIdentity
	if identity == "" {
		switch record.FinalizationMode {
		case agent.FinalizationAggregate:
			identity = FinalizationIdentityAggregate
		case agent.FinalizationCoordinator:
			identity = FinalizationIdentityCoordinator
		default:
			identity = "legacy"
		}
	}
	return DecisionFinalizationResult{
		SchemaVersion: DecisionFinalizationSchemaVersion,
		DecisionID:    record.ID, EvidenceHash: record.EvidenceHash,
		Mode: record.FinalizationMode, Identity: identity,
		OptionID: record.FinalOption, Outcome: FinalizationOutcomeLegacy,
		Stale:    record.Stale,
		Warnings: []string{"legacy decision record has no durable finalization-result event"},
	}, true
}

func cloneFinalizationRequest[T any](value T) (T, error) {
	var clone T
	data, err := json.Marshal(value)
	if err != nil {
		return clone, err
	}
	if err := json.Unmarshal(data, &clone); err != nil {
		return clone, err
	}
	return clone, nil
}

func cloneFinalizationSlices(aggregates []DecisionAggregate, challenges []DecisionChallenge, revisions []DecisionRevision) ([]DecisionAggregate, []DecisionChallenge, []DecisionRevision) {
	a := append([]DecisionAggregate(nil), aggregates...)
	c := append([]DecisionChallenge(nil), challenges...)
	r := append([]DecisionRevision(nil), revisions...)
	sort.Slice(a, func(i, j int) bool { return a[i].Round < a[j].Round })
	sort.Slice(c, func(i, j int) bool { return c[i].ID < c[j].ID })
	sort.Slice(r, func(i, j int) bool { return r[i].JudgeID < r[j].JudgeID })
	return a, c, r
}

func finalizationPrompt(kind string, decisionID string, packet DecisionEvidencePacket, aggregates []DecisionAggregate, challenges []DecisionChallenge, revisions []DecisionRevision, judgeID string) (string, error) {
	if !packet.Sealed || packet.Hash == "" {
		return "", fmt.Errorf("%s: finalization request requires sealed evidence", ReasonDecisionEvidenceNotSealed)
	}
	a, c, r := cloneFinalizationSlices(aggregates, challenges, revisions)
	input := struct {
		DecisionID string
		Packet     DecisionEvidencePacket `json:"sealed_evidence"`
		Aggregates []DecisionAggregate    `json:"aggregates"`
		Challenges []DecisionChallenge    `json:"challenges,omitempty"`
		Revisions  []DecisionRevision     `json:"revisions,omitempty"`
		JudgeID    string                 `json:"judge_id,omitempty"`
	}{decisionID, packet, a, c, r, judgeID}
	data, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode %s finalization request: %w", kind, err)
	}
	return fmt.Sprintf("You are the %s finalizer. Select exactly one option from sealed evidence. Return exactly one JSON object with only option_id and reason. A reason is required only when diverging from the last aggregate preferred option. Do not include identity, evidence, scores, or conversation history. Request:\n%s", kind, data), nil
}

func decodeFinalizationWireResult(response string) (FinalizationWireResult, error) {
	payload := extractJSONPayload(response)
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	var result FinalizationWireResult
	if err := decoder.Decode(&result); err != nil {
		return FinalizationWireResult{}, fmt.Errorf("finalization response was not the required option_id/reason object: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return FinalizationWireResult{}, fmt.Errorf("finalization response contains trailing JSON")
	} else if err != io.EOF {
		return FinalizationWireResult{}, fmt.Errorf("finalization response has invalid trailing data: %w", err)
	}
	if strings.TrimSpace(result.OptionID) == "" {
		return FinalizationWireResult{}, fmt.Errorf("finalization response requires option_id")
	}
	return result, nil
}

func persistDecisionFinalizationResult(ctx context.Context, store ArtifactStore, result DecisionFinalizationResult, runID, taskID string, attempt int) (ArtifactRef, error) {
	if store == nil {
		return ArtifactRef{}, fmt.Errorf("finalization result requires an artifact store")
	}
	result = redactedFinalizationResult(result)
	data, err := json.Marshal(result)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("encode finalization result: %w", err)
	}
	put, err := store.Put(ctx, PutArtifactRequest{
		Kind: FinalizationResultKind, Role: FinalizationResultRole,
		Path:        "decisions/finalization/" + result.DecisionID + ".json",
		Description: "decision finalization result " + result.DecisionID,
		MediaType:   FinalizationResultMediaType, Content: data,
		RunID: runID, TaskID: taskID, Attempt: attempt,
	})
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("persist finalization result: %w", err)
	}
	ref, err := store.Resolve(ctx, put.ArtifactRef)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("resolve finalization result: %w", err)
	}
	return ref, nil
}

func redactedFinalizationResult(result DecisionFinalizationResult) DecisionFinalizationResult {
	result.Reason = utils.RedactSecrets(result.Reason)
	for i := range result.Warnings {
		result.Warnings[i] = utils.RedactSecrets(result.Warnings[i])
	}
	return result
}

func validatePersistedFinalizationResult(ctx context.Context, store ArtifactStore, ref ArtifactRef, want DecisionFinalizationResult) error {
	_, data, err := readValidatedFinalizationArtifact(ctx, store, ref, FinalizationResultKind, FinalizationResultRole, FinalizationResultMediaType)
	if err != nil {
		return err
	}
	var stored DecisionFinalizationResult
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode finalization result: %w", err)
	}
	if !reflect.DeepEqual(stored, want) {
		return fmt.Errorf("finalization result artifact does not match finalization event")
	}
	return nil
}

// readValidatedFinalizationArtifact is the shared artifact integrity boundary
// for persisted finalized state. Resolve checks the immutable metadata and
// Verify checks the store's bytes; the local digest/size check below keeps this
// contract true for alternate ArtifactStore implementations as well.
func readValidatedFinalizationArtifact(ctx context.Context, store ArtifactStore, ref ArtifactRef, kind, role, mediaType string) (ArtifactRef, []byte, error) {
	if store == nil || !finalizationResultRefValid(ref) {
		return ArtifactRef{}, nil, fmt.Errorf("finalization artifact reference is unavailable")
	}
	resolved, err := store.Resolve(ctx, ref)
	if err != nil {
		return ArtifactRef{}, nil, fmt.Errorf("resolve finalization artifact: %w", err)
	}
	if !sameArtifactRef(ref, resolved) {
		return ArtifactRef{}, nil, fmt.Errorf("finalization artifact reference changed during resolution")
	}
	if resolved.Kind != kind || resolved.Role != role || resolved.MediaType != mediaType || resolved.Bytes < 0 || resolved.ByteSize < 0 || resolved.Bytes != resolved.ByteSize {
		return ArtifactRef{}, nil, fmt.Errorf("finalization artifact metadata is invalid")
	}
	if err := store.Verify(ctx, resolved); err != nil {
		return ArtifactRef{}, nil, fmt.Errorf("verify finalization artifact: %w", err)
	}
	reader, err := store.Open(ctx, resolved.ID)
	if err != nil {
		return ArtifactRef{}, nil, fmt.Errorf("open finalization artifact: %w", err)
	}
	defer func() { _ = reader.Close() }()
	data, err := io.ReadAll(reader)
	if err != nil {
		return ArtifactRef{}, nil, fmt.Errorf("read finalization artifact: %w", err)
	}
	digest := sha256.Sum256(data)
	if hex.EncodeToString(digest[:]) != resolved.SHA256 || int64(len(data)) != resolved.Bytes || int64(len(data)) != resolved.ByteSize {
		return ArtifactRef{}, nil, fmt.Errorf("finalization artifact bytes do not match immutable metadata")
	}
	return resolved, data, nil
}

func validatePersistedDecisionRecord(ctx context.Context, store ArtifactStore, ref ArtifactRef, want DecisionRecord) error {
	resolved, data, err := readValidatedFinalizationArtifact(ctx, store, ref, "decision_record", "decision", "application/json")
	if err != nil {
		return fmt.Errorf("decision record artifact: %w", err)
	}
	var stored DecisionRecord
	if err := json.Unmarshal(data, &stored); err != nil {
		return fmt.Errorf("decode decision record artifact %s: %w", resolved.ID, err)
	}
	if err := validatePersistedDecisionRecordSchema(stored); err != nil {
		return err
	}
	if !reflect.DeepEqual(stored, want) {
		return fmt.Errorf("decision record artifact does not match finalized event")
	}
	return nil
}

func validatePersistedDecisionRecordSchema(record DecisionRecord) error {
	switch record.SchemaVersion {
	case 1, DecisionRecordSchemaVersion:
		return nil
	default:
		return fmt.Errorf("decision record %s has unsupported schema version %d", record.ID, record.SchemaVersion)
	}
}

// validateFinalizedDecisionState is the one canonical finalized-state
// validator. All authoritative v2 reads use it after projection; no caller may
// accept a v2 record by trusting only the event payload or index row.
func validateFinalizedDecisionState(ctx context.Context, store ArtifactStore, state decisionState) error {
	if state.Record == nil {
		return nil
	}
	switch state.Record.SchemaVersion {
	case 1:
		return nil
	case DecisionRecordSchemaVersion:
		if state.CanonicalRecord == nil {
			return fmt.Errorf("v2 finalized decision has no canonical finalized record")
		}
		if state.Finalization == nil {
			return fmt.Errorf("v2 finalized decision has no canonical finalization result event")
		}
		if state.CanonicalRecord.ID != state.DecisionID || state.CanonicalRecord.EvidenceHash != state.Finalization.EvidenceHash {
			return fmt.Errorf("v2 finalized decision identity does not match canonical finalization result")
		}
		if !finalizationResultRefValid(state.FinalizationResultRef) || !sameArtifactRef(derefArtifact(state.CanonicalRecord.FinalizationResultRef), state.FinalizationResultRef) {
			return fmt.Errorf("v2 finalized decision finalization-result reference is not canonically bound")
		}
		if !finalizationResultRefValid(state.FinalizedRecordRef) {
			return fmt.Errorf("v2 finalized decision record reference is unavailable")
		}
		packet := state.Packet
		if !packet.Sealed || packet.Hash != state.CanonicalRecord.EvidenceHash {
			return fmt.Errorf("v2 finalized decision evidence is not canonically bound")
		}
		if err := ValidateFinalizationResult(*state.Finalization, packet, aggregateForFinalization(&state), effectiveDecisionPolicyForState(&state)); err != nil {
			return fmt.Errorf("v2 finalized decision result: %w", err)
		}
		if err := validatePersistedFinalizationResult(ctx, store, state.FinalizationResultRef, *state.Finalization); err != nil {
			return fmt.Errorf("v2 finalized decision result artifact: %w", err)
		}
		if err := validatePersistedDecisionRecord(ctx, store, state.FinalizedRecordRef, *state.CanonicalRecord); err != nil {
			return fmt.Errorf("v2 finalized decision record artifact: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("finalized decision has unsupported record schema version %d", state.Record.SchemaVersion)
	}
}

func finalizationResultEvent(req DecisionRequest, packet DecisionEvidencePacket, result DecisionFinalizationResult, ref ArtifactRef) decisionEvent {
	event := decisionEventFor(req, "finalization_result", packet.Hash)
	event.EvidenceHash = packet.Hash
	event.Finalization = &result
	event.FinalizationResultRef = ref
	return event
}

func finalizationResultEventKey(req DecisionRequest, packet DecisionEvidencePacket) string {
	return decisionStageEventKey(req.DecisionID, "finalization_result", packet.Hash, req.Policy.EffectiveFinalization())
}

func finalizationResultRefValid(ref ArtifactRef) bool {
	return strings.TrimSpace(ref.ID) != "" && strings.TrimSpace(ref.SHA256) != ""
}
