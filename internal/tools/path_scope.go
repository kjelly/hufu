package tools

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

func IntersectWritePathScopes(configured, runtime []string) ([]string, error) {
	return IntersectPathScopeCeilings(runtime, configured)
}

func IntersectReadPathScopes(configured, task []string) ([]string, error) {
	return IntersectPathScopeCeilings(task, configured)
}

func IntersectPathScopeCeilings(requested []string, ceilings ...[]string) ([]string, error) {
	if requested == nil {
		if len(ceilings) == 0 || len(ceilings[0]) == 0 {
			return nil, nil
		}
		return canonicalPathScopeEntries(ceilings[0])
	}
	canonical, err := canonicalPathScopeEntries(requested)
	if err != nil {
		return nil, err
	}
	if len(canonical) == 0 {
		return nil, fmt.Errorf("invalid_write_scope: explicit path scope is empty")
	}
	for _, ceilingGroup := range ceilings {
		if len(ceilingGroup) == 0 {
			continue
		}
		allowed, err := canonicalPathScopeEntries(ceilingGroup)
		if err != nil {
			return nil, err
		}
		for _, path := range canonical {
			if !pathScopeEntryCovered(path, allowed) {
				return nil, fmt.Errorf("invalid_write_scope: requested path %q is outside configured scope", path)
			}
		}
	}
	return canonical, nil
}

func canonicalPathScopeEntries(paths []string) ([]string, error) {
	result := make([]string, 0, len(paths))
	for _, raw := range paths {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("invalid_write_scope: path is empty")
		}
		directory := strings.HasSuffix(filepath.ToSlash(raw), "/")
		clean := filepath.Clean(raw)
		if directory && clean != string(filepath.Separator) && clean != "." {
			clean += string(filepath.Separator)
		}
		result = append(result, clean)
	}
	slices.Sort(result)
	return slices.Compact(result), nil
}

func pathScopeEntryCovered(requested string, ceilings []string) bool {
	want := strings.TrimSuffix(filepath.Clean(requested), string(filepath.Separator))
	for _, ceiling := range ceilings {
		root := strings.TrimSuffix(filepath.Clean(ceiling), string(filepath.Separator))
		rel, err := filepath.Rel(root, want)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func cloneAgentTaskPathScope(scope *AgentTaskPathScope) *AgentTaskPathScope {
	if scope == nil {
		return nil
	}
	cloned := *scope
	cloned.ReadPaths = slices.Clone(scope.ReadPaths)
	cloned.WritePaths = slices.Clone(scope.WritePaths)
	return &cloned
}

func validateAgentTaskPathScope(scope *AgentTaskPathScope) error {
	if scope == nil {
		return nil
	}
	if scope.Digest == "" {
		return fmt.Errorf("resource_scope_unreproducible: task path scope digest is empty")
	}
	if scope.ReadBounded != (len(scope.ReadPaths) > 0) {
		return fmt.Errorf("resource_scope_unreproducible: bounded read flag does not match paths")
	}
	if scope.WriteBounded != (len(scope.WritePaths) > 0) {
		return fmt.Errorf("resource_scope_unreproducible: bounded write flag does not match paths")
	}
	if scope.WriteBounded && !scope.ReadBounded {
		return fmt.Errorf("resource_scope_unreproducible: bounded write scope requires bounded read scope")
	}
	return nil
}
