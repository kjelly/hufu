package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	Mode              string               `json:"mode"`
	SnapshotID        string               `json:"snapshot_id"`
	ContentSHA256     string               `json:"content_sha256"`
	Revision          string               `json:"revision"`
	Batch             int                  `json:"batch"`
	BatchCount        int                  `json:"batch_count"`
	Expected          []string             `json:"expected"`
	Completed         []string             `json:"completed"`
	Excluded          []snapshotUnfinished `json:"excluded"`
	Uncompleted       []snapshotUnfinished `json:"uncompleted"`
	WorksetPlanSHA256 string               `json:"workset_plan_sha256,omitempty"`
	ReviewedWorksets  []int                `json:"reviewed_worksets,omitempty"`
}

const (
	snapshotCoverageArtifactPath = "snapshot-coverage.json"
	maxSnapshotArtifactBytes     = 32 * 1024 * 1024
)

type snapshotCoverageSummary struct {
	Mode          string                     `json:"mode"`
	SnapshotID    string                     `json:"snapshot_id"`
	ContentSHA256 string                     `json:"content_sha256"`
	Revision      string                     `json:"revision"`
	Batch         int                        `json:"batch"`
	BatchCount    int                        `json:"batch_count"`
	Expected      int                        `json:"expected_count"`
	Completed     int                        `json:"completed_count"`
	Excluded      int                        `json:"excluded_count"`
	Uncompleted   int                        `json:"uncompleted_count"`
	Inventory     snapshotInventoryReference `json:"inventory"`
}

func summarizeSnapshotCoverage(coverage snapshotCoverage, data []byte) snapshotCoverageSummary {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	return snapshotCoverageSummary{
		Mode: coverage.Mode, SnapshotID: coverage.SnapshotID, ContentSHA256: coverage.ContentSHA256,
		Revision: coverage.Revision, Batch: coverage.Batch, BatchCount: coverage.BatchCount,
		Expected: len(coverage.Expected), Completed: len(coverage.Completed),
		Excluded: len(coverage.Excluded), Uncompleted: len(coverage.Uncompleted),
		Inventory: snapshotInventoryReference{ID: "sha256-" + hash, Kind: "repository_snapshot_coverage", Path: snapshotCoverageArtifactPath, SHA256: hash, Bytes: int64(len(data))},
	}
}

func verifySnapshotCoverage(current sourceSession, coverage snapshotCoverage, report json.RawMessage) error {
	data, err := json.Marshal(coverage)
	if err != nil {
		return err
	}
	// A historical handoff carries the full inline inventory. New handoffs
	// must bind the exact expected summary and preserve its complete artifact.
	for _, item := range current.Tasks {
		if item.ContractID != "inventory-review-handoffs" || !item.VerifiedOutputs || item.Result == nil {
			continue
		}
		bound := item.Result.RuntimeOutputs["snapshot_coverage"]
		if !equalJSON(data, bound) {
			summary := summarizeSnapshotCoverage(coverage, data)
			encoded, err := json.Marshal(summary)
			if err != nil {
				return err
			}
			if !equalJSON(encoded, bound) {
				return &sourceReviewError{cause: fmt.Errorf("task %s substituted /snapshot_coverage", item.ID)}
			}
			if _, err := readSnapshotArtifact(current.ArtifactDataDirectory, summary.Inventory); err != nil {
				return &sourceReviewError{cause: fmt.Errorf("task %s: /snapshot_coverage/inventory %s: %w", item.ID, summary.Inventory.ID, err)}
			}
		}
		if !equalJSON(bound, report) {
			return fmt.Errorf("report substituted /snapshot_coverage from task %s", item.ID)
		}
		return nil
	}
	return &sourceReviewError{cause: fmt.Errorf("/snapshot_coverage lacks a verified handoff source")}
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
	ID               string                      `json:"snapshot_id"`
	Revision         string                      `json:"revision"`
	Hash             string                      `json:"content_sha256"`
	Batch            int                         `json:"batch"`
	BatchCount       int                         `json:"batch_count"`
	Files            []snapshotInventoryFile     `json:"files"`
	FileCount        int                         `json:"file_count"`
	WorksetsPerBatch int                         `json:"worksets_per_batch"`
	Inventory        *snapshotInventoryReference `json:"inventory,omitempty"`
	Plan             *snapshotPlan               `json:"workset_plan,omitempty"`
}

type snapshotInventoryReference struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

func collectSnapshotCoverage(current sourceSession) (snapshotCoverage, error) {
	result := snapshotCoverage{Mode: "diff", Expected: []string{}, Completed: []string{}, Excluded: []snapshotUnfinished{}, Uncompleted: []snapshotUnfinished{}}
	scope, err := acceptedSnapshotScope(current)
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
	var page *snapshotPage
	if identity.Plan != nil {
		page, err = checkSnapshotPlan(identity, files)
		if err != nil {
			return result, err
		}
		result.WorksetPlanSHA256 = page.planHash
	}
	covered, limited, err := checkedSnapshotReviews(current, identity, files, page)
	if err != nil {
		return result, err
	}
	if err := checkedSnapshotVerifier(current, identity); err != nil {
		return result, err
	}
	if page != nil {
		for _, workset := range page.worksets {
			if !workset.accepted {
				return result, fmt.Errorf("snapshot workset %d has no accepted review", workset.number)
			}
			if !workset.limited {
				result.ReviewedWorksets = append(result.ReviewedWorksets, workset.number)
			}
		}
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
			} else if page != nil && page.outside[file.Path] {
				result.Uncompleted = append(result.Uncompleted, snapshotUnfinished{Path: file.Path, Reason: "outside_selected_batch"})
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

func acceptedSnapshotScope(current sourceSession) (*snapshotAttestation, error) {
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
		if scope != nil || !item.VerifiedOutputs {
			return nil, fmt.Errorf("snapshot producer lacks current accepted identity")
		}
		if candidate.Snapshot != nil && candidate.Snapshot.Inventory != nil {
			if err := loadSnapshotInventory(current.ArtifactDataDirectory, candidate.Snapshot); err != nil {
				return nil, err
			}
		} else if candidate.Snapshot != nil && candidate.Snapshot.Plan != nil {
			return nil, fmt.Errorf("snapshot workset plan requires a verified inventory artifact")
		}
		scope = &candidate
	}
	return scope, nil
}

// The producer's accepted receipt pins an inventory in the runtime CAS.
func loadSnapshotInventory(artifactDataDirectory string, summary *snapshotIdentity) error {
	ref := summary.Inventory
	if len(summary.Files) != 0 || summary.Plan != nil || ref.Kind != "repository_snapshot_inventory" {
		return fmt.Errorf("invalid snapshot inventory reference")
	}
	data, err := readSnapshotArtifact(artifactDataDirectory, *ref)
	if err != nil {
		return err
	}
	var full snapshotIdentity
	if err := json.Unmarshal(data, &full); err != nil {
		return err
	}
	if full.Inventory != nil || full.ID != summary.ID || full.Revision != summary.Revision || full.Hash != summary.Hash || full.Batch != summary.Batch || full.BatchCount != summary.BatchCount || full.WorksetsPerBatch != summary.WorksetsPerBatch || full.FileCount != summary.FileCount || len(full.Files) != summary.FileCount {
		return fmt.Errorf("snapshot inventory differs from accepted summary")
	}
	summary.Files = full.Files
	summary.Plan = full.Plan
	return nil
}

func readSnapshotArtifact(artifactDataDirectory string, ref snapshotInventoryReference) ([]byte, error) {
	if artifactDataDirectory == "" || len(ref.SHA256) != 64 || ref.ID != "sha256-"+ref.SHA256 || ref.Bytes < 1 || ref.Bytes > maxSnapshotArtifactBytes {
		return nil, fmt.Errorf("invalid snapshot artifact reference")
	}
	if _, err := hex.DecodeString(ref.SHA256); err != nil {
		return nil, fmt.Errorf("invalid snapshot artifact digest: %w", err)
	}
	name := filepath.Join(artifactDataDirectory, ref.ID)
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != ref.Bytes {
		return nil, fmt.Errorf("invalid verified snapshot artifact file")
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, fmt.Errorf("read snapshot artifact: %w", err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, ref.Bytes+1))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != ref.Bytes || hex.EncodeToString(digest[:]) != ref.SHA256 {
		return nil, fmt.Errorf("snapshot artifact hash or size mismatch")
	}
	return data, nil
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

func checkedSnapshotReviews(current sourceSession, identity *snapshotIdentity, files map[string]snapshotInventoryFile, page *snapshotPage) (map[string]bool, map[string]bool, error) {
	covered, limited := make(map[string]bool), make(map[string]bool)
	for _, item := range current.Tasks {
		switch item.ContractID {
		case "review-primary-workset", "review-documentation-workset", "review-documentation-escalation":
		default:
			continue
		}
		value, err := accepted(item)
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
		review, err = resolveDraftLocations(current, item, review)
		if err != nil {
			return nil, nil, err
		}
		if page != nil {
			err = page.checkReview(item, review, covered, limited)
		} else {
			err = checkSnapshotReviewFiles(item, review, files, covered, limited)
		}
		if err != nil {
			return nil, nil, err
		}
	}
	return covered, limited, nil
}

func checkSnapshotReviewFiles(item sourceTask, review record, files map[string]snapshotInventoryFile, covered, limited map[string]bool) error {
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

func checkedSnapshotVerifier(current sourceSession, identity *snapshotIdentity) error {
	found := false
	for _, item := range current.Tasks {
		if item.ContractID != "verify-targeted-go-tests" {
			continue
		}
		if found || !item.VerifiedOutputs || item.Result == nil {
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
