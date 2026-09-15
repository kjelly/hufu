package team

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/tools"
)

const taskResourceScopeSnapshotVersion = 1

const (
	resourceScopeSourceAuthored        = "authored"
	resourceScopeSourceAuthoredWorkset = "authored+workset"
	resourceScopeSourceRuntimeFallback = "runtime_fallback"
)

type resourceClaimsReportedContextKey struct{}

func withResourceClaimsReported(ctx context.Context) context.Context {
	return context.WithValue(ctx, resourceClaimsReportedContextKey{}, true)
}

func resourceClaimsReported(ctx context.Context) bool {
	return ctx != nil && ctx.Value(resourceClaimsReportedContextKey{}) != nil
}

type EffectiveTaskResourceScope struct {
	Claims            []ResourceClaim
	AllowedReadPaths  []string
	AllowedWritePaths []string
	BoundedReadScope  bool
	BoundedWriteScope bool
	Source            string
}

type TaskResourceScopeSnapshot struct {
	Version           int             `json:"version"`
	Claims            []ResourceClaim `json:"claims"`
	ReadPaths         []string        `json:"read_paths,omitempty"`
	WritePaths        []string        `json:"write_paths,omitempty"`
	BoundedReadScope  bool            `json:"bounded_read_scope,omitempty"`
	BoundedWriteScope bool            `json:"bounded_write_scope,omitempty"`
	Source            string          `json:"source"`
	Digest            string          `json:"digest"`
}

func cloneTaskResourceScopeSnapshot(src *TaskResourceScopeSnapshot) *TaskResourceScopeSnapshot {
	if src == nil {
		return nil
	}
	cloned := *src
	cloned.Claims = slices.Clone(src.Claims)
	cloned.ReadPaths = slices.Clone(src.ReadPaths)
	cloned.WritePaths = slices.Clone(src.WritePaths)
	return &cloned
}

func authoredResourceClaims(task TaskDef) []ResourceClaim {
	return resourceClaims(task)
}

func canonicalScopePaths(paths []string) ([]string, error) {
	result := make([]string, 0, len(paths))
	for _, raw := range paths {
		path, err := NormalizeTouchedPath(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, path)
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func taskResourceScopeDigest(snapshot *TaskResourceScopeSnapshot) string {
	if snapshot == nil {
		return ""
	}
	hasher := newSHA256Hasher()
	writeDigestRecord(hasher, "resource_scope_version", strconv.Itoa(snapshot.Version))
	writeDigestRecord(hasher, "source", snapshot.Source)
	writeDigestRecord(hasher, "bounded_read_scope", strconv.FormatBool(snapshot.BoundedReadScope))
	writeDigestRecord(hasher, "bounded_write_scope", strconv.FormatBool(snapshot.BoundedWriteScope))
	for _, claim := range snapshot.Claims {
		writeDigestRecord(hasher, "claim", claim.Resource, string(claim.Mode))
	}
	for _, path := range snapshot.ReadPaths {
		writeDigestRecord(hasher, "read_path", path)
	}
	for _, path := range snapshot.WritePaths {
		writeDigestRecord(hasher, "write_path", path)
	}
	return encodeHash(hasher)
}

func validateTaskResourceScopeSnapshot(snapshot *TaskResourceScopeSnapshot, sideEffect SideEffectClass) error {
	if snapshot == nil {
		return nil
	}
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("resource_snapshot_invalid: "+format, args...)
	}
	if snapshot.Version != taskResourceScopeSnapshotVersion {
		return invalid("unsupported version %d", snapshot.Version)
	}
	switch snapshot.Source {
	case resourceScopeSourceAuthored, resourceScopeSourceAuthoredWorkset, resourceScopeSourceRuntimeFallback:
	default:
		return invalid("unsupported source %q", snapshot.Source)
	}
	claims, err := normalizeResourceClaims(snapshot.Claims)
	if err != nil {
		return invalid("claim: %v", err)
	}
	if !slices.Equal(claims, snapshot.Claims) {
		return invalid("claims are not canonical, sorted, and unique")
	}
	readPaths, err := canonicalScopePaths(snapshot.ReadPaths)
	if err != nil || !slices.Equal(readPaths, snapshot.ReadPaths) {
		return invalid("read paths are not canonical, sorted, and unique")
	}
	writePaths, err := canonicalScopePaths(snapshot.WritePaths)
	if err != nil || !slices.Equal(writePaths, snapshot.WritePaths) {
		return invalid("write paths are not canonical, sorted, and unique")
	}
	if snapshot.BoundedReadScope != (len(snapshot.ReadPaths) > 0) {
		return invalid("bounded read flag does not match read paths")
	}
	if snapshot.BoundedWriteScope != (len(snapshot.WritePaths) > 0) {
		return invalid("bounded write flag does not match write paths")
	}
	if snapshot.BoundedWriteScope && !snapshot.BoundedReadScope {
		return invalid("bounded write scope requires bounded read scope")
	}
	if snapshot.BoundedReadScope && snapshot.Source != resourceScopeSourceAuthoredWorkset {
		return invalid("bounded scopes require authored+workset source")
	}
	if snapshot.BoundedWriteScope && sideEffect != SideEffectWorkspaceWrite {
		return invalid("bounded write scope is incompatible with side effect %q", sideEffect)
	}
	for _, readPath := range snapshot.ReadPaths {
		claim, claimErr := NewWorkspacePathResourceClaim(strings.TrimSuffix(readPath, "/"), ResourceRead)
		if claimErr != nil || !resourceClaimCovered(claim, snapshot.Claims) {
			return invalid("read path %q has no matching read claim", readPath)
		}
	}
	for _, writePath := range snapshot.WritePaths {
		if !scopePathCovered(writePath, snapshot.ReadPaths) {
			return invalid("write path %q is not covered by read paths", writePath)
		}
		claim, claimErr := NewWorkspacePathResourceClaim(strings.TrimSuffix(writePath, "/"), ResourceWrite)
		if claimErr != nil || !resourceClaimCovered(claim, snapshot.Claims) {
			return invalid("write path %q has no matching write claim", writePath)
		}
	}
	if !snapshot.BoundedReadScope {
		mode := ResourceExclusive
		if sideEffect == "" || sideEffect == SideEffectNone {
			mode = ResourceRead
		}
		root, _ := NewWorkspacePathResourceClaim(".", mode)
		if !resourceClaimCovered(root, snapshot.Claims) {
			return invalid("unbounded scope is missing whole-root %s claim", mode)
		}
	}
	if snapshot.Digest == "" || snapshot.Digest != taskResourceScopeDigest(snapshot) {
		return invalid("digest mismatch")
	}
	return nil
}

func resourceClaimCovered(want ResourceClaim, claims []ResourceClaim) bool {
	for _, claim := range claims {
		if claim.Resource == want.Resource && resourceClaimModeStrength(claim.Mode) >= resourceClaimModeStrength(want.Mode) {
			return true
		}
	}
	return false
}

func scopePathCovered(requested string, ceilings []string) bool {
	requested = strings.TrimSuffix(requested, "/")
	for _, ceiling := range ceilings {
		ceiling = strings.TrimSuffix(ceiling, "/")
		if ceiling == "." || requested == ceiling || strings.HasPrefix(requested, ceiling+"/") {
			return true
		}
	}
	return false
}

func (c *Coordinator) resolveNewTaskResourceScope(task TaskDef, todo *TodoItem, resolvedTools ResolvedWorkerTools) (*TaskResourceScopeSnapshot, error) {
	if todo == nil {
		return nil, fmt.Errorf("resolve new task resource scope: Todo is required")
	}
	claims, err := normalizeResourceClaims(authoredResourceClaims(task))
	if err != nil {
		return nil, fmt.Errorf("invalid_resource_claim: %w", err)
	}
	effect := todo.SideEffect
	if effect == "" {
		effect = SideEffectNone
	}
	if (effect == SideEffectNone || effect == SideEffectWorkspaceWrite) && todo.WorksetBinding != nil && len(todo.WorksetBinding.TouchedPaths) > 0 {
		eligible, eligibilityErr := c.taskScopeEligible(task, resolvedTools)
		if eligibilityErr != nil {
			return nil, eligibilityErr
		}
		if eligible {
			return c.resolveBoundedTaskResourceScope(task, todo, claims, effect)
		}
	}
	return wholeRootTaskResourceScope(claims, effect)
}

func wholeRootTaskResourceScope(claims []ResourceClaim, effect SideEffectClass) (*TaskResourceScopeSnapshot, error) {
	mode := ResourceExclusive
	if effect == SideEffectNone {
		mode = ResourceRead
	}
	root, err := NewWorkspacePathResourceClaim(".", mode)
	if err != nil {
		return nil, err
	}
	claims, err = normalizeResourceClaims(append(claims, root))
	if err != nil {
		return nil, err
	}
	snapshot := &TaskResourceScopeSnapshot{
		Version: taskResourceScopeSnapshotVersion, Claims: claims,
		Source: resourceScopeSourceRuntimeFallback,
	}
	snapshot.Digest = taskResourceScopeDigest(snapshot)
	if err := validateTaskResourceScopeSnapshot(snapshot, effect); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (c *Coordinator) taskScopeEligible(task TaskDef, resolved ResolvedWorkerTools) (bool, error) {
	if task.Action != nil || len(task.Execution.Steps) > 0 || task.ResolvedExecutionTarget.IsZero() {
		return false, nil
	}
	backend, err := c.ExecutionRegistry().ResolveBackend(task.ResolvedExecutionTarget.Backend)
	if err != nil || backend.Kind() != execution.BackendKindLLM {
		return false, nil
	}
	closed := make(map[string]bool, len(task.Execution.ToolSequence))
	for _, name := range task.Execution.ToolSequence {
		closed[name] = true
	}
	for _, name := range resolved.AuthorizedNames {
		descriptor, ok := resolved.WorkspaceScopeDescriptors[name]
		if !ok {
			return false, nil
		}
		if descriptor.MayReadWorkspace && descriptor.ReadBehavior != tools.PathScopeEnforced {
			if closed[name] && descriptor.ReadBehavior == tools.PathScopeDenied {
				return false, fmt.Errorf("unsupported_closed_sequence_scope: required tool %q cannot read under a bounded scope", name)
			}
			if descriptor.ReadBehavior != tools.PathScopeDenied {
				return false, nil
			}
		}
		if descriptor.MayWriteWorkspace && descriptor.WriteBehavior != tools.PathScopeEnforced {
			if closed[name] && descriptor.WriteBehavior == tools.PathScopeDenied {
				return false, fmt.Errorf("unsupported_closed_sequence_scope: required tool %q cannot write under a bounded scope", name)
			}
			if descriptor.WriteBehavior != tools.PathScopeDenied {
				return false, nil
			}
		}
	}
	return true, nil
}

func (c *Coordinator) resolveBoundedTaskResourceScope(task TaskDef, todo *TodoItem, authored []ResourceClaim, effect SideEffectClass) (*TaskResourceScopeSnapshot, error) {
	paths, err := canonicalScopePaths(todo.WorksetBinding.TouchedPaths)
	if err != nil {
		return nil, fmt.Errorf("invalid_write_scope: %w", err)
	}
	root, err := c.taskMutableRoot()
	if err != nil {
		return nil, fmt.Errorf("resource_scope_unreproducible: %w", err)
	}
	absRead, err := absoluteTaskScopePaths(root, paths)
	if err != nil {
		return nil, err
	}
	def, _, err := c.AgentPool().ResolveAgentName(todo.Agent)
	if err != nil {
		return nil, err
	}
	configured := append(slices.Clone(c.allowedPaths), c.runtimeAllowedPaths(def.AllowedPaths)...)
	if _, err := IntersectReadPathScopes(configured, absRead); err != nil {
		return nil, err
	}
	mode := ResourceRead
	writePaths := []string(nil)
	if effect == SideEffectWorkspaceWrite {
		if c.taskScopeOverlapsControlWorkspace(root, paths) {
			return nil, fmt.Errorf("invalid_write_scope: bounded task scope overlaps coordinator-owned workspace")
		}
		if _, err := IntersectPathScopeCeilings(absRead, configured, c.runtimeAllowedWritePaths()); err != nil {
			return nil, err
		}
		mode = ResourceWrite
		writePaths = slices.Clone(paths)
	}
	claims := slices.Clone(authored)
	for _, path := range paths {
		claim, err := NewWorkspacePathResourceClaim(strings.TrimSuffix(path, "/"), mode)
		if err != nil {
			return nil, err
		}
		claims = append(claims, claim)
	}
	claims, err = normalizeResourceClaims(claims)
	if err != nil {
		return nil, err
	}
	snapshot := &TaskResourceScopeSnapshot{
		Version: taskResourceScopeSnapshotVersion, Claims: claims, ReadPaths: paths, WritePaths: writePaths,
		BoundedReadScope: true, BoundedWriteScope: effect == SideEffectWorkspaceWrite,
		Source: resourceScopeSourceAuthoredWorkset,
	}
	snapshot.Digest = taskResourceScopeDigest(snapshot)
	if err := validateTaskResourceScopeSnapshot(snapshot, effect); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func absoluteTaskScopePaths(root string, relative []string) ([]string, error) {
	root, err := canonicalMutableRoot(root)
	if err != nil {
		return nil, fmt.Errorf("resource_scope_unreproducible: %w", err)
	}
	result := make([]string, len(relative))
	for i, path := range relative {
		directory := strings.HasSuffix(path, "/")
		result[i] = filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(path, "/")))
		if directory {
			result[i] += string(filepath.Separator)
		}
	}
	return result, nil
}

func (c *Coordinator) taskScopeOverlapsControlWorkspace(root string, paths []string) bool {
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return false
	}
	root, rootErr := canonicalMutableRoot(root)
	control, controlErr := canonicalMutableRoot(c.session.Workspace)
	if rootErr != nil || controlErr != nil {
		return true
	}
	for _, path := range paths {
		directory := strings.HasSuffix(path, "/") || path == "."
		candidate := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(path, "/")))
		if candidate == control {
			return true
		}
		if directory {
			rel, err := filepath.Rel(candidate, control)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

func (c *Coordinator) bindFrozenTaskResourceScope(task TaskDef, todo *TodoItem, resolvedTools ResolvedWorkerTools, mutableRoot string) (EffectiveTaskResourceScope, error) {
	if todo == nil {
		return EffectiveTaskResourceScope{}, fmt.Errorf("resource_scope_unreproducible: Todo is required")
	}
	if todo.ResourceScopeSnapshot == nil {
		legacy, err := c.resolveNewTaskResourceScope(task, todo, ResolvedWorkerTools{})
		if err != nil {
			return EffectiveTaskResourceScope{}, err
		}
		return effectiveScopeFromSnapshot(legacy, mutableRoot)
	}
	if err := validateTaskResourceScopeSnapshot(todo.ResourceScopeSnapshot, todo.SideEffect); err != nil {
		return EffectiveTaskResourceScope{}, err
	}
	if todo.ResourceScopeSnapshot.BoundedReadScope {
		eligible, err := c.taskScopeEligible(task, resolvedTools)
		if err != nil {
			return EffectiveTaskResourceScope{}, err
		}
		if !eligible {
			return EffectiveTaskResourceScope{}, fmt.Errorf("resource_scope_unreproducible: current tool/backend surface cannot enforce frozen bounded scope")
		}
		paths, err := absoluteTaskScopePaths(mutableRoot, todo.ResourceScopeSnapshot.ReadPaths)
		if err != nil {
			return EffectiveTaskResourceScope{}, err
		}
		def, _, err := c.AgentPool().ResolveAgentName(todo.Agent)
		if err != nil {
			return EffectiveTaskResourceScope{}, err
		}
		configured := append(slices.Clone(c.allowedPaths), c.runtimeAllowedPaths(def.AllowedPaths)...)
		if _, err := IntersectReadPathScopes(configured, paths); err != nil {
			return EffectiveTaskResourceScope{}, fmt.Errorf("resource_scope_unreproducible: %w", err)
		}
		if todo.ResourceScopeSnapshot.BoundedWriteScope {
			if _, err := IntersectPathScopeCeilings(paths, configured, c.runtimeAllowedWritePaths()); err != nil {
				return EffectiveTaskResourceScope{}, fmt.Errorf("resource_scope_unreproducible: %w", err)
			}
		}
	}
	return effectiveScopeFromSnapshot(todo.ResourceScopeSnapshot, mutableRoot)
}

func effectiveScopeFromSnapshot(snapshot *TaskResourceScopeSnapshot, mutableRoot string) (EffectiveTaskResourceScope, error) {
	root, err := filepath.Abs(mutableRoot)
	if err != nil {
		return EffectiveTaskResourceScope{}, fmt.Errorf("resource_scope_unreproducible: %w", err)
	}
	root = filepath.Clean(root)
	if len(snapshot.ReadPaths) > 0 || len(snapshot.WritePaths) > 0 {
		root, err = canonicalMutableRoot(root)
		if err != nil {
			return EffectiveTaskResourceScope{}, fmt.Errorf("resource_scope_unreproducible: %w", err)
		}
	}
	resolve := func(paths []string) []string {
		result := make([]string, len(paths))
		for i, path := range paths {
			directory := strings.HasSuffix(path, "/")
			result[i] = filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(path, "/")))
			if directory {
				result[i] += string(filepath.Separator)
			}
		}
		return result
	}
	return EffectiveTaskResourceScope{
		Claims: slices.Clone(snapshot.Claims), AllowedReadPaths: resolve(snapshot.ReadPaths),
		AllowedWritePaths: resolve(snapshot.WritePaths), BoundedReadScope: snapshot.BoundedReadScope,
		BoundedWriteScope: snapshot.BoundedWriteScope, Source: snapshot.Source,
	}, nil
}

func canonicalMutableRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("mutable root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func IntersectWritePathScopes(configured, runtime []string) ([]string, error) {
	return tools.IntersectWritePathScopes(configured, runtime)
}

func IntersectReadPathScopes(configured, task []string) ([]string, error) {
	return tools.IntersectReadPathScopes(configured, task)
}

func IntersectPathScopeCeilings(requested []string, ceilings ...[]string) ([]string, error) {
	return tools.IntersectPathScopeCeilings(requested, ceilings...)
}

func (c *Coordinator) recordResourceClaimsResolved(ctx context.Context, todo *TodoItem) error {
	if c == nil || todo == nil || todo.ResourceScopeSnapshot == nil {
		return nil
	}
	snapshot := cloneTaskResourceScopeSnapshot(todo.ResourceScopeSnapshot)
	if err := validateTaskResourceScopeSnapshot(snapshot, todo.SideEffect); err != nil {
		return err
	}
	payload := struct {
		Type               string          `json:"type"`
		TaskID             string          `json:"task_id"`
		OccurrenceRevision int             `json:"occurrence_revision"`
		Source             string          `json:"source"`
		BoundedReadScope   bool            `json:"bounded_read_scope"`
		BoundedWriteScope  bool            `json:"bounded_write_scope"`
		Claims             []ResourceClaim `json:"claims"`
	}{
		Type: string(EventResourceClaimsResolved), TaskID: todo.ID,
		OccurrenceRevision: max(1, todo.OccurrenceRevision), Source: snapshot.Source,
		BoundedReadScope: snapshot.BoundedReadScope, BoundedWriteScope: snapshot.BoundedWriteScope,
		Claims: slices.Clone(snapshot.Claims),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if c.hasDurableEventJournal() {
		key := fmt.Sprintf("resource-claims-resolved:%s:%s:%d:%s", c.executionRunID, todo.ID, payload.OccurrenceRevision, snapshot.Digest)
		if _, err := c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{
			Type: string(EventResourceClaimsResolved), Actor: "runtime", RunID: c.executionRunID, TaskID: todo.ID,
			IdempotencyKey: key, Payload: raw,
		}); err != nil {
			return fmt.Errorf("record resolved resource claims for %q: %w", todo.ID, err)
		}
	}
	c.report(c.newEvent(string(EventResourceClaimsResolved)).withTodoID(todo.ID).withMessage(fmt.Sprintf("%s: %d effective resource claim(s)", snapshot.Source, len(snapshot.Claims))).withData(map[string]any{
		"source": snapshot.Source, "bounded_read_scope": snapshot.BoundedReadScope,
		"bounded_write_scope": snapshot.BoundedWriteScope, "claims": slices.Clone(snapshot.Claims),
	}))
	return nil
}
