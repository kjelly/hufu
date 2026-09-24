package team

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// attemptWorkspaceConflictReportLimit bounds the paths a conflict names.
const attemptWorkspaceConflictReportLimit = 50

// AttemptWorkspaceApplyResult summarizes an applied delta.
type AttemptWorkspaceApplyResult struct {
	FilesWritten int
	FilesDeleted int
}

// AttemptWorkspaceConflictError reports canonical paths that no longer match
// the state the attempt started from. Nothing was written.
type AttemptWorkspaceConflictError struct {
	Paths []string
	Total int
}

func (e *AttemptWorkspaceConflictError) Error() string {
	more := ""
	if e.Total > len(e.Paths) {
		more = fmt.Sprintf(" and %d more", e.Total-len(e.Paths))
	}
	return fmt.Sprintf("%s: %d path(s) changed in the project since this attempt started: %s%s", workspaceConflictCode, e.Total, strings.Join(e.Paths, ", "), more)
}

type attemptApplyAction int

const (
	attemptApplySkip attemptApplyAction = iota
	attemptApplyWrite
	attemptApplyDelete
)

// applyAttemptWorkspaceDelta applies an isolated attempt's changes to the
// canonical project. Every path's precondition is checked before anything is
// written: the canonical state must equal the attempt's baseline (or already
// equal its final state, which makes a re-run after a crash converge). Any
// other state is a conflict and nothing changes. Writes stage through temp
// files and renames inside os.Root, never following symlinks; deletions run
// only after every write succeeded; the result is verified before return.
func applyAttemptWorkspaceDelta(ctx context.Context, subjectRoot, worldRoot string, delta *AttemptWorkspaceDelta) (AttemptWorkspaceApplyResult, error) {
	if err := validateAttemptWorkspaceDelta(delta); err != nil {
		return AttemptWorkspaceApplyResult{}, err
	}
	lock := attemptWorkspaceIntegrationLock(subjectRoot)
	lock.Lock()
	defer lock.Unlock()

	canonical, err := os.OpenRoot(subjectRoot)
	if err != nil {
		return AttemptWorkspaceApplyResult{}, fmt.Errorf("%s: open project: %w", workspaceApplyIncompleteCode, err)
	}
	defer func() { _ = canonical.Close() }()
	world, err := os.OpenRoot(worldRoot)
	if err != nil {
		return AttemptWorkspaceApplyResult{}, fmt.Errorf("%s: open attempt world: %w", workspaceApplyIncompleteCode, err)
	}
	defer func() { _ = world.Close() }()

	actions, err := planAttemptWorkspaceApply(canonical, delta.Changes)
	if err != nil {
		return AttemptWorkspaceApplyResult{}, err
	}
	var result AttemptWorkspaceApplyResult
	for i, change := range delta.Changes {
		if err := ctx.Err(); err != nil {
			return result, fmt.Errorf("%s: %w", workspaceApplyIncompleteCode, err)
		}
		if actions[i] != attemptApplyWrite {
			continue
		}
		if err := writeAttemptChange(canonical, world, change); err != nil {
			return result, fmt.Errorf("%s: write %s: %w", workspaceApplyIncompleteCode, change.Path, err)
		}
		result.FilesWritten++
	}
	for i, change := range delta.Changes {
		if actions[i] != attemptApplyDelete {
			continue
		}
		if err := canonical.Remove(filepath.FromSlash(change.Path)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return result, fmt.Errorf("%s: delete %s: %w", workspaceApplyIncompleteCode, change.Path, err)
		}
		removeEmptiedParents(canonical, change.Path)
		result.FilesDeleted++
	}
	for _, change := range delta.Changes {
		current, err := observeAttemptEntry(canonical, change.Path)
		if err != nil || !current.sameState(change.After) {
			return result, fmt.Errorf("%s: %s does not match the attempt's final state after apply", workspaceApplyIncompleteCode, change.Path)
		}
	}
	return result, nil
}

// planAttemptWorkspaceApply classifies every change against the canonical
// project before any write, so a conflict leaves the project untouched.
func planAttemptWorkspaceApply(canonical *os.Root, changes []AttemptPathChange) ([]attemptApplyAction, error) {
	actions := make([]attemptApplyAction, len(changes))
	var conflicts []string
	for i, change := range changes {
		current, err := observeAttemptEntry(canonical, change.Path)
		if err != nil {
			conflicts = append(conflicts, change.Path)
			continue
		}
		switch {
		case current.sameState(change.After):
			actions[i] = attemptApplySkip // already applied
		case current.sameState(change.Before):
			if change.Op == AttemptPathDelete {
				actions[i] = attemptApplyDelete
			} else {
				actions[i] = attemptApplyWrite
			}
		default:
			conflicts = append(conflicts, change.Path)
		}
	}
	if len(conflicts) == 0 {
		return actions, nil
	}
	sort.Strings(conflicts)
	reported := conflicts
	if len(reported) > attemptWorkspaceConflictReportLimit {
		reported = reported[:attemptWorkspaceConflictReportLimit]
	}
	return nil, &AttemptWorkspaceConflictError{Paths: reported, Total: len(conflicts)}
}

// writeAttemptChange stages the world's final state for one path in a temp
// entry beside it and renames it into place.
func writeAttemptChange(canonical, world *os.Root, change AttemptPathChange) error {
	name := filepath.FromSlash(change.Path)
	if dir := path.Dir(change.Path); dir != "." {
		if err := canonical.MkdirAll(filepath.FromSlash(dir), 0o755); err != nil {
			return err
		}
	}
	temp, err := attemptApplyTempName(change.Path)
	if err != nil {
		return err
	}
	tempName := filepath.FromSlash(temp)
	switch change.After.Kind {
	case AttemptEntrySymlink:
		if err := canonical.Symlink(change.After.LinkTarget, tempName); err != nil {
			return err
		}
	case AttemptEntryFile:
		if err := copyAttemptFileVerified(world, canonical, name, tempName, change.After); err != nil {
			_ = canonical.Remove(tempName)
			return err
		}
	default:
		return fmt.Errorf("unsupported final kind %q", change.After.Kind)
	}
	if err := canonical.Rename(tempName, name); err != nil {
		_ = canonical.Remove(tempName)
		return err
	}
	return nil
}

// copyAttemptFileVerified copies a world file to a canonical temp file and
// fails if its bytes no longer match the delta, so a world edited after the
// delta was taken can never publish unreviewed content.
func copyAttemptFileVerified(world, canonical *os.Root, name, tempName string, want *AttemptManifestEntry) error {
	in, err := world.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := canonical.OpenFile(tempName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fs.FileMode(want.Mode))
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hash), in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != want.SHA256 {
		return fmt.Errorf("attempt world file changed after its delta was taken")
	}
	return canonical.Chmod(tempName, fs.FileMode(want.Mode))
}

// attemptApplyTempName names a staging entry with the version store's
// materialize prefix, so a crash leftover is never discovered as a project
// file or copied into another attempt world.
func attemptApplyTempName(rel string) (string, error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", err
	}
	return path.Join(path.Dir(rel), versionstore.MaterializeTempPrefix+hex.EncodeToString(suffix[:])), nil
}

// removeEmptiedParents removes directories a deletion left empty, stopping
// at the first non-empty directory or the project root.
func removeEmptiedParents(root *os.Root, rel string) {
	for dir := path.Dir(rel); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if err := root.Remove(filepath.FromSlash(dir)); err != nil {
			return
		}
	}
}
