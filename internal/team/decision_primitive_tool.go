package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/decisionrt"
	"github.com/kjelly/hufu/internal/tools"
)

const decisionPrimitiveToolName = "decision_primitive"

type decisionPrimitiveTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (*decisionPrimitiveTool) boundArtifactPolicyTool() {}
func (*decisionPrimitiveTool) DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor {
	return tools.ToolWorkspaceScopeDescriptor{}
}

func (t *decisionPrimitiveTool) Info() fantasy.ToolInfo {
	// The handler's catalog is filtered again using runtime invocation identity.
	// The tool descriptor is stable across agents so frozen surface digests
	// remain consistent; transport and credentials never enter the schema.
	description := "Make a bounded helper decision from the team's trusted catalog. Return decided or abstained; a decision never authorizes an action. Provide exactly the declared scalar context fields. Catalog (agents lists define grants): "
	if t.coordinator != nil && t.coordinator.decisionPrimitives != nil {
		description += t.coordinator.decisionPrimitives.ToolDescription()
	}
	return fantasy.ToolInfo{Name: decisionPrimitiveToolName, Description: description, Parameters: map[string]any{
		"name":    map[string]any{"type": "string"},
		"context": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": []string{"string", "boolean", "number"}}},
	}, Required: []string{"name", "context"}}
}

type decisionPrimitivePayload struct {
	Version       int                 `json:"version"`
	Name          string              `json:"name"`
	Scope         string              `json:"scope"`
	CatalogHash   string              `json:"catalog_hash"`
	RequestDigest string              `json:"request_digest"`
	CallID        string              `json:"call_id"`
	Result        *decisionrt.Result  `json:"result,omitempty"`
	Receipt       *decisionrt.Receipt `json:"receipt,omitempty"`
	ErrorCode     string              `json:"error_code,omitempty"`
}

func (t *decisionPrimitiveTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	c := t.coordinator
	if c == nil || c.decisionPrimitives == nil {
		return fantasy.NewTextErrorResponse("decision_unavailable"), nil
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	// Coordinator context requests deliberately have no worker task ID.
	// Bind those calls to the existing coordinator pseudo-task, never to a
	// model-supplied identity.
	if metadata.TaskID == "" && metadata.AgentName == "coordinator" && metadata.AgentRole == "coordinator" {
		metadata.TaskID = CoordTodoID
	}
	if !ok || metadata.RunID == "" || metadata.TaskID == "" || metadata.AgentName == "" || metadata.Attempt < 1 || call.ID == "" {
		return fantasy.NewTextErrorResponse("decision_identity_missing"), nil
	}
	if c.decisionPrimitivePolicyDenied(ctx, metadata.AgentName) {
		return fantasy.NewTextErrorResponse("decision_policy_denied"), nil
	}
	var args struct {
		Name    string         `json:"name"`
		Context map[string]any `json:"context"`
	}
	if len(call.Input) > 80<<10 {
		return fantasy.NewTextErrorResponse("decision_input_invalid"), nil
	}
	if _, err := decodeUniqueJSON([]byte(call.Input)); err != nil {
		return fantasy.NewTextErrorResponse("decision_input_invalid"), nil
	}
	decoder := json.NewDecoder(strings.NewReader(call.Input))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if decoder.Decode(&args) != nil || args.Context == nil {
		return fantasy.NewTextErrorResponse("decision_input_invalid"), nil
	}
	request, limit, err := c.decisionPrimitives.Request(args.Name, metadata.AgentName, args.Context)
	if err != nil {
		return fantasy.NewTextErrorResponse("decision_input_or_grant_invalid"), nil
	}
	digest, err := decisionrt.Digest(request)
	if err != nil {
		return fantasy.NewTextErrorResponse("decision_input_invalid"), nil
	}
	if !c.hasDurableEventJournal() {
		return fantasy.NewTextErrorResponse("decision_journal_unavailable"), nil
	}
	select {
	case c.decisionPrimitiveGate <- struct{}{}:
		defer func() { <-c.decisionPrimitiveGate }()
	case <-ctx.Done():
		return fantasy.ToolResponse{}, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return fantasy.ToolResponse{}, err
	}
	return c.runDecisionPrimitive(ctx, metadata, args.Name, request, digest, limit, call.ID)
}

func (c *Coordinator) runDecisionPrimitive(ctx context.Context, metadata InvocationMetadata, name string, request decisionrt.Request, digest string, limit int, callID string) (fantasy.ToolResponse, error) {
	if exceeded, _ := c.budgetExceeded(); exceeded || c.IsWrapUp() {
		return fantasy.NewTextErrorResponse("decision_budget_exceeded"), nil
	}
	if err := c.EventJournal().VerifyHashChain(ctx); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("verify decision journal: %w", err)
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	scope := decisionPrimitiveScope(events, c.activeBranchID(), metadata)
	payload := decisionPrimitivePayload{Version: 1, Name: name, Scope: scope, CatalogHash: c.decisionPrimitives.Hash(), RequestDigest: digest, CallID: callID}
	previous, calls, err := c.findDecisionPrimitiveReplay(events, payload, metadata.RunID)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if previous != nil {
		return decisionPrimitiveResponse(*previous)
	}
	if calls >= limit {
		return fantasy.NewTextErrorResponse("decision_call_limit"), nil
	}
	if err := c.appendDecisionPrimitiveEvent(ctx, metadata, EventDecisionPrimitiveStarted, payload, fmt.Sprintf("%s:start:%d", payload.Name, calls+1)); err != nil {
		return fantasy.ToolResponse{}, err
	}
	result, receipt, inferErr := c.decisionPrimitives.Decide(ctx, name, request)
	if inferErr != nil {
		payload.ErrorCode = decisionPrimitiveErrorCode(inferErr)
	} else {
		if err := c.decisionPrimitives.ValidatePublication(name, digest, result, receipt); err != nil {
			return fantasy.ToolResponse{}, err
		}
		payload.Result, payload.Receipt = &result, &receipt
	}
	// Once inference returns, commit its outcome even if cancellation arrived.
	// This is a durability operation; it does not start another model attempt.
	if err := c.appendDecisionPrimitiveEvent(context.WithoutCancel(ctx), metadata, EventDecisionPrimitiveSettled, payload, fmt.Sprintf("%s:settled:%d", payload.Name, calls+1)); err != nil {
		return fantasy.ToolResponse{}, err
	}
	return decisionPrimitiveResponse(payload)
}

func decisionPrimitiveErrorCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "decision_canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "decision_timeout"
	}
	if typed, ok := errors.AsType[*decisionrt.RuntimeError](err); ok {
		return "decision_" + string(typed.Kind)
	}
	return "decision_backend_failure"
}

func (c *Coordinator) findDecisionPrimitiveReplay(events []RunEvent, payload decisionPrimitivePayload, runID string) (*decisionPrimitivePayload, int, error) {
	calls := 0
	for _, event := range events {
		if event.Type != string(EventDecisionPrimitiveStarted) && event.Type != string(EventDecisionPrimitiveSettled) {
			continue
		}
		var previous decisionPrimitivePayload
		if err := validateDecisionPrimitiveEvent(event); err != nil {
			return nil, 0, err
		}
		if err := decodeStrictDecisionPayload(event.Payload, &previous); err != nil || previous.Version != 1 {
			return nil, 0, fmt.Errorf("invalid decision journal payload")
		}
		if previous.CatalogHash != payload.CatalogHash || previous.Name != payload.Name {
			continue
		}
		if event.Type == string(EventDecisionPrimitiveStarted) {
			calls++
		}
		if event.Type == string(EventDecisionPrimitiveSettled) && previous.Scope == payload.Scope && previous.RequestDigest == payload.RequestDigest {
			if previous.ErrorCode != "" && (previous.CallID != payload.CallID || event.RunID != runID) {
				continue
			}
			if previous.ErrorCode == "" {
				if previous.Result == nil || previous.Receipt == nil {
					return nil, 0, fmt.Errorf("decision result missing")
				}
				if err := c.decisionPrimitives.ValidatePublication(previous.Name, payload.RequestDigest, *previous.Result, *previous.Receipt); err != nil {
					return nil, 0, err
				}
			}
			return &previous, calls, nil
		}
	}
	return nil, calls, nil
}

func (c *Coordinator) decisionPrimitiveTools() []fantasy.AgentTool {
	if c.decisionPrimitives.Hash() == "" {
		return nil
	}
	return []fantasy.AgentTool{&decisionPrimitiveTool{coordinator: c}}
}

func decisionPrimitiveScope(events []RunEvent, branch string, metadata InvocationMetadata) string {
	anchor := metadata.RunID
	if metadata.TaskID != CoordTodoID {
		for _, event := range events {
			if event.Type == string(EventTaskCreated) && event.TaskID == metadata.TaskID {
				anchor = event.ID
			}
		}
	}
	sum := sha256.Sum256([]byte(branch + "\x00" + metadata.AgentName + "\x00" + metadata.TaskID + "\x00" + anchor))
	return hex.EncodeToString(sum[:])
}

func (c *Coordinator) appendDecisionPrimitiveEvent(ctx context.Context, metadata InvocationMetadata, kind EventType, payload decisionPrimitivePayload, key string) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	proposed := RunEvent{Type: string(kind), RunID: metadata.RunID, BranchID: c.activeBranchID(), TaskID: metadata.TaskID, Attempt: metadata.Attempt, Actor: metadata.AgentName,
		IdempotencyKey: "decision-primitive:" + c.activeBranchID() + ":" + payload.CatalogHash + ":" + key, Payload: encoded}
	if err := validateDecisionPrimitiveEvent(proposed); err != nil {
		return err
	}
	event, err := c.EventJournal().Append(ctx, proposed)
	if err != nil {
		return fmt.Errorf("persist decision primitive: %w", err)
	}
	var persisted decisionPrimitivePayload
	if err := decodeStrictDecisionPayload(event.Payload, &persisted); err != nil {
		return err
	}
	canonical, err := json.Marshal(persisted)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, encoded) {
		return fmt.Errorf("decision primitive persistence conflict")
	}
	if kind == EventDecisionPrimitiveSettled {
		return c.mutateSessionData(func(sd *SessionData) error {
			for _, record := range sd.DecisionPrimitiveResults {
				if record.EventID == event.ID {
					return nil
				}
			}
			sd.DecisionPrimitiveResults = append(sd.DecisionPrimitiveResults, decisionPrimitiveRecord(event, persisted))
			return nil
		})
	}
	return nil
}

func validateDecisionPrimitiveEvent(event RunEvent) error {
	var payload decisionPrimitivePayload
	if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
		return err
	}
	if payload.Version != 1 || payload.Name == "" || len(payload.Name) > 128 || payload.CallID == "" || len(payload.CallID) > 512 || !decisionDigestPattern.MatchString(payload.CatalogHash) || !decisionDigestPattern.MatchString(payload.Scope) || !strings.HasPrefix(payload.RequestDigest, "sha256:") || !decisionDigestPattern.MatchString(strings.TrimPrefix(payload.RequestDigest, "sha256:")) || event.RunID == "" || event.TaskID == "" || event.Actor == "" || event.BranchID == "" || event.Attempt < 1 {
		return fmt.Errorf("invalid decision primitive event identity")
	}
	if event.Type == string(EventDecisionPrimitiveStarted) {
		if payload.Result != nil || payload.Receipt != nil || payload.ErrorCode != "" {
			return fmt.Errorf("invalid decision primitive start")
		}
		return nil
	}
	if event.Type != string(EventDecisionPrimitiveSettled) {
		return fmt.Errorf("invalid decision primitive event type")
	}
	if payload.ErrorCode != "" {
		if payload.Result != nil || payload.Receipt != nil || len(payload.ErrorCode) > 128 || !strings.HasPrefix(payload.ErrorCode, "decision_") {
			return fmt.Errorf("invalid decision primitive failure")
		}
		return nil
	}
	if payload.Result == nil || payload.Receipt == nil || payload.Receipt.RequestDigest != payload.RequestDigest || payload.Receipt.SpecID != payload.Name || payload.Receipt.Purpose != payload.Name || payload.Result.Status != payload.Receipt.Status || payload.Result.Backend != payload.Receipt.Backend || payload.Result.Model != payload.Receipt.Model {
		return fmt.Errorf("invalid decision primitive settlement")
	}
	return nil
}

// DecisionPrimitiveRecord is a content-free projection for sessions, JSON,
// and reports. Full typed results remain in the verified event journal.
type DecisionPrimitiveRecord struct {
	EventID       string `json:"event_id"`
	RunID         string `json:"run_id"`
	TaskID        string `json:"task_id"`
	Agent         string `json:"agent"`
	Attempt       int    `json:"attempt"`
	Name          string `json:"name"`
	RequestDigest string `json:"request_digest"`
	Status        string `json:"status"`
	Backend       string `json:"backend,omitempty"`
	Model         string `json:"model,omitempty"`
	DurationMS    uint64 `json:"duration_ms,omitzero"`
	ErrorCode     string `json:"error_code,omitempty"`
}

func decisionPrimitiveRecord(event RunEvent, payload decisionPrimitivePayload) DecisionPrimitiveRecord {
	record := DecisionPrimitiveRecord{EventID: event.ID, RunID: event.RunID, TaskID: event.TaskID, Agent: event.Actor, Attempt: event.Attempt, Name: payload.Name, RequestDigest: payload.RequestDigest, ErrorCode: payload.ErrorCode, Status: "error"}
	if payload.Result != nil {
		record.Status, record.Backend, record.Model = string(payload.Result.Status), payload.Result.Backend, payload.Result.Model
	}
	if payload.Receipt != nil {
		record.DurationMS = payload.Receipt.DurationMS
	}
	return record
}

func (c *Coordinator) DecisionPrimitiveResults(ctx context.Context) ([]DecisionPrimitiveRecord, error) {
	if c == nil || !c.hasDurableEventJournal() {
		return nil, nil
	}
	if err := c.EventJournal().VerifyHashChain(ctx); err != nil {
		return nil, err
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	return projectDecisionPrimitiveResults(events)
}

func projectDecisionPrimitiveResults(events []RunEvent) ([]DecisionPrimitiveRecord, error) {
	var records []DecisionPrimitiveRecord
	for _, event := range events {
		if event.Type != string(EventDecisionPrimitiveSettled) {
			continue
		}
		if err := validateDecisionPrimitiveEvent(event); err != nil {
			return nil, err
		}
		var payload decisionPrimitivePayload
		if err := decodeStrictDecisionPayload(event.Payload, &payload); err != nil {
			return nil, err
		}
		records = append(records, decisionPrimitiveRecord(event, payload))
	}
	return records, nil
}

func decisionPrimitiveResponse(payload decisionPrimitivePayload) (fantasy.ToolResponse, error) {
	if payload.ErrorCode != "" {
		return fantasy.NewTextErrorResponse(payload.ErrorCode), nil
	}
	if payload.Result == nil || payload.Receipt == nil || payload.Receipt.RequestDigest != payload.RequestDigest || payload.Result.Status != payload.Receipt.Status {
		return fantasy.ToolResponse{}, fmt.Errorf("decision primitive receipt mismatch")
	}
	encoded, err := json.Marshal(struct {
		Result  decisionrt.Result  `json:"result"`
		Receipt decisionrt.Receipt `json:"receipt"`
	}{*payload.Result, *payload.Receipt})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	return fantasy.NewTextResponse(string(encoded)), nil
}

func staticDecisionPrimitiveGrant(session *TeamSession, def *agent.AgentDef, policy EffectiveTeamContractContext) bool {
	if session == nil || def == nil || policy.NoNet || policy.ForceMCP || def.NoNet || def.ForceMCP || !explicitlyDeclaresTool(def.Tools, decisionPrimitiveToolName) {
		return false
	}
	for _, entry := range session.Config.DecisionPrimitives {
		if slices.Contains(entry.Agents, def.Name) {
			return true
		}
	}
	return false
}

func (c *Coordinator) coordinatorDecisionPrimitiveGranted() bool {
	if c == nil || c.noNet || c.forceMCP || len(c.decisionPrimitives.Names("coordinator")) == 0 {
		return false
	}
	def := c.GetOrchestratorDef()
	return def != nil && !def.NoNet && !def.ForceMCP
}

func (c *Coordinator) decisionPrimitivePolicyDenied(ctx context.Context, actor string) bool {
	blocked, _ := ctx.Value(tools.AgentNetworkBlockKey).(bool)
	forceMCP, _ := ctx.Value(tools.AgentForceMCPKey).(bool)
	if c.noNet || c.forceMCP || blocked || forceMCP || c.toolDeniedByTeam(decisionPrimitiveToolName) {
		return true
	}
	if actor == "coordinator" && !c.coordinatorDecisionPrimitiveGranted() {
		return true
	}
	def := c.session.Agents[actor]
	return def != nil && (def.NoNet || def.ForceMCP)
}

func validateDecisionPrimitiveGrants(session *TeamSession) error {
	for name, entry := range session.Config.DecisionPrimitives {
		for _, actor := range entry.Agents {
			if actor == "coordinator" {
				continue
			}
			def := session.Agents[actor]
			if def == nil || def.Name != actor || strings.TrimSpace(actor) != actor {
				return fmt.Errorf("decision-primitives.%s: unknown agent grant %q", name, actor)
			}
		}
	}
	return nil
}
