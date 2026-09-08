package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ExecutionWorld is the Hufu-owned execution boundary an external provider
// that can run native shell/filesystem operations MUST run inside
// (docs/hufu-external-coding-agent-runtime-spec.md §10.1). Still no Codex at
// this phase (PR-06): only the generic contract and its local-sandbox
// implementation exist.
type ExecutionWorld interface {
	Name() string
	Capabilities() ExecutionWorldCapabilities

	Prepare(ctx context.Context, spec ExecutionWorldSpec) (*PreparedExecutionWorld, error)
	Snapshot(ctx context.Context, prepared *PreparedExecutionWorld) (WorkspaceSnapshot, error)
	Release(ctx context.Context, prepared *PreparedExecutionWorld) error
}

// ExecutionWorldCapabilities describes what an ExecutionWorld implementation
// can actually guarantee. Capabilities describe behavior; they never grant
// authorization on their own (mirrors SubagentCapabilities' same rule).
type ExecutionWorldCapabilities struct {
	// SupportsNetworkControl reports whether this world can actually enforce
	// NetworkAllowed rather than merely recording the caller's request.
	SupportsNetworkControl bool
	// SupportsNativeSandbox reports whether this world provides real
	// process/filesystem isolation (container/VM-grade) rather than running
	// directly in the existing working tree.
	SupportsNativeSandbox bool
}

// ExecutionWorldSpec requests one prepared world for one attempt
// (docs/hufu-external-coding-agent-runtime-spec.md §10.2).
type ExecutionWorldSpec struct {
	RunID   string
	TaskID  string
	Attempt int

	Root string
	CWD  string

	SideEffect SideEffectClass

	WritableRoots []string
	ReadOnlyRoots []string

	NetworkAllowed bool

	EnvironmentAllowlist []string

	MaxOutputBytes int64
}

// PreparedExecutionWorld is the frozen result of Prepare. No password/token
// value may be persisted in this object (§10.3).
type PreparedExecutionWorld struct {
	ID       string
	Provider string

	Root string
	CWD  string

	WritableRoots []string
	ReadOnlyRoots []string

	NetworkAllowed bool

	Environment []string

	Baseline WorkspaceSnapshot
}

// sideEffectExecutionRoots implements the side-effect → world-projection
// mapping (§10.5's Hufu-side-effect column; the Codex-sandbox column is a
// later driver's concern). writable/readOnly are Root-relative;
// credential_mutation has no projection at all and MUST reject admission.
func sideEffectExecutionRoots(root string, effect SideEffectClass) (writable, readOnly []string, err error) {
	switch effect {
	case SideEffectNone, "":
		return nil, []string{root}, nil
	case SideEffectWorkspaceWrite, SideEffectExternalWrite, SideEffectInfraMutation:
		// v1 has no broader external-effect or infra-mutation projection than
		// workspace-write (§10.5): the wider effect is simply unavailable, not
		// silently granted.
		return []string{root}, nil, nil
	case SideEffectCredential:
		return nil, nil, fmt.Errorf("execution world: side effect %q has no execution-world projection; provider admission must be rejected", effect)
	default:
		return nil, nil, fmt.Errorf("execution world: unknown side effect %q", effect)
	}
}

// buildAllowlistedEnvironment constructs a process environment strictly from
// an explicit allowlist of variable names, reading each one's current value
// only if the name was allowlisted (§10.7: "Do not use os.Environ() ...
// Construct environment from an explicit allowlist"). A name absent from the
// current environment is silently omitted, not set to empty.
func buildAllowlistedEnvironment(allowlist []string) []string {
	env := make([]string, 0, len(allowlist))
	seen := make(map[string]bool, len(allowlist))
	for _, name := range allowlist {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// ValidateExecutionWorldDelta enforces §10.4's "detect changes outside
// allowed writable roots after execution" rule: every Added/Modified/Deleted
// path in delta must fall under one of prepared's WritableRoots (relative to
// Root). An empty WritableRoots means nothing was authorized to change, so
// any observed change at all is a violation.
func ValidateExecutionWorldDelta(prepared *PreparedExecutionWorld, delta WorkspaceDelta) error {
	if prepared == nil {
		return fmt.Errorf("execution world: prepared world is unavailable")
	}
	relWritable := make([]string, 0, len(prepared.WritableRoots))
	for _, root := range prepared.WritableRoots {
		rel, err := workspaceRelativeWritableRoot(prepared.Root, root)
		if err != nil {
			return err
		}
		relWritable = append(relWritable, rel)
	}
	check := func(path string) error {
		if len(relWritable) == 0 {
			return fmt.Errorf("execution world: %q changed outside every authorized writable root (none were writable)", path)
		}
		for _, allowed := range relWritable {
			if allowed == "." || path == allowed || strings.HasPrefix(path, allowed+"/") {
				return nil
			}
		}
		return fmt.Errorf("execution world: %q changed outside every authorized writable root", path)
	}
	for _, f := range delta.Added {
		if err := check(f.Path); err != nil {
			return err
		}
	}
	for _, f := range delta.Modified {
		if err := check(f.Path); err != nil {
			return err
		}
	}
	for _, path := range delta.Deleted {
		if err := check(path); err != nil {
			return err
		}
	}
	return nil
}

func workspaceRelativeWritableRoot(root, writableRoot string) (string, error) {
	if writableRoot == root {
		return ".", nil
	}
	rel, err := filepath.Rel(root, writableRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("execution world: writable root %q is not inside workspace root %q", writableRoot, root)
	}
	return filepath.ToSlash(rel), nil
}
