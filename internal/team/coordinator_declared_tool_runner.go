package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

// coordinatorDeclaredToolRunner is the default adapter from a typed
// ExecutionStep to Hufu's ordinary worker tools. It executes exactly the
// declared JSON input without asking a model to reconstruct or rewrite it.
// Integrations that need a generative producer/repairer may replace it through
// SetStructuredStepRunner while retaining the same lifecycle and receipts.
type coordinatorDeclaredToolRunner struct {
	c *Coordinator
}

func (r *coordinatorDeclaredToolRunner) RunStructuredStep(ctx context.Context, request StructuredStepRequest) (ExecutionStepResult, error) {
	if r == nil || r.c == nil {
		return ExecutionStepResult{}, fmt.Errorf("structured declared-tool runner has no coordinator")
	}
	item := r.c.todoItemByID(request.TaskID)
	if item == nil {
		return ExecutionStepResult{}, fmt.Errorf("structured task %q does not exist", request.TaskID)
	}
	agentDef, _, err := r.c.AgentPool().ResolveAgentName(item.Agent)
	if err != nil {
		return ExecutionStepResult{}, fmt.Errorf("resolve structured task agent %q: %w", item.Agent, err)
	}
	if agentDef == nil {
		return ExecutionStepResult{}, fmt.Errorf("resolve structured task agent %q: definition is nil", item.Agent)
	}
	agentTools := r.c.selectWorkerTools(agentDef)
	mcpAllowed := r.c.phaseWorkflow == nil || !r.c.phaseWorkflow.Enabled() || r.c.phaseWorkflow.State() == PhaseExecute
	if r.c.mcpManager != nil && mcpAllowed {
		agentTools = append(agentTools, r.c.mcpManager.AsAgentTools()...)
		if len(agentDef.MCPTools) > 0 {
			agentTools = append(agentTools, r.c.mcpManager.GetAgentMCPTools(agentDef.Name, agentDef.Shell)...)
		}
	}
	agentTools = r.c.filterDeniedWorkerTools(agentTools)
	var selected fantasy.AgentTool
	for _, candidate := range agentTools {
		if candidate != nil && candidate.Info().Name == request.Step.Tool {
			selected = candidate
			break
		}
	}
	if selected == nil {
		return ExecutionStepResult{}, fmt.Errorf("structured step %q requires unavailable tool %q for agent %q", request.Step.ID, request.Step.Tool, agentDef.Name)
	}

	exposed := agentToolNames(agentTools)
	stepCtx := context.WithValue(ctx, todoIDKey{}, request.TaskID)
	stepCtx = context.WithValue(stepCtx, executionAttemptKey{}, request.Attempt)
	stepCtx = context.WithValue(stepCtx, tools.AgentNameKey, strings.ToLower(agentDef.Name))
	stepCtx = tools.SetSSHSessionManager(stepCtx, r.c.sshSessionMgr)
	stepCtx = r.c.withEffectiveToolsAllowed(stepCtx, agentDef, exposed)
	artifactScope, scopeErr := r.c.buildArtifactAccessScope(request.TaskID, request.Attempt)
	if scopeErr != nil {
		return ExecutionStepResult{}, fmt.Errorf("artifact scope preflight failed: %w", scopeErr)
	}
	if artifactScope != nil {
		stepCtx = context.WithValue(stepCtx, artifactAccessScopeKey, cloneArtifactAccessScope(artifactScope))
		stepCtx = context.WithValue(stepCtx, tools.ArtifactPathPolicyKey, tools.ArtifactPathPolicy{
			BlockedPaths:             r.c.artifactScopePathCandidates(artifactScope),
			FailClosedForUnsupported: item.WorksetBinding != nil,
		})
	}
	if len(agentDef.Guard) > 0 {
		stepCtx = context.WithValue(stepCtx, tools.GuardRulesKey, agentDef.Guard)
	}
	if allowedPaths := r.c.runtimeAllowedPaths(agentDef.AllowedPaths); len(allowedPaths) > 0 {
		stepCtx = context.WithValue(stepCtx, tools.AgentAllowedPathsKey, allowedPaths)
	}
	if strings.EqualFold(strings.TrimSpace(agentDef.SideEffect), string(SideEffectNone)) {
		stepCtx = context.WithValue(stepCtx, tools.AgentReadOnlyExecutionKey, true)
	}
	if writePaths := r.c.runtimeAllowedWritePaths(); len(writePaths) > 0 {
		stepCtx = context.WithValue(stepCtx, tools.AgentAllowedWritePathsKey, writePaths)
	}
	if agentDef.RestrictedPath != "" {
		stepCtx = context.WithValue(stepCtx, tools.AgentRestrictedPathKey, agentDef.RestrictedPath)
	}
	if r.c.noNet || agentDef.NoNet {
		stepCtx = context.WithValue(stepCtx, tools.AgentNetworkBlockKey, true)
	}
	if r.c.forceMCP || agentDef.ForceMCP {
		stepCtx = context.WithValue(stepCtx, tools.AgentForceMCPKey, true)
	}
	if r.c.unattended {
		stepCtx = context.WithValue(stepCtx, tools.UnattendedKey, true)
	}
	if r.c.autoApprove {
		stepCtx = context.WithValue(stepCtx, tools.AutoApproveKey, true)
	}
	r.c.sessionToolPermissionsMu.RLock()
	sessionPerms := make(map[string]bool, len(r.c.sessionToolPermissions))
	for name, allowed := range r.c.sessionToolPermissions {
		sessionPerms[name] = allowed
	}
	r.c.sessionToolPermissionsMu.RUnlock()
	stepCtx = context.WithValue(stepCtx, tools.AgentToolsSessionPermissionsKey, sessionPerms)
	stepCtx = context.WithValue(stepCtx, tools.ToolPermissionCallbackKey, tools.ToolPermissionCallback(func(name string, allowed bool) {
		r.c.sessionToolPermissionsMu.Lock()
		r.c.sessionToolPermissions[name] = allowed
		r.c.sessionToolPermissionsMu.Unlock()
	}))

	input, err := json.Marshal(request.ResolvedInput)
	if err != nil {
		return ExecutionStepResult{}, fmt.Errorf("marshal structured step %q input: %w", request.Step.ID, err)
	}
	callID := structuredReceiptID(request.TaskID, request.Attempt, request.Step.ID, request.RepairAttempt, 0) + "-tool"
	response, runErr := (&policyGatedTool{inner: selected, coordinator: r.c}).Run(stepCtx, fantasy.ToolCall{
		ID: callID, Name: request.Step.Tool, Input: string(input),
	})
	result := ExecutionStepResult{Stdout: response.Content, PolicyVerdict: r.c.takeToolPolicyVerdict(callID)}
	if exitCode, ok := transcriptExitCode(request.Step.Tool, response.Content); ok {
		result.ExitCode = exitCode
	}
	if response.IsError || runErr != nil {
		if result.ExitCode == 0 {
			result.ExitCode = 1
		}
		result.Stderr = response.Content
		result.Stdout = ""
	}
	if runErr != nil {
		if result.Stderr == "" {
			result.Stderr = runErr.Error()
		}
		return result, runErr
	}
	if result.ExitCode != 0 {
		return result, nil
	}
	if err := r.collectDeclaredOutputs(request, response, &result); err != nil {
		return result, err
	}
	return result, nil
}

func (r *coordinatorDeclaredToolRunner) collectDeclaredOutputs(request StructuredStepRequest, response fantasy.ToolResponse, result *ExecutionStepResult) error {
	var decoded map[string]any
	_ = json.Unmarshal([]byte(response.Content), &decoded)
	factOutputs := 0
	for _, output := range request.Step.Outputs {
		if normalizedExecutionOutputKind(output.Kind) == ExecutionOutputFact {
			factOutputs++
		}
	}
	for _, output := range request.Step.Outputs {
		switch normalizedExecutionOutputKind(output.Kind) {
		case ExecutionOutputArtifact:
			path := strings.TrimSpace(output.Path)
			if path == "" {
				path, _ = request.ResolvedInput["path"].(string)
			}
			artifact, err := r.fileArtifactRef(request, output.Name, path)
			if err != nil {
				return err
			}
			if result.Artifacts == nil {
				result.Artifacts = make(map[string]ArtifactRef)
			}
			result.Artifacts[output.Name] = artifact
		case ExecutionOutputFact:
			value, ok := decoded[output.Name]
			if !ok && factOutputs == 1 {
				value, ok = response.Content, true
			}
			if !ok {
				return fmt.Errorf("structured step %q response omits fact output %q", request.Step.ID, output.Name)
			}
			if result.Facts == nil {
				result.Facts = make(map[string]any)
			}
			result.Facts[output.Name] = value
		}
	}
	return nil
}

func (r *coordinatorDeclaredToolRunner) fileArtifactRef(request StructuredStepRequest, name, path string) (ArtifactRef, error) {
	if strings.TrimSpace(path) == "" {
		return ArtifactRef{}, fmt.Errorf("structured artifact output %q must declare path or use input.path", name)
	}
	item := r.c.todoItemByID(request.TaskID)
	if item == nil {
		return ArtifactRef{}, fmt.Errorf("structured task %q does not exist", request.TaskID)
	}
	base := r.c.projectDir
	if strings.TrimSpace(base) == "" && r.c.session != nil {
		base = r.c.session.Workspace
	}
	workspace := base
	if r.c.session != nil && strings.TrimSpace(r.c.session.Workspace) != "" {
		workspace = r.c.session.Workspace
	}
	store, err := NewFileArtifactStore(workspace, base)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("open structured artifact store: %w", err)
	}
	putResult, err := store.Put(context.Background(), PutArtifactRequest{
		Kind:       string(ExecutionOutputArtifact),
		Path:       path,
		SourcePath: path,
		RunID:      r.c.contextRunID(),
		TaskID:     request.TaskID,
		Attempt:    request.Attempt,
		Agent:      item.Agent,
	})
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("snapshot structured artifact %q: %w", name, err)
	}
	return putResult.ArtifactRef, nil
}

// InspectStructuredArtifact re-hashes workspace-backed artifacts immediately
// before mutation.
func (r *coordinatorDeclaredToolRunner) InspectStructuredArtifact(ctx context.Context, artifact ArtifactRef) (ArtifactRef, error) {
	if artifact.ID != "" && r.c.session != nil && strings.TrimSpace(r.c.session.Workspace) != "" {
		store, err := NewFileArtifactStore(r.c.session.Workspace, r.c.session.Workspace)
		if err != nil {
			return ArtifactRef{}, err
		}
		if err := store.Verify(ctx, artifact); err == nil {
			return artifact, nil
		} else if strings.HasPrefix(artifact.ID, "sha256-") {
			return ArtifactRef{}, fmt.Errorf("content-addressed artifact %q failed integrity verification: %w", artifact.ID, err)
		}
	}
	base := r.c.projectDir
	if strings.TrimSpace(base) == "" && r.c.session != nil {
		base = r.c.session.Workspace
	}
	return inspectFileArtifact(base, artifact)
}

func inspectFileArtifact(base string, artifact ArtifactRef) (ArtifactRef, error) {
	resolved, err := resolveArtifactSourcePath(base, artifact.Path)
	if err != nil {
		return ArtifactRef{}, err
	}
	file, err := os.Open(resolved)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("open structured artifact %q: %w", artifact.Path, err)
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("hash structured artifact %q: %w", artifact.Path, err)
	}
	artifact.SHA256 = hex.EncodeToString(hash.Sum(nil))
	artifact.Bytes = size
	return artifact, nil
}

func (c *Coordinator) todoItemByID(todoID string) *TodoItem {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == todoID {
			return item
		}
	}
	return nil
}
