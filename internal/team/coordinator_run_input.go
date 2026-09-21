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
		if allowSemantic && resolver.Mode == runInputResolverModeSemanticJSON {
			if assignment, matched := c.resolveSemanticRunInputCandidate(ctx, prompt, explicit[definition.Name], definition, resolver, schemaHash); matched {
				assignments = append(assignments, assignment)
				continue
			}
		}
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
		resolverCtx := ctx
		var cancel context.CancelFunc
		if resolver.Timeout > 0 {
			resolverCtx, cancel = context.WithTimeout(ctx, time.Duration(resolver.Timeout)*time.Second)
		}
		invocationID := "run-input-resolver:" + runID + ":" + definition.Name + ":" + schemaHash
		resolverCtx = WithActionEnvironment(resolverCtx, ActionEnvironment{
			Workspace: c.session.Workspace, Repository: c.projectDir, TeamName: c.session.Config.Name,
			RunID: runID, TaskID: "<run-input:" + definition.Name + ">", Attempt: 1, ActionInvocationID: invocationID,
		})
		response, resolveErr := resolverProvider.ResolveRunInput(resolverCtx, RunInputResolverRequest{
			Type: "resolve_run_input", InputName: definition.Name, Prompt: prompt,
			ExplicitValue: slices.Clone(explicit[definition.Name]), SchemaHash: schemaHash, ResolverID: resolver.ID,
		})
		if cancel != nil {
			cancel()
		}
		if resolveErr != nil {
			return nil, fmt.Errorf("input_resolver_failed: %s: %w", definition.Name, resolveErr)
		}
		if err := validateRunInputResolverResponse(response); err != nil {
			return nil, fmt.Errorf("input_resolver_failed: %s: %w", definition.Name, err)
		}
		switch response.Status {
		case "no_match":
			continue
		case "ambiguous":
			return nil, fmt.Errorf("input_ambiguous: %s: %s", definition.Name, utils.RedactSecrets(response.Diagnostic))
		case "invalid":
			return nil, fmt.Errorf("input_invalid: %s: %s", definition.Name, utils.RedactSecrets(response.Diagnostic))
		case "matched":
			evidence := make([]InputEvidence, len(response.Evidence))
			for index, item := range response.Evidence {
				if item.End > len(prompt) {
					return nil, fmt.Errorf("input_resolver_failed: %s evidence range exceeds invocation prompt", definition.Name)
				}
				evidence[index] = InputEvidence{Source: RunInputSourceResolver, Location: "invocation_prompt", Start: item.Start, End: item.End, Kind: item.Kind}
			}
			assignments = append(assignments, RunInputAssignment{
				Name: definition.Name, RawValue: slices.Clone(response.Value), Source: RunInputSourceResolver,
				ResolverID: resolver.ID, ResolverVersion: response.ResolverVersion, Evidence: evidence,
			})
		}
	}
	return assignments, nil
}

func (c *Coordinator) resolveSemanticRunInputCandidate(ctx context.Context, prompt string, explicit json.RawMessage, definition RunInputDefinition, resolver *RunInputResolverSpec, schemaHash string) (RunInputAssignment, bool) {
	semanticResolver := c.semanticRunInputResolver()
	if semanticResolver == nil {
		return RunInputAssignment{}, false
	}
	resolverCtx := ctx
	var cancel context.CancelFunc
	if resolver.Timeout > 0 {
		resolverCtx, cancel = context.WithTimeout(ctx, time.Duration(resolver.Timeout)*time.Second)
	}
	raw, err := semanticResolver.Resolve(resolverCtx, SemanticRunInputRequest{
		InputName: definition.Name, Prompt: prompt, Schema: definition.Schema, SchemaHash: schemaHash,
		ResolverID: resolver.ID, ExplicitValue: slices.Clone(explicit),
	})
	if cancel != nil {
		cancel()
	}
	if err != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return RunInputAssignment{}, false
	}
	canonical, err := validateAndCanonicalizeRunInput(definition.Schema, raw)
	if err != nil {
		return RunInputAssignment{}, false
	}
	if semanticJSONContainsCommand(canonical) {
		return RunInputAssignment{}, false
	}
	return RunInputAssignment{
		Name: definition.Name, RawValue: canonical, Source: RunInputSourceResolver,
		ResolverID: resolver.ID, ResolverVersion: semanticRunInputResolverVersion,
		Evidence: []InputEvidence{{Source: RunInputSourceResolver, Location: "invocation_prompt", Kind: "semantic_json"}},
	}, true
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
