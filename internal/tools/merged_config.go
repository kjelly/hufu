//go:build linux || darwin
// +build linux darwin

package tools

import "context"

func mergedAllowedPaths(cfg ToolConfig, ctx context.Context) []string {
	paths := cfg.AllowedPaths
	if extra, _ := ctx.Value(AgentAllowedPathsKey).([]string); len(extra) > 0 {
		seen := make(map[string]bool, len(paths))
		for _, p := range paths {
			seen[p] = true
		}
		for _, p := range extra {
			if !seen[p] {
				paths = append(paths, p)
				seen[p] = true
			}
		}
	}
	return paths
}

func mergedAllowedWritePaths(cfg ToolConfig, ctx context.Context) []string {
	if extra, ok := ctx.Value(AgentAllowedWritePathsKey).([]string); ok && len(extra) > 0 {
		paths := append([]string(nil), cfg.AllowedWritePaths...)
		seen := make(map[string]bool, len(paths))
		for _, p := range paths {
			seen[p] = true
		}
		for _, p := range extra {
			if !seen[p] {
				paths = append(paths, p)
				seen[p] = true
			}
		}
		return paths
	}
	// Read scopes and write-isolation scopes are different capabilities. A
	// worker's AgentAllowedPathsKey is installed for ordinary read access and
	// must not silently turn into an explicit write scope: bash treats a
	// non-empty AllowedWritePaths as a runtime workflow isolation boundary.
	return cfg.AllowedWritePaths
}

func cfgWithMergedPaths(cfg ToolConfig, ctx context.Context) ToolConfig {
	needMerge := false
	if _, ok := ctx.Value(AgentAllowedPathsKey).([]string); ok {
		needMerge = true
	}
	if _, ok := ctx.Value(AgentAllowedWritePathsKey).([]string); ok {
		needMerge = true
	}
	if _, ok := ctx.Value(AgentTaskPathScopeKey).(AgentTaskPathScope); ok {
		needMerge = true
	}
	if rp, ok := ctx.Value(AgentRestrictedPathKey).(string); ok && rp != "" {
		needMerge = true
	}
	if nb, ok := ctx.Value(AgentNetworkBlockKey).(bool); ok && nb {
		needMerge = true
	}
	if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
		needMerge = true
	}
	if _, ok := ctx.Value(ArtifactPathPolicyKey).(ArtifactPathPolicy); ok {
		needMerge = true
	}
	if _, ok := executionRootFromContext(ctx); ok {
		needMerge = true
	}
	if !needMerge {
		return cfg
	}
	merged := cfg
	merged.AllowedPaths = mergedAllowedPaths(cfg, ctx)
	merged.AllowedWritePaths = mergedAllowedWritePaths(cfg, ctx)
	if scope, ok := ctx.Value(AgentTaskPathScopeKey).(AgentTaskPathScope); ok {
		merged.TaskPathScope = cloneAgentTaskPathScope(&scope)
		merged.TaskPathScopeError = applyTaskPathScope(&merged, cfg, ctx)
	}
	merged.RestrictedPath = mergedRestrictedPath(cfg, ctx)
	merged.NetworkBlock = mergedNetworkBlock(cfg, ctx)
	merged.ForceMCP = mergedForceMCP(cfg, ctx)
	if policy, ok := ctx.Value(ArtifactPathPolicyKey).(ArtifactPathPolicy); ok {
		copyPolicy := policy
		copyPolicy.BlockedPaths = append([]string(nil), policy.BlockedPaths...)
		merged.ArtifactPathPolicy = &copyPolicy
	}
	applyExecutionRoot(&merged, ctx)
	return merged
}

func applyTaskPathScope(merged *ToolConfig, configured ToolConfig, ctx context.Context) error {
	if merged == nil || merged.TaskPathScope == nil {
		return nil
	}
	if err := validateAgentTaskPathScope(merged.TaskPathScope); err != nil {
		return err
	}
	scope := merged.TaskPathScope
	if scope.ReadBounded {
		paths, err := IntersectReadPathScopes(merged.AllowedPaths, scope.ReadPaths)
		if err != nil {
			return err
		}
		merged.AllowedPaths = paths
	}
	if scope.WriteBounded {
		runtimeWrite, _ := ctx.Value(AgentAllowedWritePathsKey).([]string)
		paths, err := IntersectPathScopeCeilings(scope.WritePaths, merged.AllowedPaths, configured.AllowedWritePaths, runtimeWrite)
		if err != nil {
			return err
		}
		merged.AllowedWritePaths = paths
	}
	return nil
}

func mergedRestrictedPath(cfg ToolConfig, ctx context.Context) string {
	if rp, ok := ctx.Value(AgentRestrictedPathKey).(string); ok && rp != "" {
		return rp
	}
	return cfg.RestrictedPath
}

func mergedNetworkBlock(cfg ToolConfig, ctx context.Context) bool {
	if cfg.NetworkBlock {
		return true
	}
	if nb, ok := ctx.Value(AgentNetworkBlockKey).(bool); ok && nb {
		return true
	}
	return false
}

func mergedForceMCP(cfg ToolConfig, ctx context.Context) bool {
	if cfg.ForceMCP {
		return true
	}
	if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
		return true
	}
	return false
}
