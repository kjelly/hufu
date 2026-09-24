package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

type agentExecutionRootKeyType struct{}

// AgentExecutionRootKey installs an AgentExecutionRoot for one attempt. Every
// filesystem-bound tool then treats Root as the project root: relative paths
// resolve inside it, shells start in it, and writes into DeniedWriteRoots are
// refused even when another allowed path would otherwise cover them.
var AgentExecutionRootKey = agentExecutionRootKeyType{}

// AgentExecutionRoot rebinds an attempt's tools to a private project root.
type AgentExecutionRoot struct {
	Root string
	// DeniedWriteRoots are never writable by file tools, except where Root
	// itself lies beneath one of them. The runtime uses it to keep an
	// isolated attempt out of the canonical project and out of other
	// attempts' private roots.
	DeniedWriteRoots []string
}

func executionRootFromContext(ctx context.Context) (AgentExecutionRoot, bool) {
	if ctx == nil {
		return AgentExecutionRoot{}, false
	}
	root, ok := ctx.Value(AgentExecutionRootKey).(AgentExecutionRoot)
	if !ok || strings.TrimSpace(root.Root) == "" {
		return AgentExecutionRoot{}, false
	}
	return root, true
}

// executionWorkDir is the project root a tool must resolve relative paths
// against: the attempt's execution root when one is installed, otherwise
// the root the tool was constructed with.
func executionWorkDir(ctx context.Context, cfg ToolConfig) string {
	if root, ok := executionRootFromContext(ctx); ok {
		return root.Root
	}
	return cfg.WorkDir
}

// applyExecutionRoot rebinds a merged tool configuration to the attempt's
// execution root. AllowedWritePaths is deliberately left alone: a non-empty
// write list is the phase-workflow isolation boundary and disables bash.
func applyExecutionRoot(merged *ToolConfig, ctx context.Context) {
	root, ok := executionRootFromContext(ctx)
	if !ok || merged == nil {
		return
	}
	merged.WorkDir = root.Root
	merged.ExecutionRoot = root.Root
	merged.DeniedWriteRoots = append([]string(nil), root.DeniedWriteRoots...)
	for _, allowed := range merged.AllowedPaths {
		if allowed == root.Root {
			return
		}
	}
	merged.AllowedPaths = append(append([]string(nil), merged.AllowedPaths...), root.Root)
}

// checkExecutionRootWrite enforces the execution root write rule on a path
// the ordinary write resolver already accepted: inside the execution root is
// allowed, inside a denied root is refused, anything else keeps the
// ordinary decision.
func checkExecutionRootWrite(resolved string, cfg ToolConfig) error {
	if cfg.ExecutionRoot == "" {
		return nil
	}
	target := canonicalWritePath(resolved)
	if pathWithinRoot(canonicalWritePath(cfg.ExecutionRoot), target) {
		return nil
	}
	for _, denied := range cfg.DeniedWriteRoots {
		if strings.TrimSpace(denied) == "" {
			continue
		}
		if pathWithinRoot(canonicalWritePath(denied), target) {
			return fmt.Errorf("write to %s is outside this attempt's isolated project root %s; write inside the project root instead", resolved, cfg.ExecutionRoot)
		}
	}
	return nil
}

// canonicalWritePath resolves symlinks in the deepest existing ancestor of
// path, so a write through a symlinked directory is judged by where it lands.
func canonicalWritePath(path string) string {
	current := filepath.Clean(path)
	rest := ""
	for {
		if evaluated, err := filepath.EvalSymlinks(current); err == nil {
			if rest == "" {
				return filepath.Clean(evaluated)
			}
			return filepath.Join(evaluated, rest)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(path)
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

func pathWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// executionRootShellEnv points a shell at the execution root and stops git
// from walking above it into an enclosing repository.
func executionRootShellEnv(ctx context.Context, env []string) []string {
	root, ok := executionRootFromContext(ctx)
	if !ok {
		return env
	}
	return append(env, "PWD="+root.Root, "GIT_CEILING_DIRECTORIES="+filepath.Dir(root.Root))
}
