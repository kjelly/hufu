package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

const (
	workspaceIsolationUnsupportedCode = "workspace_isolation_unsupported"
	workspaceIsolationUnavailableCode = "workspace_isolation_unavailable"
	workspaceDeltaRejectedCode        = "workspace_delta_rejected"
	workspaceConflictCode             = "workspace_conflict"
	workspaceApplyIncompleteCode      = "workspace_apply_incomplete"

	// attemptWorkspaceMaxChanges bounds one attempt's delta.
	attemptWorkspaceMaxChanges = 50000
)

// Attempt path change operations.
const (
	AttemptPathAdd    = "add"
	AttemptPathModify = "modify"
	AttemptPathDelete = "delete"
)

// AttemptPathChange is one path an isolated attempt changed. Before is the
// state the attempt started from (nil for an add); After is its final state
// (nil for a delete).
type AttemptPathChange struct {
	Path   string                `json:"path"`
	Op     string                `json:"op"`
	Before *AttemptManifestEntry `json:"before,omitempty"`
	After  *AttemptManifestEntry `json:"after,omitempty"`
}

// AttemptWorkspaceDelta is what an isolated attempt changed relative to the
// canonical state it copied. It is applied to the canonical project only
// through applyAttemptWorkspaceDelta's per-path preconditions.
type AttemptWorkspaceDelta struct {
	WorldID        string              `json:"world_id"`
	BaselineDigest string              `json:"baseline_digest"`
	FinalDigest    string              `json:"final_digest"`
	Changes        []AttemptPathChange `json:"changes"`
	Digest         string              `json:"digest"`
}

// computeAttemptWorkspaceDelta diffs the baseline an attempt copied against
// its final state. A kind change (file to symlink or back) is a modify.
func computeAttemptWorkspaceDelta(worldID string, baseline, final attemptManifest) *AttemptWorkspaceDelta {
	paths := make(map[string]struct{}, len(baseline)+len(final))
	for rel := range baseline {
		paths[rel] = struct{}{}
	}
	for rel := range final {
		paths[rel] = struct{}{}
	}
	sorted := make([]string, 0, len(paths))
	for rel := range paths {
		sorted = append(sorted, rel)
	}
	sort.Strings(sorted)
	delta := &AttemptWorkspaceDelta{WorldID: worldID, BaselineDigest: baseline.digest(), FinalDigest: final.digest(), Changes: []AttemptPathChange{}}
	for _, rel := range sorted {
		before, hadBefore := baseline[rel]
		after, hasAfter := final[rel]
		switch {
		case hadBefore && hasAfter:
			if before.sameState(&after) {
				continue
			}
			delta.Changes = append(delta.Changes, AttemptPathChange{Path: rel, Op: AttemptPathModify, Before: before.clone(), After: after.clone()})
		case hadBefore:
			delta.Changes = append(delta.Changes, AttemptPathChange{Path: rel, Op: AttemptPathDelete, Before: before.clone()})
		default:
			delta.Changes = append(delta.Changes, AttemptPathChange{Path: rel, Op: AttemptPathAdd, After: after.clone()})
		}
	}
	delta.Digest = attemptWorkspaceChangesDigest(delta.Changes)
	return delta
}

func attemptWorkspaceChangesDigest(changes []AttemptPathChange) string {
	encoded, _ := json.Marshal(changes)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// validateAttemptWorkspaceDelta rejects a delta the runtime must never apply:
// a path that is not a clean relative path, a path through .git, a special
// file, a symlink that leaves the project, or an oversized change set.
func validateAttemptWorkspaceDelta(delta *AttemptWorkspaceDelta) error {
	if delta == nil {
		return fmt.Errorf("%s: delta is missing", workspaceDeltaRejectedCode)
	}
	if len(delta.Changes) > attemptWorkspaceMaxChanges {
		return fmt.Errorf("%s: %d changed paths exceed the %d path limit", workspaceDeltaRejectedCode, len(delta.Changes), attemptWorkspaceMaxChanges)
	}
	if delta.Digest != attemptWorkspaceChangesDigest(delta.Changes) {
		return fmt.Errorf("%s: delta digest does not match its changes", workspaceDeltaRejectedCode)
	}
	for _, change := range delta.Changes {
		rel := change.Path
		if rel == "" || rel != path.Clean(rel) || path.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "\\") {
			return fmt.Errorf("%s: %q is not a clean relative path", workspaceDeltaRejectedCode, rel)
		}
		if attemptPathHasGitSegment(rel) {
			return fmt.Errorf("%s: %q is inside .git", workspaceDeltaRejectedCode, rel)
		}
		switch change.Op {
		case AttemptPathAdd, AttemptPathModify:
			if change.After == nil || change.After.Path != rel {
				return fmt.Errorf("%s: %q has no final state", workspaceDeltaRejectedCode, rel)
			}
			switch change.After.Kind {
			case AttemptEntryFile:
			case AttemptEntrySymlink:
				if !symlinkTargetStaysInRoot(rel, change.After.LinkTarget) {
					return fmt.Errorf("%s: symlink %q points outside the project (%q)", workspaceDeltaRejectedCode, rel, change.After.LinkTarget)
				}
			default:
				return fmt.Errorf("%s: %q is a special file", workspaceDeltaRejectedCode, rel)
			}
			if change.Op == AttemptPathModify && change.Before == nil {
				return fmt.Errorf("%s: modified %q has no baseline state", workspaceDeltaRejectedCode, rel)
			}
		case AttemptPathDelete:
			if change.Before == nil || change.After != nil {
				return fmt.Errorf("%s: deleted %q is malformed", workspaceDeltaRejectedCode, rel)
			}
		default:
			return fmt.Errorf("%s: %q has unknown operation %q", workspaceDeltaRejectedCode, rel, change.Op)
		}
	}
	return nil
}
