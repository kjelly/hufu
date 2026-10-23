package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

const EventDecisionArtifactObserved EventType = "decision_artifact_observed"

// Artifact evidence keys share the decision observation journal and validation.
// One read produces one observation, including when it also feeds a decision.
const EventArtifactEvidenceObserved EventType = "artifact_evidence_observed"

type decisionArtifactObservation struct {
	EvidenceKeys     map[string]string `json:"evidence_keys,omitempty"`
	ModelExecutionID string            `json:"model_execution_id,omitempty"`
	Version          int               `json:"version"`
	Scope            string            `json:"scope"`
	SchemaHash       string            `json:"schema_hash"`
	CallID           string            `json:"call_id"`
	Input            string            `json:"input"`
	Sources          []ArtifactRef     `json:"sources"`
	Output           ArtifactRef       `json:"output"`
	ErrorCode        string            `json:"error_code,omitempty"`
}

func decisionBytesHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func decisionObjectHash(value any) (string, error) {
	raw, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return decisionBytesHash(raw), nil
}

func validateDecisionObservationEvent(event RunEvent) error {
	var p decisionArtifactObservation
	if err := decodeStrictDecisionPayload(event.Payload, &p); err != nil {
		return err
	}
	if p.Version != 1 || !decisionDigestPattern.MatchString(p.Scope) || !decisionDigestPattern.MatchString(p.SchemaHash) || p.CallID == "" || event.TaskID == "" || event.RunID == "" || event.Actor == "" || event.BranchID == "" || event.Attempt < 1 {
		return fmt.Errorf("invalid decision observation identity")
	}
	if p.ErrorCode != "" {
		if p.ErrorCode != "decision_context_too_large" {
			return fmt.Errorf("invalid decision observation failure")
		}
		return nil
	}
	if len(p.Sources) == 0 || len(p.Sources) > 32 || p.Output.ID == "" || !decisionDigestPattern.MatchString(p.Output.SHA256) || len(p.Input) > 16<<10 {
		return fmt.Errorf("invalid decision observation evidence")
	}
	if event.Type == string(EventArtifactEvidenceObserved) {
		bindings := make(map[string]ArtifactRef)
		for _, source := range p.Sources {
			key := artifactEvidenceKey(event, p, source)
			if source.ID == "" || !decisionDigestPattern.MatchString(source.SHA256) || p.EvidenceKeys[key] != source.ID {
				return fmt.Errorf("invalid evidence key projection")
			}
			if err := rememberArtifactEvidenceKey(bindings, key, source); err != nil {
				return err
			}
		}
		if len(p.EvidenceKeys) != len(bindings) {
			return fmt.Errorf("invalid evidence key projection coverage")
		}
	}
	return nil
}

func (c *Coordinator) putDecisionArtifact(ctx context.Context, metadata InvocationMetadata, kind string, raw []byte) (ArtifactRef, error) {
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		return ArtifactRef{}, err
	}
	// Persist already redacted bytes exactly once; any rewriting changes the binding.
	if !utf8.Valid(raw) || utils.RedactSecrets(string(raw)) != string(raw) {
		return ArtifactRef{}, fmt.Errorf("decision_context_redaction_unstable")
	}
	result, err := store.Put(ctx, PutArtifactRequest{Kind: kind, Path: kind, MediaType: "application/json", Content: raw, RunID: metadata.RunID, TaskID: metadata.TaskID, Agent: metadata.AgentName, Attempt: metadata.Attempt})
	return result.ArtifactRef, err
}

func (c *Coordinator) readDecisionArtifact(ctx context.Context, ref ArtifactRef, limit int) ([]byte, error) {
	if ref.ByteSize < 0 || ref.ByteSize > int64(limit) {
		return nil, fmt.Errorf("decision_context_too_large")
	}
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		return nil, err
	}
	if err := store.Verify(ctx, ref); err != nil {
		return nil, err
	}
	reader, err := store.Open(ctx, ref.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	raw, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit || decisionBytesHash(raw) != ref.SHA256 || !utf8.Valid(raw) {
		return nil, fmt.Errorf("decision_context_integrity_invalid")
	}
	return raw, nil
}

// Called only after the central gate and the authentic built-in view succeeded.
// Nothing is recovered from worker-writable transcript files.
func (c *Coordinator) captureDecisionArtifactRead(ctx context.Context, inner fantasy.AgentTool, call fantasy.ToolCall, response *fantasy.ToolResponse) error {
	if c == nil || response == nil || response.IsError || inner.Info().Name != "view" || !tools.IsTrustedArtifactPathTool(inner) {
		return nil
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	if !ok || metadata.TaskID == "" {
		return nil
	}
	_, compiled, err := c.boundResultContract(metadata.TaskID)
	if err != nil {
		return err
	}
	if !hasResultDecisions(compiled) && (compiled == nil || compiled.evidenceKeys == nil) {
		return nil
	}
	if !c.hasDurableEventJournal() {
		return fmt.Errorf("decision_journal_unavailable")
	}
	if hasResultDecisions(compiled) {
		// Parallel view calls must reserve capacity against the same durable
		// observation prefix before either response reaches the worker.
		c.decisionObservationMu.Lock()
		defer c.decisionObservationMu.Unlock()
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return err
	}
	p := decisionArtifactObservation{ModelExecutionID: metadata.ModelExecutionID, Version: 1, Scope: decisionPrimitiveScope(events, c.activeBranchID(), metadata), SchemaHash: compiled.SchemaSHA256, CallID: call.ID, Input: utils.RedactSecrets(call.Input)}
	// The authentic view reports only reads included in the response. Do not
	// infer success from requested IDs or parse artifact-controlled XML text.
	ids := tools.ViewArtifactReads(*response)
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		if id == "" {
			return nil
		} // Path reads never become decision evidence.
		ref, authorized := c.authorizedDecisionEvidence(ctx, id)
		if !authorized {
			return fmt.Errorf("decision_evidence_unauthorized")
		}
		p.Sources = append(p.Sources, ref)
	}
	output := utils.RedactSecrets(response.Content)
	event, err := newDecisionObservationEvent(events, metadata, c.activeBranchID(), compiled.evidenceKeys != nil, &p, output)
	if err != nil {
		return err
	}
	allowed, err := c.admitDecisionArtifactObservation(ctx, events, compiled, metadata.TaskID, p, event.ID, output)
	if err != nil {
		return err
	}
	if !allowed {
		// Withhold the entire new response, including its read metadata.
		// Earlier observations remain intact and can still settle decisions.
		*response = fantasy.NewTextErrorResponse("decision_read_budget_exhausted: this read result was withheld because it would exceed a bound decision's complete context limit. No new evidence was observed. Request a substantially smaller focused excerpt, or call decision_primitive with the evidence already returned; use the declared uncertainty option when it is insufficient. Do not repeat this read or increase its limit.")
		return nil
	}
	maxObservation := resultDecisionContextMax
	if compiled.evidenceKeys != nil {
		maxObservation = 1 << 20
	}
	if len(output) > maxObservation || len(p.Input) > 16<<10 || len(p.Sources) > 32 {
		p.ErrorCode = "decision_context_too_large"
		p.Input = ""
		p.Sources = nil
	} else if p.Output.ID == "" {
		p.Output, err = c.putDecisionArtifact(context.WithoutCancel(ctx), metadata, "decision_observation", []byte(output))
		if err != nil {
			return err
		}
	}
	if compiled.evidenceKeys != nil && p.ErrorCode == "" {
		bindings := make(map[string]ArtifactRef)
		p.EvidenceKeys = make(map[string]string)
		for _, source := range p.Sources {
			key := artifactEvidenceKey(event, p, source)
			if err := rememberArtifactEvidenceKey(bindings, key, source); err != nil {
				return err
			}
			p.EvidenceKeys[key] = source.ID
		}
	}
	raw, err := json.Marshal(p, json.Deterministic(true))
	if err != nil {
		return err
	}
	event.Payload = raw
	if err := validateDecisionObservationEvent(event); err != nil {
		return err
	}
	_, err = c.EventJournal().Append(context.WithoutCancel(ctx), event)
	if err == nil && compiled.evidenceKeys != nil && p.ErrorCode == "" {
		var guidance strings.Builder
		guidance.WriteString("\n[runtime evidence keys: use in declared evidence fields or files_read.path; these identify successful reads, not proof of a claim]\n")
		seen := make(map[string]bool)
		for _, source := range p.Sources {
			key := artifactEvidenceKey(event, p, source)
			if !seen[key] {
				fmt.Fprintf(&guidance, "%s = %s\n", key, source.ID)
				seen[key] = true
			}
		}
		response.Content += guidance.String()
	}
	return err
}

func newDecisionObservationEvent(events []RunEvent, metadata InvocationMetadata, branchID string, evidenceKeys bool, p *decisionArtifactObservation, output string) (RunEvent, error) {
	kind := EventDecisionArtifactObserved
	if evidenceKeys {
		kind = EventArtifactEvidenceObserved
	}
	event := RunEvent{ID: generateEventID(), Type: string(kind), RunID: metadata.RunID, BranchID: branchID, TaskID: metadata.TaskID, Actor: metadata.AgentName, Attempt: metadata.Attempt, IdempotencyKey: strings.Join([]string{"decision-observation", branchID, p.Scope, metadata.RunID, fmt.Sprint(metadata.Attempt), p.CallID}, ":")}
	for _, previous := range events {
		if previous.BranchID != event.BranchID || previous.IdempotencyKey != event.IdempotencyKey {
			continue
		}
		var observed decisionArtifactObservation
		if err := json.Unmarshal(previous.Payload, &observed); err != nil {
			return RunEvent{}, err
		}
		if previous.Type != event.Type || observed.ErrorCode != "" || observed.Output.SHA256 != decisionBytesHash([]byte(output)) {
			return RunEvent{}, ErrDecisionIdempotencyConflict
		}
		event.ID = previous.ID
		// Reuse the durable artifact metadata, including its timestamp.
		// Append still checks all remaining payload fields for conflict.
		p.Output = observed.Output
		break
	}
	return event, nil
}

type decisionContextObservation struct {
	EventID string        `json:"event_id"`
	Input   string        `json:"input"`
	Sources []ArtifactRef `json:"sources"`
	Content string        `json:"content"`
}

type decisionContextEnvelope struct {
	Finding      any                          `json:"finding"`
	Observations []decisionContextObservation `json:"observations"`
}

func (c *Coordinator) buildDecisionContext(ctx context.Context, events []RunEvent, scope string, compiled *CompiledResultContract, row resultFixedCollectionRow, maxBytes int) ([]byte, error) {
	return c.buildDecisionContextWithObservation(ctx, events, scope, compiled, row, maxBytes, nil)
}

// Admission uses the same encoding, source authorization and complete history
// as inference. It never drops, summarizes or truncates an observed read.
func (c *Coordinator) admitDecisionArtifactObservation(ctx context.Context, events []RunEvent, compiled *CompiledResultContract, todoID string, p decisionArtifactObservation, eventID, output string) (bool, error) {
	if !hasResultDecisions(compiled) {
		return true, nil
	}
	if len(p.Input) > 16<<10 || len(p.Sources) > 32 {
		return false, nil
	}
	plans, err := c.fixedCollectionPlans(todoID)
	if err != nil {
		return false, err
	}
	prospective := &decisionContextObservation{EventID: eventID, Input: p.Input, Sources: p.Sources, Content: output}
	for _, plan := range plans {
		for _, row := range plan.rows {
			for _, group := range plan.spec.DecisionGroups {
				if _, err := c.buildDecisionContextWithObservation(ctx, events, p.Scope, compiled, row, group.Context.MaxBytes, prospective); err != nil {
					if err.Error() == "decision_context_too_large" {
						return false, nil
					}
					return false, err
				}
			}
		}
	}
	return true, nil
}

func (c *Coordinator) buildDecisionContextWithObservation(ctx context.Context, events []RunEvent, scope string, compiled *CompiledResultContract, row resultFixedCollectionRow, maxBytes int, prospective *decisionContextObservation) ([]byte, error) {
	envelope := decisionContextEnvelope{Finding: row.Finding}
	alreadyObserved := false
	for _, event := range events {
		if event.Type != string(EventDecisionArtifactObserved) && event.Type != string(EventArtifactEvidenceObserved) {
			continue
		}
		if err := validateDecisionObservationEvent(event); err != nil {
			return nil, err
		}
		var p decisionArtifactObservation
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return nil, err
		}
		if p.Scope != scope {
			continue
		}
		if prospective != nil && event.ID == prospective.EventID {
			alreadyObserved = true
		}
		if p.SchemaHash != compiled.SchemaSHA256 {
			return nil, fmt.Errorf("decision_binding_drift")
		}
		if p.ErrorCode != "" {
			return nil, fmt.Errorf("%s", p.ErrorCode)
		}
		for _, source := range p.Sources {
			ref, ok := c.authorizedDecisionEvidence(ctx, source.ID)
			if !ok || ref.SHA256 != source.SHA256 {
				return nil, fmt.Errorf("decision_evidence_unauthorized")
			}
		}
		content, err := c.readDecisionArtifact(ctx, p.Output, maxBytes)
		if err != nil {
			return nil, err
		}
		envelope.Observations = append(envelope.Observations, decisionContextObservation{EventID: event.ID, Input: p.Input, Sources: p.Sources, Content: string(content)})
		raw, err := json.Marshal(envelope, json.Deterministic(true))
		if err != nil {
			return nil, err
		}
		if len(raw) > maxBytes {
			return nil, fmt.Errorf("decision_context_too_large")
		}
	}
	if prospective != nil && !alreadyObserved {
		envelope.Observations = append(envelope.Observations, *prospective)
	}
	if len(envelope.Observations) == 0 {
		return nil, fmt.Errorf("decision_context_unavailable")
	}
	raw, err := json.Marshal(envelope, json.Deterministic(true))
	if len(raw) > maxBytes {
		return nil, fmt.Errorf("decision_context_too_large")
	}
	return raw, err
}

func (c *Coordinator) authorizedDecisionEvidence(ctx context.Context, id string) (ArtifactRef, bool) {
	scope, ok := artifactAccessScopeFromContext(ctx)
	if !ok {
		return ArtifactRef{}, false
	}
	ref, _, authorized := c.authorizedArtifactRef(ctx, id)
	if !authorized {
		return ArtifactRef{}, false
	}
	for _, allowed := range scope.AuthorizedRefs {
		if allowed.ID == ref.ID && allowed.SHA256 == ref.SHA256 {
			return ref, true
		}
	}
	return ArtifactRef{}, false
}
