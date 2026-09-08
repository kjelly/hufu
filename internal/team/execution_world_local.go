package team

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// localExecutionWorldName is the reserved "local-sandbox" world
// (docs/hufu-external-coding-agent-runtime-spec.md §10.4).
const localExecutionWorldName = "local-sandbox"

var localExecutionWorldSeq atomic.Uint64

// LocalExecutionWorld is the first ExecutionWorld implementation. It uses the
// existing working tree rather than a VM/container — it does NOT claim
// Firecracker/container-level isolation — but it does freeze cwd/writable
// roots, sanitize the environment, and take a baseline snapshot so an
// external provider's actual effect can be verified afterward.
type LocalExecutionWorld struct {
	snapshotter WorkspaceSnapshotter
}

// NewLocalExecutionWorld returns the "local-sandbox" ExecutionWorld.
func NewLocalExecutionWorld() *LocalExecutionWorld {
	return &LocalExecutionWorld{snapshotter: NewWorkspaceSnapshotter()}
}

func (w *LocalExecutionWorld) Name() string { return localExecutionWorldName }

func (w *LocalExecutionWorld) Capabilities() ExecutionWorldCapabilities {
	return ExecutionWorldCapabilities{SupportsNetworkControl: false, SupportsNativeSandbox: false}
}

func (w *LocalExecutionWorld) Prepare(ctx context.Context, spec ExecutionWorldSpec) (*PreparedExecutionWorld, error) {
	root := strings.TrimSpace(spec.Root)
	if root == "" {
		return nil, fmt.Errorf("execution world: root is required")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("execution world: resolve root: %w", err)
	}

	cwd := strings.TrimSpace(spec.CWD)
	if cwd == "" {
		cwd = absRoot
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return nil, fmt.Errorf("execution world: resolve cwd: %w", err)
	}
	if !isWithinRoot(absRoot, absCWD) {
		return nil, fmt.Errorf("execution world: cwd %q is not inside root %q", cwd, root)
	}

	writable, readOnly, err := sideEffectExecutionRoots(absRoot, spec.SideEffect)
	if err != nil {
		return nil, err
	}
	// An explicitly narrower caller-supplied set only ever restricts, never
	// widens, the side-effect mapping above.
	if len(spec.WritableRoots) > 0 {
		writable = spec.WritableRoots
	}
	if len(spec.ReadOnlyRoots) > 0 {
		readOnly = spec.ReadOnlyRoots
	}

	baseline, err := w.snapshotter.Snapshot(ctx, absRoot)
	if err != nil {
		return nil, fmt.Errorf("execution world: baseline snapshot: %w", err)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}

	return &PreparedExecutionWorld{
		ID:             fmt.Sprintf("localworld-%d-%d", time.Now().UnixNano(), localExecutionWorldSeq.Add(1)),
		Provider:       localExecutionWorldName,
		Root:           absRoot,
		CWD:            absCWD,
		WritableRoots:  writable,
		ReadOnlyRoots:  readOnly,
		NetworkAllowed: spec.NetworkAllowed,
		Environment:    buildAllowlistedEnvironment(spec.EnvironmentAllowlist),
		Baseline:       baseline,
	}, nil
}

func (w *LocalExecutionWorld) Snapshot(ctx context.Context, prepared *PreparedExecutionWorld) (WorkspaceSnapshot, error) {
	if prepared == nil {
		return WorkspaceSnapshot{}, fmt.Errorf("execution world: prepared world is unavailable")
	}
	return w.snapshotter.Snapshot(ctx, prepared.Root)
}

// Release is a no-op for local-sandbox: version 1 allocates nothing beyond an
// in-memory baseline snapshot, so there is no container/VM/temp mount to tear
// down. A future ExecutionWorld implementation (§45.2) will have real work to
// do here.
func (w *LocalExecutionWorld) Release(context.Context, *PreparedExecutionWorld) error {
	return nil
}

var _ ExecutionWorld = (*LocalExecutionWorld)(nil)
