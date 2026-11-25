package team

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// localExecutionWorldName is the reserved "local-sandbox" world
// (docs/architecture/execution-runtime.md).
const localExecutionWorldName = "local-sandbox"

var localExecutionWorldSeq atomic.Uint64

type localExecutionWorldLease struct {
	slot chan struct{}
	refs int
}

var localExecutionWorldLeases = struct {
	sync.Mutex
	byRoot map[string]*localExecutionWorldLease
}{byRoot: make(map[string]*localExecutionWorldLease)}

// acquireLocalExecutionWorldLease serializes snapshot/process lifetimes for
// one canonical workspace root. A snapshot is a temporal observation, not
// process provenance; without this lease two attempts can observe each
// other's writes and both receive an incorrect delta.
func acquireLocalExecutionWorldLease(ctx context.Context, root string) (func(), error) {
	localExecutionWorldLeases.Lock()
	lease := localExecutionWorldLeases.byRoot[root]
	if lease == nil {
		lease = &localExecutionWorldLease{slot: make(chan struct{}, 1)}
		localExecutionWorldLeases.byRoot[root] = lease
	}
	lease.refs++
	localExecutionWorldLeases.Unlock()

	select {
	case lease.slot <- struct{}{}:
		return sync.OnceFunc(func() {
			<-lease.slot
			releaseLocalExecutionWorldLease(root, lease)
		}), nil
	case <-ctx.Done():
		releaseLocalExecutionWorldLease(root, lease)
		return nil, ctx.Err()
	}
}

func releaseLocalExecutionWorldLease(root string, lease *localExecutionWorldLease) {
	localExecutionWorldLeases.Lock()
	lease.refs--
	if lease.refs == 0 && localExecutionWorldLeases.byRoot[root] == lease {
		delete(localExecutionWorldLeases.byRoot, root)
	}
	localExecutionWorldLeases.Unlock()
}

// LocalExecutionWorld is the first ExecutionWorld implementation. It uses the
// existing working tree rather than a VM/container — it does NOT claim
// Firecracker/container-level isolation — but it does freeze cwd/writable
// roots, sanitize the environment, and take a baseline snapshot so an
// external provider's actual effect can be verified afterward.
type LocalExecutionWorld struct {
	snapshotter *defaultWorkspaceSnapshotter
}

// NewLocalExecutionWorld returns the "local-sandbox" ExecutionWorld.
func NewLocalExecutionWorld() *LocalExecutionWorld {
	return &LocalExecutionWorld{snapshotter: &defaultWorkspaceSnapshotter{}}
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
	relativeCWD, err := filepath.Rel(absRoot, absCWD)
	if err != nil || !filepath.IsLocal(relativeCWD) {
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

	leaseRoot, admittedRoot, err := resolveSnapshotDirectory(absRoot)
	if err != nil {
		return nil, fmt.Errorf("execution world: resolve lease root: %w", err)
	}
	if spec.RootIdentity != nil && leaseRoot != absRoot {
		return nil, errWorkspaceSnapshotRootChanged
	}
	ignoredSnapshotPaths, err := hufuControlWorkspaceSnapshotPaths(leaseRoot, spec.ControlWorkspace)
	if err != nil {
		return nil, err
	}

	// Use the resolved root only as the lease identity. Keep the lexical root
	// in the prepared object so caller-supplied writable roots remain relative
	// to the same path representation used during admission/validation.
	releaseLease, err := acquireLocalExecutionWorldLease(ctx, leaseRoot)
	if err != nil {
		return nil, fmt.Errorf("execution world: acquire workspace lease: %w", err)
	}
	rootDirectory, cwdDirectory, err := openLocalExecutionDirectories(leaseRoot, relativeCWD)
	if err != nil {
		releaseLease()
		return nil, fmt.Errorf("execution world: pin workspace directories: %w", err)
	}
	releaseWorld := sync.OnceFunc(func() {
		if cwdDirectory != rootDirectory {
			_ = cwdDirectory.Close()
		}
		_ = rootDirectory.Close()
		releaseLease()
	})
	rootInfo, err := rootDirectory.Stat()
	if err != nil {
		releaseWorld()
		return nil, fmt.Errorf("execution world: stat pinned workspace root: %w", err)
	}
	if !os.SameFile(admittedRoot, rootInfo) ||
		(spec.RootIdentity != nil && !os.SameFile(spec.RootIdentity, rootInfo)) {
		releaseWorld()
		return nil, errWorkspaceSnapshotRootChanged
	}
	baseline, err := w.snapshotter.snapshotPinned(ctx, absRoot, leaseRoot, rootDirectory, ignoredSnapshotPaths)
	if err != nil {
		releaseWorld()
		return nil, fmt.Errorf("execution world: baseline snapshot: %w", err)
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		releaseWorld()
		return nil, ctxErr
	}

	environment := buildAllowlistedEnvironment(spec.EnvironmentAllowlist)
	if spec.Environment != nil {
		environment = append([]string(nil), spec.Environment...)
	}

	return &PreparedExecutionWorld{
		ID:                   fmt.Sprintf("localworld-%d-%d", time.Now().UnixNano(), localExecutionWorldSeq.Add(1)),
		Provider:             localExecutionWorldName,
		Root:                 absRoot,
		CWD:                  absCWD,
		WritableRoots:        writable,
		ReadOnlyRoots:        readOnly,
		NetworkAllowed:       spec.NetworkAllowed,
		Environment:          environment,
		Baseline:             baseline,
		snapshotIgnoredPaths: ignoredSnapshotPaths,
		localRoot:            rootDirectory,
		localCWD:             cwdDirectory,
		releaseLease:         releaseWorld,
	}, nil
}

func openLocalExecutionDirectories(root, relativeCWD string) (*os.File, *os.File, error) {
	rootHandle, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rootHandle.Close() }()
	rootDir, err := rootHandle.Open(".")
	if err != nil {
		return nil, nil, err
	}
	if relativeCWD == "." {
		return rootDir, rootDir, nil
	}
	cwdHandle, err := rootHandle.OpenRoot(relativeCWD)
	if err != nil {
		_ = rootDir.Close()
		return nil, nil, err
	}
	defer func() { _ = cwdHandle.Close() }()
	cwdDir, err := cwdHandle.Open(".")
	if err != nil {
		_ = rootDir.Close()
		return nil, nil, err
	}
	return rootDir, cwdDir, nil
}

func (w *LocalExecutionWorld) Snapshot(ctx context.Context, prepared *PreparedExecutionWorld) (WorkspaceSnapshot, error) {
	if prepared == nil || prepared.localRoot == nil {
		return WorkspaceSnapshot{}, fmt.Errorf("execution world: prepared world is unavailable")
	}
	return w.snapshotter.snapshotPinned(ctx, prepared.Root, prepared.Baseline.Root, prepared.localRoot, prepared.snapshotIgnoredPaths)
}

// Release relinquishes the process-local shared-workspace lease. A future
// ExecutionWorld implementation (§45.2) may have additional container/VM
// teardown work here.
func (w *LocalExecutionWorld) Release(_ context.Context, prepared *PreparedExecutionWorld) error {
	if prepared != nil && prepared.releaseLease != nil {
		prepared.releaseLease()
	}
	return nil
}

var _ ExecutionWorld = (*LocalExecutionWorld)(nil)
