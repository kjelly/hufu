package team

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/fsutil"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

const (
	// isolatedCopyExecutionWorldName is the execution world that runs an
	// attempt in a private copy of the project.
	isolatedCopyExecutionWorldName = "isolated-copy"
	// attemptWorldsDirName holds attempt worlds under the control root.
	attemptWorldsDirName  = "attempt-worlds"
	attemptWorldOwnerFile = "owner.json"
	attemptWorldRootDir   = "root"
	attemptWorldFormat    = 1
	// isolatedWorldMaxBytes bounds the project bytes one world may copy.
	isolatedWorldMaxBytes int64 = 2 << 30
)

// attemptWorldOwner is the marker written first into every attempt world. A
// directory is Hufu-owned, and may be deleted, only when its marker parses
// and names the directory's own world ID.
type attemptWorldOwner struct {
	Format            int       `json:"format"`
	WorldID           string    `json:"world_id"`
	RunID             string    `json:"run_id"`
	TaskID            string    `json:"task_id"`
	Attempt           int       `json:"attempt"`
	OccurrenceAttempt int       `json:"occurrence_attempt,omitempty"`
	SourceRoot        string    `json:"source_root"`
	CreatedAt         time.Time `json:"created_at"`
}

// isolatedWorldState is the process-local state of one prepared world.
type isolatedWorldState struct {
	worldID    string
	worldDir   string
	sourceRoot string
	gitMode    bool
	baseline   attemptManifest
	fileCount  int
	byteCount  int64
}

// IsolatedCopyExecutionWorld runs an attempt in a Hufu-owned copy of the
// project's managed files, including uncommitted changes. It never touches
// the user's .git: Git is used only to discover candidate files and ignore
// rules. It is not a security sandbox; a shell can still name an absolute
// path outside the copy.
type IsolatedCopyExecutionWorld struct {
	snapshotter WorkspaceSnapshotter
	now         func() time.Time
}

// NewIsolatedCopyExecutionWorld returns the "isolated-copy" ExecutionWorld.
func NewIsolatedCopyExecutionWorld() *IsolatedCopyExecutionWorld {
	return &IsolatedCopyExecutionWorld{snapshotter: NewWorkspaceSnapshotter(), now: time.Now}
}

func (w *IsolatedCopyExecutionWorld) Name() string { return isolatedCopyExecutionWorldName }

func (w *IsolatedCopyExecutionWorld) Capabilities() ExecutionWorldCapabilities {
	return ExecutionWorldCapabilities{SupportsNetworkControl: false, SupportsNativeSandbox: false}
}

// Prepare copies the subject root's managed files into a new attempt world
// under the control root and records the copied state as the baseline.
func (w *IsolatedCopyExecutionWorld) Prepare(ctx context.Context, spec ExecutionWorldSpec) (*PreparedExecutionWorld, error) {
	subject, control, err := isolatedWorldRoots(spec)
	if err != nil {
		return nil, err
	}
	set, err := versionstore.DiscoverManagedPaths(ctx, subject, versionstore.DiscoveryOptions{})
	if err != nil {
		return nil, fmt.Errorf("%s: %w", workspaceIsolationUnavailableCode, err)
	}
	if len(set.Nested) > 0 {
		return nil, fmt.Errorf("%s: the project contains nested repositories or submodules (%s) that an isolated copy cannot represent", workspaceIsolationUnavailableCode, strings.Join(set.Nested, ", "))
	}

	lock := attemptWorkspaceIntegrationLock(subject)
	lock.RLock()
	defer lock.RUnlock()

	source, err := os.OpenRoot(subject)
	if err != nil {
		return nil, fmt.Errorf("%s: open project: %w", workspaceIsolationUnavailableCode, err)
	}
	defer func() { _ = source.Close() }()
	plan, fileCount, byteCount, err := planIsolatedCopy(source, set.Paths)
	if err != nil {
		return nil, err
	}

	worldID, worldDir, err := createAttemptWorldDir(filepath.Join(control, attemptWorldsDirName))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", workspaceIsolationUnavailableCode, err)
	}
	owner := attemptWorldOwner{
		Format: attemptWorldFormat, WorldID: worldID, RunID: spec.RunID, TaskID: spec.TaskID,
		Attempt: spec.Attempt, OccurrenceAttempt: spec.OccurrenceAttempt, SourceRoot: subject, CreatedAt: w.now().UTC(),
	}
	if err := writeAttemptWorldOwner(worldDir, owner); err != nil {
		_ = os.RemoveAll(worldDir)
		return nil, fmt.Errorf("%s: %w", workspaceIsolationUnavailableCode, err)
	}
	worldRoot := filepath.Join(worldDir, attemptWorldRootDir)
	baseline, err := copyIntoAttemptWorld(ctx, source, worldRoot, plan)
	if err != nil {
		_ = os.RemoveAll(worldDir)
		return nil, err
	}
	if err := writeAttemptWorldBaseline(worldDir, set.GitMode, baseline); err != nil {
		_ = os.RemoveAll(worldDir)
		return nil, fmt.Errorf("%s: %w", workspaceIsolationUnavailableCode, err)
	}

	cwd := worldRoot
	if requested := strings.TrimSpace(spec.CWD); requested != "" {
		if rel, relErr := filepath.Rel(subject, requested); relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			cwd = filepath.Join(worldRoot, rel)
		}
	}
	environment := buildAllowlistedEnvironment(spec.EnvironmentAllowlist)
	if spec.Environment != nil {
		environment = append([]string(nil), spec.Environment...)
	}
	return &PreparedExecutionWorld{
		ID: worldID, Provider: isolatedCopyExecutionWorldName,
		Root: worldRoot, CWD: cwd, WritableRoots: []string{worldRoot},
		NetworkAllowed: spec.NetworkAllowed, Environment: environment,
		isolated: &isolatedWorldState{
			worldID: worldID, worldDir: worldDir, sourceRoot: subject, gitMode: set.GitMode,
			baseline: baseline, fileCount: fileCount, byteCount: byteCount,
		},
	}, nil
}

// Snapshot observes the world root with the effect snapshotter, for callers
// that consume the generic ExecutionWorld contract. Isolated integration uses
// Delta instead, which is kind-aware and ignore-aware.
func (w *IsolatedCopyExecutionWorld) Snapshot(ctx context.Context, prepared *PreparedExecutionWorld) (WorkspaceSnapshot, error) {
	if prepared == nil {
		return WorkspaceSnapshot{}, fmt.Errorf("execution world: prepared world is unavailable")
	}
	return w.snapshotter.Snapshot(ctx, prepared.Root)
}

// Delta is what the attempt changed in its world relative to the copied
// baseline. New paths the project ignores (build output, dependencies) are
// dropped; they are discarded with the world.
func (w *IsolatedCopyExecutionWorld) Delta(ctx context.Context, prepared *PreparedExecutionWorld) (*AttemptWorkspaceDelta, error) {
	state, err := isolatedStateOf(prepared)
	if err != nil {
		return nil, err
	}
	final, err := scanAttemptRoot(ctx, prepared.Root)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", workspaceDeltaRejectedCode, err)
	}
	var added []string
	for rel := range final {
		if _, existed := state.baseline[rel]; !existed {
			added = append(added, rel)
		}
	}
	if len(added) > 0 {
		ignored, err := isolatedWorldIgnored(ctx, state, added)
		if err != nil {
			return nil, fmt.Errorf("%s: evaluate ignore rules: %w", workspaceDeltaRejectedCode, err)
		}
		for rel := range ignored {
			delete(final, rel)
		}
	}
	delta := computeAttemptWorkspaceDelta(state.worldID, state.baseline, final)
	if err := validateAttemptWorkspaceDelta(delta); err != nil {
		return nil, err
	}
	return delta, nil
}

// Release deletes the world. It refuses to delete a directory whose owner
// marker does not name this world.
func (w *IsolatedCopyExecutionWorld) Release(_ context.Context, prepared *PreparedExecutionWorld) error {
	state, err := isolatedStateOf(prepared)
	if err != nil {
		return err
	}
	return removeAttemptWorldDir(state.worldDir, state.worldID)
}

var _ ExecutionWorld = (*IsolatedCopyExecutionWorld)(nil)

func isolatedStateOf(prepared *PreparedExecutionWorld) (*isolatedWorldState, error) {
	if prepared == nil || prepared.isolated == nil {
		return nil, fmt.Errorf("execution world: prepared world is not an isolated copy")
	}
	return prepared.isolated, nil
}

// isolatedWorldRoots resolves the subject and control roots and requires
// that neither contains the other: worlds live under the control root and
// must never be observed as project files.
func isolatedWorldRoots(spec ExecutionWorldSpec) (string, string, error) {
	resolve := func(label, value string) (string, error) {
		if strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("%s: %s root is required", workspaceIsolationUnsupportedCode, label)
		}
		abs, err := filepath.Abs(value)
		if err != nil {
			return "", fmt.Errorf("%s: resolve %s root: %w", workspaceIsolationUnsupportedCode, label, err)
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", fmt.Errorf("%s: resolve %s root: %w", workspaceIsolationUnsupportedCode, label, err)
		}
		return resolved, nil
	}
	subject, err := resolve("project", spec.Root)
	if err != nil {
		return "", "", err
	}
	control, err := resolve("control", spec.ControlWorkspace)
	if err != nil {
		return "", "", err
	}
	if isWithinRoot(subject, control) || isWithinRoot(control, subject) {
		return "", "", fmt.Errorf("%s: the control root %s and the project %s overlap; isolation requires a managed workspace", workspaceIsolationUnsupportedCode, control, subject)
	}
	return subject, control, nil
}

type isolatedCopyEntry struct {
	rel    string
	kind   AttemptEntryKind
	mode   fs.FileMode
	target string
}

// planIsolatedCopy classifies every candidate before anything is written, so
// an unsupported layout or an over-limit project fails before a world exists.
func planIsolatedCopy(source *os.Root, candidates []string) ([]isolatedCopyEntry, int, int64, error) {
	plan := make([]isolatedCopyEntry, 0, len(candidates))
	var total int64
	for _, rel := range candidates {
		info, err := source.Lstat(filepath.FromSlash(rel))
		if errors.Is(err, fs.ErrNotExist) {
			continue // tracked but deleted from the working tree
		}
		if err != nil {
			return nil, 0, 0, fmt.Errorf("%s: stat %s: %w", workspaceIsolationUnavailableCode, rel, err)
		}
		switch {
		case info.Mode().IsRegular():
			total += info.Size()
			plan = append(plan, isolatedCopyEntry{rel: rel, kind: AttemptEntryFile, mode: info.Mode().Perm()})
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := source.Readlink(filepath.FromSlash(rel))
			if err != nil {
				return nil, 0, 0, fmt.Errorf("%s: read link %s: %w", workspaceIsolationUnavailableCode, rel, err)
			}
			if !symlinkTargetStaysInRoot(rel, target) {
				return nil, 0, 0, fmt.Errorf("%s: symlink %s points outside the project (%q)", workspaceIsolationUnavailableCode, rel, target)
			}
			plan = append(plan, isolatedCopyEntry{rel: rel, kind: AttemptEntrySymlink, target: target})
		case info.IsDir():
			continue
		default:
			return nil, 0, 0, fmt.Errorf("%s: %s is a special file (%s) that an isolated copy cannot represent", workspaceIsolationUnavailableCode, rel, info.Mode().Type())
		}
		if len(plan) > workspaceSnapshotMaxFiles {
			return nil, 0, 0, fmt.Errorf("%s: the project has more than %d managed files", workspaceIsolationUnavailableCode, workspaceSnapshotMaxFiles)
		}
		if total > isolatedWorldMaxBytes {
			return nil, 0, 0, fmt.Errorf("%s: the project's managed files exceed %d bytes", workspaceIsolationUnavailableCode, isolatedWorldMaxBytes)
		}
	}
	return plan, len(plan), total, nil
}

// copyIntoAttemptWorld copies the planned entries through os.Root on both
// sides, never following a symlink, and hashes the bytes actually copied.
func copyIntoAttemptWorld(ctx context.Context, source *os.Root, worldRoot string, plan []isolatedCopyEntry) (attemptManifest, error) {
	if err := os.MkdirAll(worldRoot, 0o755); err != nil {
		return nil, fmt.Errorf("%s: create world root: %w", workspaceIsolationUnavailableCode, err)
	}
	dest, err := os.OpenRoot(worldRoot)
	if err != nil {
		return nil, fmt.Errorf("%s: open world root: %w", workspaceIsolationUnavailableCode, err)
	}
	defer func() { _ = dest.Close() }()
	baseline := make(attemptManifest, len(plan))
	for _, entry := range plan {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := filepath.FromSlash(entry.rel)
		if dir := path.Dir(entry.rel); dir != "." {
			if err := dest.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
				return nil, fmt.Errorf("%s: create %s: %w", workspaceIsolationUnavailableCode, dir, err)
			}
		}
		switch entry.kind {
		case AttemptEntryFile:
			observed, err := copyAttemptFile(source, dest, entry)
			if err != nil {
				return nil, fmt.Errorf("%s: copy %s: %w", workspaceIsolationUnavailableCode, entry.rel, err)
			}
			baseline[entry.rel] = observed
		case AttemptEntrySymlink:
			if err := dest.Symlink(entry.target, name); err != nil {
				return nil, fmt.Errorf("%s: link %s: %w", workspaceIsolationUnavailableCode, entry.rel, err)
			}
			sum := sha256.Sum256([]byte(entry.target))
			baseline[entry.rel] = AttemptManifestEntry{Path: entry.rel, Kind: AttemptEntrySymlink, SHA256: hex.EncodeToString(sum[:]), LinkTarget: entry.target}
		}
	}
	return baseline, nil
}

func copyAttemptFile(source, dest *os.Root, entry isolatedCopyEntry) (AttemptManifestEntry, error) {
	name := filepath.FromSlash(entry.rel)
	in, err := source.Open(name)
	if err != nil {
		return AttemptManifestEntry{}, err
	}
	defer func() { _ = in.Close() }()
	out, err := dest.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, entry.mode)
	if err != nil {
		return AttemptManifestEntry{}, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(out, hash), in)
	closeErr := out.Close()
	if copyErr != nil {
		return AttemptManifestEntry{}, copyErr
	}
	if closeErr != nil {
		return AttemptManifestEntry{}, closeErr
	}
	// The umask can narrow the create mode; the copy must keep the source's.
	if err := dest.Chmod(name, entry.mode); err != nil {
		return AttemptManifestEntry{}, err
	}
	return AttemptManifestEntry{Path: entry.rel, Kind: AttemptEntryFile, SHA256: hex.EncodeToString(hash.Sum(nil)), Size: size, Mode: uint32(entry.mode)}, nil
}

// isolatedWorldIgnored applies the project's own ignore rules to paths the
// attempt created: Git's rules for a Git working tree (evaluated read-only
// in the project), otherwise the project's .hufuignore.
func isolatedWorldIgnored(ctx context.Context, state *isolatedWorldState, paths []string) (map[string]bool, error) {
	if state.gitMode {
		return versionstore.GitIgnoredPaths(ctx, state.sourceRoot, paths)
	}
	matches, err := versionstore.HufuignoreMatcher(state.sourceRoot)
	if err != nil {
		return nil, err
	}
	ignored := make(map[string]bool)
	for _, rel := range paths {
		if matches(rel, false) || ignoredByParent(matches, rel) {
			ignored[rel] = true
		}
	}
	return ignored, nil
}

func ignoredByParent(matches func(string, bool) bool, rel string) bool {
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if matches(dir, true) {
			return true
		}
	}
	return false
}

// createAttemptWorldDir creates a new world directory named by a random
// nonce. The nonce is never derived from worker input or from process-local
// counters, so a restart cannot collide with an older world.
func createAttemptWorldDir(worldsDir string) (string, string, error) {
	if err := os.MkdirAll(worldsDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create attempt worlds directory: %w", err)
	}
	for range 3 {
		var nonce [8]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return "", "", fmt.Errorf("generate world id: %w", err)
		}
		worldID := "aw-" + hex.EncodeToString(nonce[:])
		worldDir := filepath.Join(worldsDir, worldID)
		if err := os.Mkdir(worldDir, 0o755); err == nil {
			return worldID, worldDir, nil
		} else if !errors.Is(err, fs.ErrExist) {
			return "", "", fmt.Errorf("create world directory: %w", err)
		}
	}
	return "", "", errors.New("could not allocate a unique world id")
}

func writeAttemptWorldOwner(worldDir string, owner attemptWorldOwner) error {
	encoded, err := json.MarshalIndent(owner, "", "  ")
	if err != nil {
		return fmt.Errorf("encode world owner: %w", err)
	}
	if err := fsutil.AtomicWriteFile(filepath.Join(worldDir, attemptWorldOwnerFile), append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write world owner: %w", err)
	}
	return nil
}

// readAttemptWorldOwner returns a world directory's owner marker. A missing
// or malformed marker, or one that names another directory, means the
// directory is not a Hufu-owned world.
func readAttemptWorldOwner(worldDir string) (attemptWorldOwner, error) {
	data, err := os.ReadFile(filepath.Join(worldDir, attemptWorldOwnerFile))
	if err != nil {
		return attemptWorldOwner{}, fmt.Errorf("read world owner: %w", err)
	}
	var owner attemptWorldOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return attemptWorldOwner{}, fmt.Errorf("decode world owner: %w", err)
	}
	if owner.Format != attemptWorldFormat || owner.WorldID == "" || owner.WorldID != filepath.Base(worldDir) {
		return attemptWorldOwner{}, fmt.Errorf("world owner marker does not name %s", filepath.Base(worldDir))
	}
	return owner, nil
}

// removeAttemptWorldDir deletes a world only when its owner marker names it
// and it lies directly under an attempt-worlds directory.
func removeAttemptWorldDir(worldDir, worldID string) error {
	if filepath.Base(filepath.Dir(worldDir)) != attemptWorldsDirName || filepath.Base(worldDir) != worldID {
		return fmt.Errorf("refusing to remove %s: not an attempt world directory", worldDir)
	}
	if _, err := os.Lstat(worldDir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if _, err := readAttemptWorldOwner(worldDir); err != nil {
		return fmt.Errorf("refusing to remove %s: %w", worldDir, err)
	}
	if err := os.RemoveAll(worldDir); err != nil {
		return fmt.Errorf("remove attempt world: %w", err)
	}
	return nil
}
