package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Coverage is computed from accepted task bindings and the producer inventory,
// never reconstructed or copied by a model. Keep wire compatibility with the
// embedded interpreter's json v1 support.
type snapshotInventoryFile struct {
	Path        string `json:"path"`
	Mode        string `json:"mode"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	Lines       int    `json:"lines"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason"`
}

type snapshotUnfinished struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

type snapshotCoverage struct {
	Mode          string               `json:"mode"`
	SnapshotID    string               `json:"snapshot_id"`
	ContentSHA256 string               `json:"content_sha256"`
	Revision      string               `json:"revision"`
	Batch         int                  `json:"batch"`
	BatchCount    int                  `json:"batch_count"`
	Expected      []string             `json:"expected"`
	Completed     []string             `json:"completed"`
	Excluded      []snapshotUnfinished `json:"excluded"`
	Uncompleted   []snapshotUnfinished `json:"uncompleted"`
}

type snapshotAttestation struct {
	Requested struct {
		Kind string `json:"kind"`
	} `json:"requested"`
	Resolved struct {
		Head string `json:"head"`
	} `json:"resolved"`
	Satisfied bool              `json:"satisfied"`
	Snapshot  *snapshotIdentity `json:"repository_snapshot"`
}

type snapshotIdentity struct {
	ID         string                  `json:"snapshot_id"`
	Revision   string                  `json:"revision"`
	Hash       string                  `json:"content_sha256"`
	Batch      int                     `json:"batch"`
	BatchCount int                     `json:"batch_count"`
	Files      []snapshotInventoryFile `json:"files"`
}

func collectSnapshotCoverage(current session, runID string) (snapshotCoverage, error) {
	result := snapshotCoverage{Mode: "diff", Expected: []string{}, Completed: []string{}, Excluded: []snapshotUnfinished{}, Uncompleted: []snapshotUnfinished{}}
	scope, err := acceptedSnapshotScope(current, runID)
	if err != nil || scope == nil {
		return result, err
	}
	identity := scope.Snapshot
	files, err := checkedSnapshotFiles(scope)
	if err != nil {
		return result, err
	}
	result.Mode, result.SnapshotID, result.ContentSHA256, result.Revision = "repository_snapshot", identity.ID, identity.Hash, identity.Revision
	result.Batch, result.BatchCount = identity.Batch, identity.BatchCount
	covered, limited, err := checkedSnapshotReviews(current, runID, identity, files)
	if err != nil {
		return result, err
	}
	if err := checkedSnapshotVerifier(current, runID, identity); err != nil {
		return result, err
	}
	for _, file := range identity.Files {
		switch file.Disposition {
		case "selected":
			result.Expected = append(result.Expected, file.Path)
			if !covered[file.Path] {
				return result, fmt.Errorf("selected file %s has no accepted review", file.Path)
			}
			if limited[file.Path] {
				result.Uncompleted = append(result.Uncompleted, snapshotUnfinished{Path: file.Path, Reason: "review_evidence_gap"})
			} else {
				result.Completed = append(result.Completed, file.Path)
			}
		case "unreviewed":
			result.Expected = append(result.Expected, file.Path)
			result.Uncompleted = append(result.Uncompleted, snapshotUnfinished{Path: file.Path, Reason: file.Reason})
		case "excluded":
			result.Excluded = append(result.Excluded, snapshotUnfinished{Path: file.Path, Reason: file.Reason})
		}
	}
	return result, nil
}

func acceptedSnapshotScope(current session, runID string) (*snapshotAttestation, error) {
	var scope *snapshotAttestation
	for _, item := range current.Tasks {
		if item.ContractID != "produce-workset" || item.Result == nil {
			continue
		}
		var candidate snapshotAttestation
		if err := json.Unmarshal(item.Result.RuntimeOutputs["scope"], &candidate); err != nil {
			return nil, err
		}
		if candidate.Requested.Kind != "repository_snapshot" {
			continue
		}
		if scope != nil || item.Status != "done" || item.Result.Status != "success" || item.Receipt == nil || item.Receipt.RunID != runID || item.Receipt.TaskID != item.ID || current.SnapshotID == "" || item.SnapshotID != current.SnapshotID {
			return nil, fmt.Errorf("snapshot producer lacks current accepted identity")
		}
		scope = &candidate
	}
	return scope, nil
}

func checkedSnapshotFiles(scope *snapshotAttestation) (map[string]snapshotInventoryFile, error) {
	identity := scope.Snapshot
	if !scope.Satisfied || identity == nil || len(identity.Hash) != 64 || identity.ID != "sha256:"+identity.Hash || identity.Revision != scope.Resolved.Head || identity.Batch < 1 || identity.BatchCount < identity.Batch {
		return nil, fmt.Errorf("invalid snapshot identity")
	}
	files := make(map[string]snapshotInventoryFile)
	digest := sha256.New()
	for _, file := range identity.Files {
		if file.Path == "" || len(file.SHA256) != 64 {
			return nil, fmt.Errorf("invalid snapshot file")
		}
		if _, exists := files[file.Path]; exists {
			return nil, fmt.Errorf("duplicate snapshot file %s", file.Path)
		}
		switch file.Disposition {
		case "selected", "excluded", "unreviewed":
		default:
			return nil, fmt.Errorf("unknown snapshot file disposition")
		}
		if file.Disposition != "selected" && file.Reason == "" {
			return nil, fmt.Errorf("snapshot exclusion or gap lacks a reason")
		}
		entry, err := json.Marshal([]any{file.Path, file.Mode, file.SHA256, file.Bytes})
		if err != nil {
			return nil, err
		}
		if _, err := digest.Write(append(entry, '\n')); err != nil {
			return nil, err
		}
		files[file.Path] = file
	}
	if hex.EncodeToString(digest.Sum(nil)) != identity.Hash {
		return nil, fmt.Errorf("snapshot content inventory hash mismatch")
	}
	return files, nil
}

func checkedSnapshotReviews(current session, runID string, identity *snapshotIdentity, files map[string]snapshotInventoryFile) (map[string]bool, map[string]bool, error) {
	covered, limited := make(map[string]bool), make(map[string]bool)
	for _, item := range current.Tasks {
		switch item.ContractID {
		case "review-primary-workset", "review-documentation-workset", "review-documentation-escalation":
		default:
			continue
		}
		value, err := accepted(item, current.SnapshotID, runID)
		if err != nil {
			return nil, nil, err
		}
		if item.Binding == nil || item.Binding.Bindings["review_mode"] != "repository_snapshot" || item.Binding.Bindings["snapshot_id"] != identity.ID || item.Binding.Bindings["content_sha256"] != identity.Hash || item.Binding.Bindings["review_revision"] != identity.Revision {
			return nil, nil, fmt.Errorf("review %s uses a different repository snapshot", item.ID)
		}
		var review record
		if err := json.Unmarshal(value.Record, &review); err != nil {
			return nil, nil, err
		}
		if err := verifyReview(item, review); err != nil {
			return nil, nil, err
		}
		if err := checkSnapshotReviewFiles(item, review, files, covered, limited); err != nil {
			return nil, nil, err
		}
	}
	return covered, limited, nil
}

func checkSnapshotReviewFiles(item task, review record, files map[string]snapshotInventoryFile, covered, limited map[string]bool) error {
	observedSource := false
	for _, input := range item.Binding.Inputs {
		if input.Kind == "review_source_snapshot" && input.SHA256 != "" && observed(item, []string{input.ID}) {
			observedSource = true
		}
	}
	if len(item.Binding.TouchedPaths) > 0 && !observedSource {
		return fmt.Errorf("review %s did not observe its source snapshot", item.ID)
	}
	assigned := make(map[string]bool)
	for _, name := range item.Binding.TouchedPaths {
		file, ok := files[name]
		if !ok || file.Disposition != "selected" || covered[name] {
			return fmt.Errorf("review %s substituted or duplicated snapshot file %s", item.ID, name)
		}
		covered[name], assigned[name] = true, true
		limited[name] = len(review.Gaps) > 0
	}
	for _, finding := range review.Findings {
		name, lineText, ok := splitSnapshotLocation(finding.Location)
		line, err := strconv.Atoi(lineText)
		file := files[name]
		if !ok || err != nil || !assigned[name] || line < 1 || line > file.Lines {
			return fmt.Errorf("finding location %q is outside the assigned snapshot", finding.Location)
		}
	}
	return nil
}

func checkedSnapshotVerifier(current session, runID string, identity *snapshotIdentity) error {
	found := false
	for _, item := range current.Tasks {
		if item.ContractID != "verify-targeted-go-tests" {
			continue
		}
		if found || item.Status != "done" || item.Result == nil || item.Result.Status != "success" || item.Receipt == nil || item.Receipt.RunID != runID || item.Receipt.TaskID != item.ID || item.SnapshotID != current.SnapshotID {
			return fmt.Errorf("missing or ambiguous snapshot verifier")
		}
		found = true
		var tests struct {
			SnapshotID string `json:"snapshot_id"`
			Hash       string `json:"content_sha256"`
			Revision   string `json:"reviewed_revision"`
		}
		if err := json.Unmarshal(item.Result.RuntimeOutputs["targeted_go_tests"], &tests); err != nil {
			return err
		}
		if tests.SnapshotID != identity.ID || tests.Hash != identity.Hash || tests.Revision != identity.Revision {
			return fmt.Errorf("snapshot verifier identity differs from reviewer identity")
		}
	}
	if !found {
		return fmt.Errorf("snapshot verifier missing")
	}
	return nil
}

// Split from the right so repository paths containing ':' remain valid. A
// finding uses exactly one original source line, never a synthetic patch line.
func splitSnapshotLocation(location string) (string, string, bool) {
	index := strings.LastIndex(location, ":")
	if index < 1 || index == len(location)-1 {
		return "", "", false
	}
	return location[:index], location[index+1:], true
}
