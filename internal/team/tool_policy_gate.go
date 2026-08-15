package team

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

const initialCoordinatorToolCorrectionPrefix = "Initial coordinator tool correction:"

// The authorization boundary for agent tool calls lives here.
//
// It used to live in the stream's OnToolCall callback, which can only return an
// error — and an error there aborts the entire model round. A single call to an
// ungranted tool therefore destroyed the whole attempt, discarding every tool
// call the worker had already completed and burning a retry, before the call was
// even recorded as evidence. Denials are ordinary, recoverable conditions: the
// model should be told and given the chance to finish with the tools it has.
// The exception is normally a coordinator policy violation: a coordinator
// makes cross-task decisions, so it cannot safely continue after violating an
// ordering or authorization boundary. The configured initial-tool boundary
// gets one protocol-only correction because no task or side effect exists yet.
//
// Enforcing in a tool wrapper also unifies the decision with the tool adapter in
// internal/tools, which already surfaces its own denials as tool errors, so the
// two gates no longer disagree about the same tool.

// policyGatedTool wraps an agent tool so an authorization denial reaches the
// model as a tool error result it can adapt to.
type policyGatedTool struct {
	inner       fantasy.AgentTool
	coordinator *Coordinator
}

func (t *policyGatedTool) Info() fantasy.ToolInfo { return t.inner.Info() }

func (t *policyGatedTool) ProviderOptions() fantasy.ProviderOptions {
	return t.inner.ProviderOptions()
}

func (t *policyGatedTool) SetProviderOptions(opts fantasy.ProviderOptions) {
	t.inner.SetProviderOptions(opts)
}

func (t *policyGatedTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	denial, fatal := t.coordinator.authorizeToolInvocation(ctx, agentName, t.Info().Name)
	if fatal != nil {
		return fantasy.ToolResponse{}, fatal
	}
	if denial != "" {
		t.coordinator.setToolPolicyVerdict(call.ID, "denied")
		if todoID, _ := ctx.Value(todoIDKey{}).(string); todoID == CoordTodoID {
			if t.coordinator.allowInitialToolCorrection(agentName, t.Info().Name) {
				t.coordinator.report(t.coordinator.newEvent("step").withAgent(agentName).
					withMessage(fmt.Sprintf("tool %q denied by initial coordinator policy; one correction remains", t.Info().Name)))
				return fantasy.NewTextErrorResponse(initialCoordinatorToolCorrectionPrefix + " " + denial + ". This call was not executed. Call the required tool now; another ordering violation will terminate the run."), nil
			}
			if t.coordinator != nil {
				t.coordinator.report(t.coordinator.newEvent("step").withAgent(agentName).
					withMessage(fmt.Sprintf("tool %q denied by coordinator policy; aborting run", t.Info().Name)))
			}
			// Initial-batch ordering and all other coordinator policy denials
			// are hard run boundaries. Reusing errCoordinatorToolFailure also
			// prevents wrap-up recovery from spending another coordinator turn.
			return fantasy.ToolResponse{}, fmt.Errorf("%w: %s", errCoordinatorToolFailure, denial)
		}
		if t.coordinator != nil {
			t.coordinator.report(t.coordinator.newEvent("step").withAgent(agentName).
				withMessage(fmt.Sprintf("tool %q denied by policy; continuing with remaining tools", t.Info().Name)))
		}
		return fantasy.NewTextErrorResponse(denial), nil
	}
	if denial := t.coordinator.mandatorySkillLoadDenial(ctx, t.Info().Name, call.Input); denial != "" {
		t.coordinator.setToolPolicyVerdict(call.ID, "denied")
		return fantasy.NewTextErrorResponse(denial), nil
	}
	sequence := taskToolSequenceFromContext(ctx)
	reservedSlot, effectiveInput, canonicalized, sequenceDenial := sequence.reserve(t.Info().Name, call.Input, earlyTerminalSubmitResult(t.Info().Name, call))
	if sequenceDenial != "" {
		t.coordinator.setToolPolicyVerdict(call.ID, "denied")
		if t.coordinator != nil {
			t.coordinator.report(t.coordinator.newEvent("step").withAgent(agentName).
				withMessage(fmt.Sprintf("tool %q denied by closed task sequence", t.Info().Name)))
		}
		// Any out-of-order call is itself a terminal contract failure. Do not
		// let the model keep trying different tools after the sequence has
		// rejected one; only an honest failed/blocked submit_result may follow.
		sequence.markFailedAt(reservedSlot, t.Info().Name, sequenceDenial)
		return fantasy.NewTextErrorResponse(sequenceDenial), nil
	}
	if canonicalized {
		t.coordinator.setToolPolicyVerdict(call.ID, "canonicalized")
		if t.coordinator != nil {
			todoID, _ := ctx.Value(todoIDKey{}).(string)
			t.coordinator.report(t.coordinator.newEvent("policy_decision").withAgent(agentName).withTodoID(todoID).
				withMessage(fmt.Sprintf("template-owned input selected for tool %q at closed task sequence position %d", t.Info().Name, reservedSlot+1)))
		}
	}
	if transform := sequence.inputTransform(reservedSlot); transform != "" {
		transformedInput, changed, transformErr := transformTaskToolInput(transform, t.Info().Name, effectiveInput)
		if transformErr != nil {
			t.coordinator.setToolPolicyVerdict(call.ID, "denied")
			sequence.markFailedAt(reservedSlot, t.Info().Name, transformErr.Error())
			return fantasy.NewTextErrorResponse("template-owned input transform rejected this closed task slot: " + transformErr.Error()), nil
		}
		if changed {
			t.coordinator.setToolPolicyVerdict(call.ID, "transformed")
			if t.coordinator != nil {
				todoID, _ := ctx.Value(todoIDKey{}).(string)
				t.coordinator.report(t.coordinator.newEvent("policy_decision").withAgent(agentName).withTodoID(todoID).
					withMessage(fmt.Sprintf("template-owned structural transform %q selected for tool %q at closed task sequence position %d", transform, t.Info().Name, reservedSlot+1)))
			}
		}
		effectiveInput = transformedInput
	}
	call.Input = effectiveInput
	response, err := t.inner.Run(ctx, call)
	if todoID, _ := ctx.Value(todoIDKey{}).(string); todoID == CoordTodoID && (err != nil || response.IsError) {
		detail := strings.TrimSpace(response.Content)
		if err != nil {
			if detail != "" {
				detail += ": "
			}
			detail += err.Error()
		}
		return fantasy.ToolResponse{}, fmt.Errorf("%w: tool %q failed: %s", errCoordinatorToolFailure, t.Info().Name, detail)
	}
	if err != nil || response.IsError {
		if sequence.allowsExpectedExitCode(reservedSlot, t.Info().Name, response.Content) {
			// A bounded probe often terminates with a non-zero process status
			// (for example timeout=124). When the task contract explicitly
			// names that result, preserve its output as normal evidence and let
			// the worker continue to the next declared step.
			return fantasy.NewTextResponse(response.Content), nil
		}
		// A closed sequence is an atomic evidence protocol. Once a tool has
		// failed, the worker may only report the failure; it must not spend a
		// later slot on an improvised repair or rerun.
		taskToolSequenceFromContext(ctx).markFailedAt(reservedSlot, t.Info().Name, response.Content)
	}
	return response, err
}

// allowInitialToolCorrection admits exactly one recoverable denial while the
// coordinator has no canonical task yet. It is deliberately independent of
// providers and tool semantics: the denied call never executes, and the next
// violation remains the existing hard run boundary.
func (c *Coordinator) allowInitialToolCorrection(agentName, toolName string) bool {
	if c == nil || c.initialCoordinatorToolDenial(agentName, toolName) == "" {
		return false
	}
	return c.initialToolCorrections.CompareAndSwap(0, 1)
}

func (c *Coordinator) isInitialToolCorrectionResult(toolName, result string) bool {
	if c == nil || c.initialToolCorrections.Load() != 1 ||
		!strings.HasPrefix(strings.TrimSpace(result), initialCoordinatorToolCorrectionPrefix) {
		return false
	}
	want := c.initialCoordinatorToolName()
	return want != "" && strings.TrimSpace(toolName) != want
}

// earlyTerminalSubmitResult reports whether call is a submit_result
// invocation whose declared status is an honest early bail-out (blocked,
// failed, partial) rather than a completion claim (success,
// completed_with_gaps). Only these statuses may skip ahead in a closed task
// tool sequence (taskToolSequence.reserve) — a worker must still complete
// every scripted step before a success claim; it must never be forced to
// keep fabricating remaining steps once it has already determined, and is
// honestly reporting, that the checkpoint cannot proceed. Any parse failure
// (malformed JSON, missing status, an unrecognized status) returns false,
// so it is never treated as more privileged than an ordinary tool call.
func earlyTerminalSubmitResult(toolName string, call fantasy.ToolCall) bool {
	if toolName != "submit_result" {
		return false
	}
	var payload struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(call.Input), &payload); err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(payload.Status)) {
	case TaskResultStatusBlocked, TaskResultStatusFailed, TaskResultStatusPartial:
		return true
	default:
		return false
	}
}

// gatePolicyTools wraps every tool in agentTools with the recoverable policy
// gate. Tools already wrapped are returned as-is so repeated application (for
// example an escalated retry reusing a prepared set) stays idempotent.
func (c *Coordinator) gatePolicyTools(agentTools []fantasy.AgentTool) []fantasy.AgentTool {
	if c == nil || len(agentTools) == 0 {
		return agentTools
	}
	gated := make([]fantasy.AgentTool, 0, len(agentTools))
	for _, t := range agentTools {
		if t == nil {
			continue
		}
		if _, already := t.(*policyGatedTool); already {
			gated = append(gated, t)
			continue
		}
		gated = append(gated, &policyGatedTool{inner: t, coordinator: c})
	}
	return gated
}

// createGatedAgent is the only agent constructor this package uses. Funnelling
// every agent through one call is what makes the recoverable gate complete: a
// tool set assembled somewhere that skipped gatePolicyTools would have no
// authorization boundary at all now that OnToolCall no longer aborts.
// TestAgentsAreCreatedThroughTheGatedConstructor enforces the funnel.
func (c *Coordinator) createGatedAgent(ctx context.Context, provider *agent.OllamaProvider, cfg agent.AgentConfig, agentTools []fantasy.AgentTool) (fantasy.Agent, error) {
	gated := c.gatePolicyTools(agentTools)
	if todoID, _ := ctx.Value(todoIDKey{}).(string); todoID == CoordTodoID {
		// Schema validation must be the outermost coordinator boundary: malformed
		// arguments are rejected before authorization, sequence state, or the
		// underlying tool can observe the call.
		gated = c.wrapWithProtocolRepair(gated)
	}
	return agent.CreateAgent(ctx, provider, cfg, gated)
}

// authorizeToolInvocation resolves whether agentName may call toolName under the
// allowlist attached to ctx.
//
// It returns ("", nil) to allow the call, a non-empty denial message the model
// should see when the call is refused but the run can continue, and a non-nil
// error only when the failure is not something the model can work around (a
// cancelled context, for instance).
func (c *Coordinator) authorizeToolInvocation(ctx context.Context, agentName, toolName string) (string, error) {
	if denial := c.initialCoordinatorToolDenial(agentName, toolName); denial != "" {
		return denial, nil
	}
	configured := tools.GetToolsAllowed(ctx)
	if configured == nil {
		// No allowlist attached: the tool adapter in internal/tools remains the
		// source of truth, as it is for legacy session permissions and
		// deterministic test agents.
		return "", nil
	}
	// An operator's explicit "always allow"/"always deny" from this session is a
	// decision about this exact tool. The tool adapter honours it; this gate must
	// too, or the same tool is permitted by one boundary and refused by the other.
	if perms, ok := ctx.Value(tools.AgentToolsSessionPermissionsKey).(map[string]bool); ok {
		if allowed, decided := perms[toolName]; decided {
			if allowed {
				return "", nil
			}
			return fmt.Sprintf("tool %q was denied earlier in this session and stays denied. Do not call it again; finish with the tools you have and report what you could not do.", toolName), nil
		}
	}
	allowed := make(map[string]bool, len(configured))
	for _, name := range configured {
		allowed[strings.TrimSpace(name)] = true
	}
	decision, err := c.authorizeStreamTool(ctx, agentName, toolName, allowed)
	if err != nil {
		return "", fmt.Errorf("tool authorization failed for %q: %w", toolName, err)
	}
	if decision.Code == DecisionAllow {
		return "", nil
	}
	reason := strings.TrimSpace(decision.Reason)
	if reason == "" {
		reason = "not authorized"
	}
	return fmt.Sprintf("tool %q is not available to you: %s. Do not call it again — achieve the goal with the tools you do have, and state in your result what you could not do without it.", toolName, reason), nil
}

func (c *Coordinator) initialCoordinatorToolDenial(agentName, toolName string) string {
	if c == nil || c.session == nil || c.taskTracker == nil || strings.TrimSpace(agentName) != "" {
		return ""
	}
	want := c.initialCoordinatorToolName()
	if want == "" || toolName == want {
		return ""
	}
	return fmt.Sprintf("coordinator's first tool call must be %q before the initial delegation batch; got %q", want, toolName)
}

// initialCoordinatorToolName returns the configured tool only while no
// canonical task exists. It is shared by the runtime authorization gate and
// the model-visible tool filter so stale conversation or memory prose cannot
// cause an exposed-but-forbidden first tool call.
func (c *Coordinator) initialCoordinatorToolName() string {
	if c == nil || c.session == nil || c.taskTracker == nil || len(c.taskTracker.TodoList().Items()) > 0 {
		return ""
	}
	return strings.TrimSpace(c.session.Config.Delegation.InitialCoordinatorTool)
}

// unauthorizedExposedTools returns the exposed tool names that the allowlist does
// not cover. An empty allowlist means no policy is attached, so nothing is
// unauthorized.
//
// This is the invariant whose absence deadlocked every worker task: submit_result
// was handed to the model on every task and granted on none.
func unauthorizedExposedTools(exposed, allowed []string) []string {
	if len(allowed) == 0 {
		return nil
	}
	granted := toolNameSet(allowed)
	var missing []string
	seen := map[string]bool{}
	for _, name := range exposed {
		name = strings.TrimSpace(name)
		if name == "" || granted[name] || seen[name] {
			continue
		}
		seen[name] = true
		missing = append(missing, name)
	}
	return missing
}

// validateToolGrants checks the statically constructible worker slices before a
// run starts. Per-task MCP and protocol additions are checked from their actual
// final slices at construction time, because predicting them here would merely
// recreate the drift this guard exists to prevent.
func (c *Coordinator) validateToolGrants() error {
	if c == nil || c.session == nil {
		return nil
	}
	names := make([]string, 0, len(c.session.Agents))
	for name := range c.session.Agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := c.session.Agents[name]
		if def == nil || strings.EqualFold(def.Role, "coordinator") {
			continue // orchestrator grants are asserted by coordinatorAllowedToolNames
		}
		exposed := agentToolNames(c.selectWorkerTools(def))
		allowed := tools.GetToolsAllowed(c.withEffectiveToolsAllowed(context.Background(), def, exposed))
		if missing := unauthorizedExposedTools(exposed, allowed); len(missing) > 0 {
			return fmt.Errorf("agent %q would be shown tools it is not authorized to call: %s — every tool handed to a model must be in its runtime allowlist", name, strings.Join(missing, ", "))
		}
	}
	return nil
}
