package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

func (c *Coordinator) SetRunInputAssignments(assignments []RunInputAssignment) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.runInputAssignments = cloneRunInputAssignments(assignments)
}

func (c *Coordinator) RunInputSnapshot() *RunInputSnapshot {
	if c == nil {
		return nil
	}
	var snapshot *RunInputSnapshot
	c.viewSessionData(func(session *SessionData) {
		for index := range session.RunInputSnapshots {
			if session.RunInputSnapshots[index].ID == session.ActiveRunInputSnapshotID {
				snapshot = CloneRunInputSnapshot(&session.RunInputSnapshots[index])
				return
			}
		}
	})
	return snapshot
}

func (c *Coordinator) resolveRunInputsForInvocation(ctx context.Context, prompt string) error {
	if c == nil || c.session == nil {
		return nil
	}
	if len(prompt) > maxRunInputPromptBytes {
		return fmt.Errorf("input_resolver_failed: invocation prompt exceeds %d bytes", maxRunInputPromptBytes)
	}
	c.mu.RLock()
	assignments := cloneRunInputAssignments(c.runInputAssignments)
	c.mu.RUnlock()
	if len(c.session.RunInputDefinitions) == 0 && len(assignments) == 0 {
		return nil
	}
	runID := strings.TrimSpace(c.executionRunID)
	if runID == "" {
		return fmt.Errorf("run input resolution requires an active run identity")
	}
	if frozen, interrupted := c.interruptedRunInputSnapshot(); interrupted {
		if frozen == nil {
			return errors.New("resume_input_conflict: interrupted typed-input run has no frozen input snapshot; start with --new")
		}
		if err := validateResumeRunInputAssignments(c.session.RunInputDefinitions, assignments, frozen, c.session.Config.Name); err != nil {
			return err
		}
		return c.mutateSessionData(func(session *SessionData) error {
			session.ActiveRunInputSnapshotID = frozen.ID
			return nil
		})
	}
	explicit, err := resolveExplicitRunInputValues(c.session.RunInputDefinitions, assignments, c.session.Config.Name)
	if err != nil {
		return err
	}
	resolverAssignments, err := c.resolveRunInputCandidates(ctx, prompt, explicit, runID, true)
	if err != nil {
		return err
	}
	assignments = append(assignments, resolverAssignments...)
	snapshot, err := ResolveRunInputSnapshot(c.session.RunInputDefinitions, assignments, runID, runID+":invocation", c.session.Config.Name)
	if err != nil {
		return err
	}
	if snapshot == nil {
		return nil
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode run_inputs_resolved event: %w", err)
	}
	key := "run-inputs:" + snapshot.RunID + ":" + snapshot.InvocationID + ":" + snapshot.SnapshotHash
	if _, err := c.emitEventOnce(key, RunEvent{Type: string(EventRunInputsResolved), Actor: "runtime", Payload: payload}); err != nil {
		return fmt.Errorf("persist run_inputs_resolved event: %w", err)
	}
	return c.mutateSessionData(func(session *SessionData) error {
		session.RunInputSnapshots = appendRunInputSnapshot(session.RunInputSnapshots, *snapshot)
		session.ActiveRunInputSnapshotID = snapshot.ID
		return nil
	})
}

func (c *Coordinator) interruptedRunInputSnapshot() (*RunInputSnapshot, bool) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil || len(c.getInterruptedTasks()) == 0 {
		return nil, false
	}
	interruptedRunID := strings.TrimSpace(c.taskTracker.TodoList().RunID())
	var snapshot *RunInputSnapshot
	c.viewSessionData(func(session *SessionData) {
		for index := range session.RunInputSnapshots {
			if session.RunInputSnapshots[index].RunID == interruptedRunID {
				snapshot = CloneRunInputSnapshot(&session.RunInputSnapshots[index])
				return
			}
		}
	})
	return snapshot, true
}

func validateResumeRunInputAssignments(definitions []RunInputDefinition, assignments []RunInputAssignment, frozen *RunInputSnapshot, teamName string) error {
	explicit, err := resolveExplicitRunInputValues(definitions, assignments, teamName)
	if err != nil {
		return err
	}
	frozenValues := make(map[string]json.RawMessage, len(frozen.Inputs))
	for _, input := range frozen.Inputs {
		frozenValues[input.Name] = input.CanonicalValue
	}
	for name, value := range explicit {
		if !bytes.Equal(value, frozenValues[name]) {
			return fmt.Errorf("resume_input_conflict: input %q differs from frozen snapshot %s; start with --new", name, frozen.ID)
		}
	}
	return nil
}

func (c *Coordinator) resolveRunInputCandidates(ctx context.Context, prompt string, explicit map[string]json.RawMessage, runID string, allowSemantic bool) ([]RunInputAssignment, error) {
	assignments := make([]RunInputAssignment, 0)
	schemaHash, err := RunInputSchemaHash(c.session.RunInputDefinitions)
	if err != nil {
		return nil, err
	}
	for _, definition := range c.session.RunInputDefinitions {
		resolver := definition.Resolver
		if resolver == nil {
			continue
		}
		resolverProvider, err := c.runInputResolverProvider(resolver)
		if err != nil {
			return nil, err
		}
		var semanticFailure error
		if allowSemantic && resolver.Mode == runInputResolverModeSemanticJSON {
			assignment, matched, err := c.resolveValidatedSemanticRunInput(ctx, prompt, explicit[definition.Name], definition, resolver, resolverProvider, schemaHash, runID)
			if errors.Is(err, errSemanticRunInputFailed) {
				semanticFailure, err = err, nil
			}
			if err != nil {
				return nil, err
			}
			if matched {
				assignments = append(assignments, assignment)
				continue
			}
		}
		response, err := c.callRunInputResolver(ctx, prompt, explicit[definition.Name], nil, definition, resolver, resolverProvider, schemaHash, runID, "fallback")
		if err != nil {
			return nil, err
		}
		assignment, matched, err := resolverResponseAssignment(definition, resolver, prompt, response)
		if err != nil {
			return nil, err
		}
		if matched {
			assignments = append(assignments, assignment)
			continue
		}
		if semanticFailure != nil && len(explicit[definition.Name]) == 0 {
			// The deterministic resolver only parses literal syntax, so its
			// no_match says nothing about prose. Without the semantic answer
			// the team default would silently replace whatever the request
			// asked for, as when "the last 22 commits" became the default 10.
			return nil, fmt.Errorf("input_resolver_failed: %s: the request could not be translated and the deterministic resolver found no value, so the default would be a guess: %w; pass the value explicitly with --input %s=<json>, or retry", definition.Name, semanticFailure, definition.Name)
		}
	}
	return assignments, nil
}

func (c *Coordinator) runInputResolverProvider(resolver *RunInputResolverSpec) (RunInputResolverProvider, error) {
	if c.session.ProviderRegistry == nil {
		return nil, errors.New("input_resolver_failed: action provider registry is unavailable")
	}
	provider, ok := c.session.ProviderRegistry.Get(resolver.Capability)
	if !ok {
		return nil, fmt.Errorf("input_resolver_failed: capability %q is not registered", resolver.Capability)
	}
	resolverProvider, ok := provider.(RunInputResolverProvider)
	if !ok {
		return nil, fmt.Errorf("input_resolver_failed: provider for %q does not implement deterministic run input resolution", resolver.Capability)
	}
	return resolverProvider, nil
}

func (c *Coordinator) callRunInputResolver(ctx context.Context, prompt string, explicit, candidate json.RawMessage, definition RunInputDefinition, resolver *RunInputResolverSpec, provider RunInputResolverProvider, schemaHash, runID, stage string) (RunInputResolverResponse, error) {
	resolverCtx := ctx
	var cancel context.CancelFunc
	if resolver.Timeout > 0 {
		resolverCtx, cancel = context.WithTimeout(ctx, time.Duration(resolver.Timeout)*time.Second)
	}
	if cancel != nil {
		defer cancel()
	}
	invocationID := "run-input-resolver:" + runID + ":" + definition.Name + ":" + schemaHash + ":" + stage
	resolverCtx = WithActionEnvironment(resolverCtx, ActionEnvironment{
		Workspace: c.session.Workspace, Repository: c.projectDir, TeamName: c.session.Config.Name,
		RunID: runID, TaskID: "<run-input:" + definition.Name + ">", Attempt: 1, ActionInvocationID: invocationID,
	})
	response, err := provider.ResolveRunInput(resolverCtx, RunInputResolverRequest{
		Type: "resolve_run_input", InputName: definition.Name, Prompt: prompt,
		ExplicitValue: slices.Clone(explicit), CandidateValue: slices.Clone(candidate), SchemaHash: schemaHash, ResolverID: resolver.ID,
	})
	if err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("input_resolver_failed: %s: %w", definition.Name, err)
	}
	if err := validateRunInputResolverResponse(response); err != nil {
		return RunInputResolverResponse{}, fmt.Errorf("input_resolver_failed: %s: %w", definition.Name, err)
	}
	return response, nil
}

func resolverResponseAssignment(definition RunInputDefinition, resolver *RunInputResolverSpec, prompt string, response RunInputResolverResponse) (RunInputAssignment, bool, error) {
	switch response.Status {
	case "no_match":
		return RunInputAssignment{}, false, nil
	case "ambiguous":
		return RunInputAssignment{}, false, fmt.Errorf("input_ambiguous: %s: %s", definition.Name, utils.RedactSecrets(response.Diagnostic))
	case "invalid":
		return RunInputAssignment{}, false, fmt.Errorf("input_invalid: %s: %s", definition.Name, utils.RedactSecrets(response.Diagnostic))
	case "matched":
		evidence := make([]InputEvidence, len(response.Evidence))
		for index, item := range response.Evidence {
			if item.End > len(prompt) {
				return RunInputAssignment{}, false, fmt.Errorf("input_resolver_failed: %s evidence range exceeds invocation prompt", definition.Name)
			}
			evidence[index] = InputEvidence{Source: RunInputSourceResolver, Location: "invocation_prompt", Start: item.Start, End: item.End, Kind: item.Kind}
		}
		return RunInputAssignment{
			Name: definition.Name, RawValue: slices.Clone(response.Value), Source: RunInputSourceResolver,
			ResolverID: resolver.ID, ResolverVersion: response.ResolverVersion, Evidence: evidence,
		}, true, nil
	default:
		return RunInputAssignment{}, false, fmt.Errorf("input_resolver_failed: %s returned unsupported status %q", definition.Name, response.Status)
	}
}

type semanticRunInputCandidate struct {
	assignment RunInputAssignment
	raw        json.RawMessage
	status     string
	diagnostic string
}

func (c *Coordinator) resolveValidatedSemanticRunInput(ctx context.Context, prompt string, explicit json.RawMessage, definition RunInputDefinition, resolver *RunInputResolverSpec, provider RunInputResolverProvider, schemaHash, runID string) (RunInputAssignment, bool, error) {
	candidate := c.resolveSemanticRunInputCandidate(ctx, prompt, explicit, definition, resolver, schemaHash, nil, "")
	if candidate.status == "no_match" {
		return RunInputAssignment{}, false, nil
	}
	repaired := false
	validationAttempt := 0
	for {
		if candidate.status == "failed" {
			return RunInputAssignment{}, false, fmt.Errorf("%w: %s", errSemanticRunInputFailed, utils.TruncateRunes(utils.RedactSecrets(candidate.diagnostic), maxSemanticRepairDiagnosticRunes))
		}
		if candidate.status == "invalid" {
			if repaired {
				return RunInputAssignment{}, false, fmt.Errorf("input_invalid: %s: semantic candidate remained invalid after one repair: %s", definition.Name, utils.RedactSecrets(candidate.diagnostic))
			}
			repaired = true
			candidate = c.resolveSemanticRunInputCandidate(ctx, prompt, explicit, definition, resolver, schemaHash, candidate.raw, candidate.diagnostic)
			if candidate.status == "no_match" {
				return RunInputAssignment{}, false, fmt.Errorf("input_invalid: %s: semantic repair returned no value", definition.Name)
			}
			continue
		}

		validationAttempt++
		response, err := c.callRunInputResolver(ctx, prompt, explicit, candidate.assignment.RawValue, definition, resolver, provider, schemaHash, runID, fmt.Sprintf("candidate-%d", validationAttempt))
		if err != nil {
			return RunInputAssignment{}, false, err
		}
		switch response.Status {
		case "matched":
			if len(response.Evidence) != 0 {
				return RunInputAssignment{}, false, fmt.Errorf("input_resolver_failed: %s candidate validator returned prompt evidence", definition.Name)
			}
			validated, err := validateAndCanonicalizeRunInput(definition.Schema, response.Value)
			if err != nil {
				return RunInputAssignment{}, false, fmt.Errorf("input_resolver_failed: %s validator returned an invalid typed value: %w", definition.Name, err)
			}
			if !bytes.Equal(validated, candidate.assignment.RawValue) {
				return RunInputAssignment{}, false, fmt.Errorf("input_resolver_failed: %s validator changed the semantic candidate instead of validating it", definition.Name)
			}
			candidate.assignment.ResolverVersion = semanticRunInputResolverVersion + "+" + response.ResolverVersion
			return candidate.assignment, true, nil
		case "invalid":
			candidate.status = "invalid"
			candidate.diagnostic = response.Diagnostic
		case "ambiguous":
			return RunInputAssignment{}, false, fmt.Errorf("input_ambiguous: %s: %s", definition.Name, utils.RedactSecrets(response.Diagnostic))
		case "no_match":
			return RunInputAssignment{}, false, fmt.Errorf("input_resolver_failed: %s candidate validator returned no_match", definition.Name)
		}
	}
}

func (c *Coordinator) resolveSemanticRunInputCandidate(ctx context.Context, prompt string, explicit json.RawMessage, definition RunInputDefinition, resolver *RunInputResolverSpec, schemaHash string, previous json.RawMessage, diagnostic string) semanticRunInputCandidate {
	semanticResolver := c.semanticRunInputResolver()
	if semanticResolver == nil {
		return semanticRunInputCandidate{status: "no_match"}
	}
	request := SemanticRunInputRequest{
		InputName: definition.Name, Prompt: prompt, Schema: definition.Schema, SchemaHash: schemaHash,
		ResolverID: resolver.ID, ExplicitValue: slices.Clone(explicit), Guidance: resolver.SemanticGuidance,
		PreviousValue: slices.Clone(previous), Diagnostic: diagnostic,
	}
	raw, err := resolveSemanticRunInputOnce(ctx, semanticResolver, resolver, request)
	if err != nil && !errors.Is(err, errSemanticRunInputUnavailable) && ctx.Err() == nil {
		// A provider hiccup, a timeout, or a truncated answer is usually
		// transient, and failing here fails the whole run.
		raw, err = resolveSemanticRunInputOnce(ctx, semanticResolver, resolver, request)
	}
	if errors.Is(err, errSemanticRunInputUnavailable) {
		// No semantic resolver is configured; only the deterministic resolver
		// and the default apply, which is the team's declared behavior.
		return semanticRunInputCandidate{status: "no_match"}
	}
	if err != nil {
		return semanticRunInputCandidate{raw: slices.Clone(raw), status: "failed", diagnostic: err.Error()}
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return semanticRunInputCandidate{raw: slices.Clone(raw), status: "no_match"}
	}
	canonical, err := validateAndCanonicalizeRunInput(definition.Schema, raw)
	if err != nil {
		return semanticRunInputCandidate{raw: slices.Clone(raw), status: "invalid", diagnostic: err.Error()}
	}
	if semanticJSONContainsCommand(canonical) {
		return semanticRunInputCandidate{raw: slices.Clone(canonical), status: "invalid", diagnostic: "semantic candidate contains a command-shaped value"}
	}
	return semanticRunInputCandidate{
		raw: slices.Clone(canonical), status: "matched",
		assignment: RunInputAssignment{
			Name: definition.Name, RawValue: canonical, Source: RunInputSourceResolver,
			ResolverID: resolver.ID, ResolverVersion: semanticRunInputResolverVersion,
			Evidence: []InputEvidence{{Source: RunInputSourceResolver, Location: "invocation_prompt", Kind: "semantic_json"}},
		},
	}
}

func resolveSemanticRunInputOnce(ctx context.Context, semanticResolver SemanticRunInputResolver, resolver *RunInputResolverSpec, request SemanticRunInputRequest) (json.RawMessage, error) {
	resolverCtx := ctx
	if resolver.Timeout > 0 {
		var cancel context.CancelFunc
		resolverCtx, cancel = context.WithTimeout(ctx, time.Duration(resolver.Timeout)*time.Second)
		defer cancel()
	}
	return semanticResolver.Resolve(resolverCtx, request)
}

func (c *Coordinator) previewRunInputs(ctx context.Context, prompt string) (*RunInputSnapshot, error) {
	if c == nil || c.session == nil {
		return nil, nil
	}
	c.mu.RLock()
	assignments := cloneRunInputAssignments(c.runInputAssignments)
	c.mu.RUnlock()
	explicit, err := resolveExplicitRunInputValues(c.session.RunInputDefinitions, assignments, c.session.Config.Name)
	if err != nil {
		return nil, err
	}
	resolverAssignments, err := c.resolveRunInputCandidates(ctx, prompt, explicit, "dry-run", false)
	if err != nil {
		return nil, err
	}
	assignments = append(assignments, resolverAssignments...)
	return ResolveRunInputSnapshot(c.session.RunInputDefinitions, assignments, "dry-run", "dry-run:invocation", c.session.Config.Name)
}

func cloneRunInputAssignments(src []RunInputAssignment) []RunInputAssignment {
	clone := slices.Clone(src)
	for index := range clone {
		clone[index].RawValue = slices.Clone(src[index].RawValue)
		clone[index].Evidence = slices.Clone(src[index].Evidence)
	}
	return clone
}
