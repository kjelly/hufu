package team

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"slices"
	"strconv"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	internalmcp "github.com/kjelly/hufu/internal/mcp"
)

const (
	dynamicToolAuthorizationSnapshotVersion = 1
	logicalToolsetDigestVersion             = 1
	providerSurfaceDigestVersion            = 1
	maxFrozenDynamicTargets                 = 512
	maxDynamicTargetNameBytes               = 256
)

type FrozenDynamicToolTarget struct {
	Name             string `json:"name"`
	DescriptorSHA256 string `json:"descriptor_sha256"`
}

type DynamicToolAuthorizationSnapshot struct {
	Version             int                       `json:"version"`
	Targets             []FrozenDynamicToolTarget `json:"targets,omitempty"`
	FrozenCatalogDigest string                    `json:"frozen_catalog_digest"`
}

type DynamicToolTarget struct {
	Name             string
	Kind             string
	Server           string
	NativeName       string
	Description      string
	InputSchema      map[string]any
	Parameters       map[string]any
	Required         []string
	DescriptorSHA256 string
}

func cloneResolvedWorkerTools(src ResolvedWorkerTools) ResolvedWorkerTools {
	cloned := src
	cloned.Tools = slices.Clone(src.Tools)
	cloned.Names = slices.Clone(src.Names)
	cloned.AuthorizedNames = slices.Clone(src.AuthorizedNames)
	cloned.DynamicTargets = slices.Clone(src.DynamicTargets)
	for i := range cloned.DynamicTargets {
		cloned.DynamicTargets[i].InputSchema = cloneJSONMap(src.DynamicTargets[i].InputSchema)
		cloned.DynamicTargets[i].Parameters = cloneJSONMap(src.DynamicTargets[i].Parameters)
		cloned.DynamicTargets[i].Required = slices.Clone(src.DynamicTargets[i].Required)
	}
	cloned.Capabilities = slices.Clone(src.Capabilities)
	cloned.DynamicAuthorization = cloneDynamicToolAuthorizationSnapshot(src.DynamicAuthorization)
	return cloned
}

func cloneJSONMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	payload, err := json.Marshal(src)
	if err != nil {
		return nil
	}
	var cloned map[string]any
	if json.Unmarshal(payload, &cloned) != nil {
		return nil
	}
	return cloned
}

func cloneDynamicToolAuthorizationSnapshot(src *DynamicToolAuthorizationSnapshot) *DynamicToolAuthorizationSnapshot {
	if src == nil {
		return nil
	}
	cloned := *src
	cloned.Targets = slices.Clone(src.Targets)
	return &cloned
}

func validateDynamicToolAuthorizationSnapshot(snapshot *DynamicToolAuthorizationSnapshot) error {
	if snapshot == nil {
		return nil
	}
	if snapshot.Version != dynamicToolAuthorizationSnapshotVersion {
		return fmt.Errorf("dynamic_snapshot_invalid: unsupported version %d", snapshot.Version)
	}
	if len(snapshot.Targets) > maxFrozenDynamicTargets {
		return fmt.Errorf("dynamic_snapshot_invalid: %d targets exceeds limit %d", len(snapshot.Targets), maxFrozenDynamicTargets)
	}
	previous := ""
	for i, target := range snapshot.Targets {
		if target.Name == "" || len(target.Name) > maxDynamicTargetNameBytes {
			return fmt.Errorf("dynamic_snapshot_invalid: target %d has invalid name length", i)
		}
		if i > 0 && target.Name <= previous {
			return fmt.Errorf("dynamic_snapshot_invalid: target names are not strictly ascending")
		}
		if len(target.DescriptorSHA256) != sha256.Size*2 || strings.ToLower(target.DescriptorSHA256) != target.DescriptorSHA256 {
			return fmt.Errorf("dynamic_snapshot_invalid: target %q has invalid descriptor hash", target.Name)
		}
		if _, err := hex.DecodeString(target.DescriptorSHA256); err != nil {
			return fmt.Errorf("dynamic_snapshot_invalid: target %q has invalid descriptor hash", target.Name)
		}
		previous = target.Name
	}
	digest := frozenDynamicCatalogDigest(snapshot.Targets)
	if snapshot.FrozenCatalogDigest != digest {
		return fmt.Errorf("dynamic_snapshot_invalid: frozen catalog digest mismatch")
	}
	return nil
}

func frozenDynamicCatalogDigest(targets []FrozenDynamicToolTarget) string {
	hasher := sha256.New()
	writeDigestRecord(hasher, "dynamic_catalog_version", strconv.Itoa(dynamicToolAuthorizationSnapshotVersion))
	for _, target := range targets {
		writeDigestRecord(hasher, "target", target.Name, target.DescriptorSHA256)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

func writeDigestRecord(writer hash.Hash, parts ...string) {
	var size [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = writer.Write(size[:])
		_, _ = writer.Write([]byte(part))
	}
}

func (c *Coordinator) resolveNewTaskToolAuthorization(_ context.Context, task TaskDef, def *agent.AgentDef) (*DynamicToolAuthorizationSnapshot, error) {
	if c == nil || def == nil {
		return nil, fmt.Errorf("resolve new task tool authorization: agent definition is required")
	}
	descriptors := c.managerMCPDescriptors()
	baseNames := agentToolNames(c.selectWorkerToolsForTask(def, task))
	names := make([]string, len(descriptors))
	for i := range descriptors {
		names[i] = descriptors[i].Name
	}
	modes := []WorkerToolResolutionMode{workerToolResolutionModeForTask(task)}
	if task.PlanFirst {
		if len(task.Execution.ToolSequence) > 0 {
			// The established plan-first/closed-sequence admission gate owns
			// this invalid combination and its stable diagnostic. No occurrence
			// will be created, so avoid changing observable rejection behavior.
			return newEmptyDynamicToolAuthorizationSnapshot(), nil
		}
		modes = []WorkerToolResolutionMode{WorkerToolResolutionInitialPlan, WorkerToolResolutionApprovedPlan}
	}
	authorized := make(map[string]bool, len(names))
	phase := PhaseExecute
	workflowEnabled := c.phaseWorkflow != nil && c.phaseWorkflow.Enabled()
	if workflowEnabled {
		phase = c.phaseWorkflow.State()
	}
	for _, mode := range modes {
		resolved, err := ResolveStaticWorkerTools(StaticToolResolutionInput{
			Session: c.session, Agent: def, Task: task, LifecycleMode: mode,
			Policy:    EffectiveTeamContractContext{NoNet: c.noNet || def.NoNet, ForceMCP: c.forceMCP || def.ForceMCP},
			BaseTools: baseNames, SupplementalTools: names, TrustedTaskGrants: c.taskToolGrants(def, task),
			WorkflowEnabled: workflowEnabled, WorkflowPhase: phase,
		})
		if err != nil {
			return nil, fmt.Errorf("resolve new task tool authorization: %w", err)
		}
		for _, name := range resolved.Names {
			authorized[name] = true
		}
	}
	targets := make([]FrozenDynamicToolTarget, 0, len(descriptors))
	for _, descriptor := range descriptors {
		if !authorized[descriptor.Name] {
			continue
		}
		fingerprint, err := internalmcp.MCPToolDescriptorSHA256(descriptor)
		if err != nil {
			return nil, fmt.Errorf("resolve new task tool authorization: target %q: %w", descriptor.Name, err)
		}
		targets = append(targets, FrozenDynamicToolTarget{Name: descriptor.Name, DescriptorSHA256: fingerprint})
	}
	if len(targets) > maxFrozenDynamicTargets {
		return nil, fmt.Errorf("dynamic_snapshot_invalid: %d targets exceeds limit %d", len(targets), maxFrozenDynamicTargets)
	}
	snapshot := &DynamicToolAuthorizationSnapshot{Version: dynamicToolAuthorizationSnapshotVersion, Targets: targets}
	snapshot.FrozenCatalogDigest = frozenDynamicCatalogDigest(snapshot.Targets)
	if err := validateDynamicToolAuthorizationSnapshot(snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func newEmptyDynamicToolAuthorizationSnapshot() *DynamicToolAuthorizationSnapshot {
	snapshot := &DynamicToolAuthorizationSnapshot{Version: dynamicToolAuthorizationSnapshotVersion}
	snapshot.FrozenCatalogDigest = frozenDynamicCatalogDigest(nil)
	return snapshot
}

func (c *Coordinator) freezeTodoSpecDynamicAuthorization(ctx context.Context, task TaskDef, def *agent.AgentDef, spec *TodoSpec) error {
	if spec == nil {
		return fmt.Errorf("freeze dynamic tool authorization: Todo spec is required")
	}
	snapshot, err := c.resolveNewTaskToolAuthorization(ctx, task, def)
	if err != nil {
		return err
	}
	spec.DynamicToolAuthorization = cloneDynamicToolAuthorizationSnapshot(snapshot)
	return nil
}

func (c *Coordinator) managerMCPDescriptors() []internalmcp.MCPTool {
	if c == nil || c.mcpManager == nil {
		return nil
	}
	return c.mcpManager.SnapshotToolDescriptors()
}

func frozenDynamicTargetMap(snapshot *DynamicToolAuthorizationSnapshot) map[string]string {
	result := make(map[string]string)
	if snapshot == nil {
		return result
	}
	for _, target := range snapshot.Targets {
		result[target.Name] = target.DescriptorSHA256
	}
	return result
}

type dynamicToolUnavailable struct {
	Target string
	Reason string
}

func filterManagerToolsThroughSnapshot(snapshot *DynamicToolAuthorizationSnapshot, descriptors []internalmcp.MCPTool, tools []fantasy.AgentTool, sequence []string) ([]fantasy.AgentTool, []dynamicToolUnavailable, error) {
	if snapshot == nil {
		return tools, nil, nil
	}
	if err := validateDynamicToolAuthorizationSnapshot(snapshot); err != nil {
		return nil, nil, err
	}
	frozen := frozenDynamicTargetMap(snapshot)
	live := make(map[string]string, len(descriptors))
	for _, descriptor := range descriptors {
		fingerprint, err := internalmcp.MCPToolDescriptorSHA256(descriptor)
		if err != nil {
			return nil, nil, fmt.Errorf("bind frozen task tools: target %q: %w", descriptor.Name, err)
		}
		live[descriptor.Name] = fingerprint
	}
	filtered := make([]fantasy.AgentTool, 0, len(tools))
	available := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		name := tool.Info().Name
		if fingerprint, ok := frozen[name]; ok && live[name] == fingerprint {
			filtered = append(filtered, tool)
			available[name] = true
		}
	}
	for _, name := range sequence {
		if _, frozenTarget := frozen[name]; frozenTarget && !available[name] {
			return nil, nil, fmt.Errorf("bind frozen task tools: required dynamic target %q is unavailable", name)
		}
	}
	unavailable := make([]dynamicToolUnavailable, 0)
	for _, target := range snapshot.Targets {
		if available[target.Name] {
			continue
		}
		reason := "missing"
		if fingerprint, exists := live[target.Name]; exists && fingerprint != target.DescriptorSHA256 {
			reason = "descriptor_changed"
		}
		unavailable = append(unavailable, dynamicToolUnavailable{Target: target.Name, Reason: reason})
	}
	return filtered, unavailable, nil
}

func (c *Coordinator) recordDynamicToolUnavailable(ctx context.Context, todo *TodoItem, entries []dynamicToolUnavailable) error {
	if c == nil || todo == nil || len(entries) == 0 {
		return nil
	}
	for _, entry := range entries {
		payload := struct {
			Type               string `json:"type"`
			TaskID             string `json:"task_id"`
			OccurrenceRevision int    `json:"occurrence_revision"`
			Target             string `json:"target"`
			Reason             string `json:"reason"`
		}{
			Type: string(EventDynamicToolUnavailable), TaskID: todo.ID,
			OccurrenceRevision: max(1, todo.OccurrenceRevision), Target: entry.Target, Reason: entry.Reason,
		}
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if c.hasDurableEventJournal() {
			key := fmt.Sprintf("dynamic-tool-unavailable:%s:%d:%s:%s", todo.ID, payload.OccurrenceRevision, entry.Target, entry.Reason)
			if _, err := c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{
				Type: string(EventDynamicToolUnavailable), Actor: "runtime", TaskID: todo.ID,
				IdempotencyKey: key, Payload: raw,
			}); err != nil {
				return fmt.Errorf("record unavailable dynamic tool %q: %w", entry.Target, err)
			}
		}
		c.report(c.newEvent(string(EventDynamicToolUnavailable)).withTodoID(todo.ID).withMessage(entry.Target + ": " + entry.Reason))
	}
	return nil
}

type providerToolSchemaV1 struct {
	Version     int            `json:"version"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
	Required    []string       `json:"required"`
}

func providerToolSchemaSHA256(info fantasy.ToolInfo) (string, error) {
	required := slices.Clone(info.Required)
	slices.Sort(required)
	payload, err := json.Marshal(providerToolSchemaV1{
		Version: providerSurfaceDigestVersion, Name: info.Name, Description: info.Description,
		Parameters: info.Parameters, Required: required,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func workerToolDigests(c *Coordinator, def *agent.AgentDef, task TaskDef, mode WorkerToolResolutionMode, phase Phase, tools []fantasy.AgentTool, snapshot *DynamicToolAuthorizationSnapshot, effectiveSequence []string) (string, string, error) {
	type namedFingerprint struct{ name, fingerprint string }
	direct := make([]namedFingerprint, 0, len(tools))
	for _, tool := range tools {
		fingerprint, err := providerToolSchemaSHA256(tool.Info())
		if err != nil {
			return "", "", fmt.Errorf("fingerprint provider tool %q: %w", tool.Info().Name, err)
		}
		direct = append(direct, namedFingerprint{name: tool.Info().Name, fingerprint: fingerprint})
	}
	slices.SortFunc(direct, func(a, b namedFingerprint) int { return strings.Compare(a.name, b.name) })
	providerHasher := sha256.New()
	writeDigestRecord(providerHasher, "provider_surface_digest_version", strconv.Itoa(providerSurfaceDigestVersion))
	for _, entry := range direct {
		writeDigestRecord(providerHasher, "direct", entry.name, entry.fingerprint)
	}
	logicalHasher := sha256.New()
	writeDigestRecord(logicalHasher, "logical_toolset_digest_version", strconv.Itoa(logicalToolsetDigestVersion))
	frozenDigest := "legacy"
	if snapshot != nil {
		frozenDigest = snapshot.FrozenCatalogDigest
	}
	writeDigestRecord(logicalHasher, "frozen_catalog_digest", frozenDigest)
	for _, entry := range direct {
		writeDigestRecord(logicalHasher, "direct", entry.name, entry.fingerprint)
	}
	if snapshot != nil {
		for _, target := range snapshot.Targets {
			writeDigestRecord(logicalHasher, "frozen_dynamic", target.Name, target.DescriptorSHA256)
			if slices.ContainsFunc(direct, func(entry namedFingerprint) bool { return entry.name == target.Name }) {
				writeDigestRecord(logicalHasher, "executable_dynamic", target.Name, target.DescriptorSHA256)
			}
		}
	}
	writeDigestRecord(logicalHasher, "lifecycle_mode", string(mode))
	writeDigestRecord(logicalHasher, "workflow_phase", string(phase))
	writeDigestRecord(logicalHasher, "side_effect", string(task.SideEffect))
	for i, name := range effectiveSequence {
		writeDigestRecord(logicalHasher, "sequence", strconv.Itoa(i), name)
	}
	writeDigestRecord(logicalHasher, "no_net", strconv.FormatBool(c.noNet || def.NoNet))
	writeDigestRecord(logicalHasher, "force_mcp", strconv.FormatBool(c.forceMCP || def.ForceMCP))
	if c.session != nil {
		for _, name := range sortedUniqueToolNames(c.session.Config.ToolsDenied) {
			writeDigestRecord(logicalHasher, "team_deny", name)
		}
	}
	grants := c.taskToolGrants(def, task)
	grantNames := make([]string, 0, len(grants))
	for name, granted := range grants {
		if granted {
			grantNames = append(grantNames, name)
		}
	}
	slices.Sort(grantNames)
	for _, name := range grantNames {
		writeDigestRecord(logicalHasher, "trusted_task_grant", name)
	}
	return hex.EncodeToString(providerHasher.Sum(nil)), hex.EncodeToString(logicalHasher.Sum(nil)), nil
}

func sortedUniqueToolNames(names []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			result = append(result, name)
		}
	}
	slices.Sort(result)
	return slices.Compact(result)
}
