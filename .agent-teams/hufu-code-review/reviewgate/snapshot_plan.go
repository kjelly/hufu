package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// Wire types for the producer's receipt-bound inventory. Keep their JSON
// encoding compatible with the trusted-static action interpreter.
type snapshotPlan struct {
	Worksets          []snapshotPlannedWorkset `json:"worksets"`
	BatchSourceSHA256 []string                 `json:"batch_source_sha256"`
}

type snapshotPlannedWorkset struct {
	Sources []snapshotPlannedSource `json:"sources"`
}

type snapshotPlannedSource struct {
	Path  string         `json:"path"`
	Spans []snapshotSpan `json:"spans"`
}

type snapshotSpan struct {
	Start     int64 `json:"start"`
	End       int64 `json:"end"`
	StartLine int   `json:"start_line"`
	EndLine   int   `json:"end_line"`
}

type snapshotPageWorkset struct {
	number   int
	source   string
	files    map[string][]snapshotSpan
	accepted bool
	limited  bool
}

type snapshotPage struct {
	planHash string
	worksets []*snapshotPageWorkset
	byKey    map[string]*snapshotPageWorkset
	outside  map[string]bool
	noops    map[string]bool
}

func checkSnapshotPlan(identity *snapshotIdentity, files map[string]snapshotInventoryFile) (*snapshotPage, error) {
	plan, quota := identity.Plan, identity.WorksetsPerBatch
	if quota < 1 || quota > 30 || plan.Worksets == nil || plan.BatchSourceSHA256 == nil {
		return nil, fmt.Errorf("invalid snapshot workset plan")
	}
	count := (len(plan.Worksets) + quota - 1) / quota
	if count == 0 {
		count = 1
	}
	if count != identity.BatchCount {
		return nil, fmt.Errorf("snapshot workset plan differs from batch count")
	}
	start, end := (identity.Batch-1)*quota, identity.Batch*quota
	if end > len(plan.Worksets) {
		end = len(plan.Worksets)
	}
	if len(plan.BatchSourceSHA256) != end-start {
		return nil, fmt.Errorf("snapshot workset plan differs from batch source count")
	}
	page := &snapshotPage{
		byKey: make(map[string]*snapshotPageWorkset), outside: make(map[string]bool), noops: make(map[string]bool),
	}
	assigned := make(map[string][]snapshotSpan)
	selected := make(map[string]bool)
	for index, workset := range plan.Worksets {
		inPage := index >= start && index < end
		members, err := checkSnapshotWorkset(workset, files)
		if err != nil {
			return nil, err
		}
		for name, spans := range members {
			assigned[name] = append(assigned[name], spans...)
			if inPage {
				selected[name] = true
			} else {
				page.outside[name] = true
			}
		}
		if inPage {
			offset := index - start
			hash := plan.BatchSourceSHA256[offset]
			if _, err := hex.DecodeString(hash); err != nil || len(hash) != 64 {
				return nil, fmt.Errorf("invalid snapshot workset %d source hash", index+1)
			}
			entry := &snapshotPageWorkset{number: index + 1, source: hash, files: members}
			page.worksets = append(page.worksets, entry)
			page.byKey[fmt.Sprintf("unit-%04d", offset)] = entry
		}
	}
	for _, file := range identity.Files {
		if file.Disposition == "excluded" {
			continue
		}
		spans, exists := assigned[file.Path]
		if !exists || selected[file.Path] != (file.Disposition == "selected") || !coversSnapshotFile(file.Bytes, spans) {
			return nil, fmt.Errorf("snapshot workset plan does not account for %q", file.Path)
		}
	}
	encoded, err := json.Marshal(plan.Worksets)
	if err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("%s\n%d\n", identity.ID, quota)
	digest := sha256.Sum256(append([]byte(prefix), encoded...))
	page.planHash = hex.EncodeToString(digest[:])
	return page, nil
}

func checkSnapshotWorkset(workset snapshotPlannedWorkset, files map[string]snapshotInventoryFile) (map[string][]snapshotSpan, error) {
	if len(workset.Sources) == 0 {
		return nil, fmt.Errorf("snapshot workset has no primary sources")
	}
	members := make(map[string][]snapshotSpan)
	for _, source := range workset.Sources {
		file, exists := files[source.Path]
		_, duplicate := members[source.Path]
		if !exists || duplicate || file.Disposition == "excluded" || source.Spans == nil || (len(source.Spans) == 0) != (file.Bytes == 0) {
			return nil, fmt.Errorf("invalid snapshot workset source %q", source.Path)
		}
		var lastEnd int64
		for _, span := range source.Spans {
			if span.Start < lastEnd || span.End <= span.Start || span.End > file.Bytes ||
				span.StartLine < 1 || span.EndLine < span.StartLine || span.EndLine > file.Lines {
				return nil, fmt.Errorf("invalid snapshot source range for %q", source.Path)
			}
			lastEnd = span.End
		}
		members[source.Path] = source.Spans
	}
	return members, nil
}

func coversSnapshotFile(size int64, spans []snapshotSpan) bool {
	sort.Slice(spans, func(i, j int) bool { return spans[i].Start < spans[j].Start })
	var end int64
	for _, span := range spans {
		if span.Start > end {
			return false
		}
		if span.End > end {
			end = span.End
		}
	}
	return end == size
}

func (page *snapshotPage) checkReview(item sourceTask, review record, covered, limited map[string]bool) error {
	if review.Lens == "noop" && len(item.Binding.TouchedPaths) == 0 && len(review.Findings) == 0 {
		if page.noops[item.ContractID] || item.ContractID == "review-primary-workset" && len(page.worksets) > 0 {
			return fmt.Errorf("unexpected snapshot no-op review %s", item.ID)
		}
		page.noops[item.ContractID] = true
		return nil
	}
	workset := page.byKey[review.Key]
	if item.ContractID != "review-primary-workset" || workset == nil || workset.accepted ||
		len(item.Binding.TouchedPaths) != len(workset.files) {
		return fmt.Errorf("unexpected or duplicated snapshot workset review %s", item.ID)
	}
	sourceID := "sha256-" + workset.source
	observedSource := false
	for _, input := range item.Binding.Inputs {
		if input.Kind == "review_source_snapshot" && input.SHA256 == workset.source && input.ID == sourceID {
			observedSource = observed(item, []string{sourceID})
			break
		}
	}
	if !observedSource {
		return fmt.Errorf("review %s did not observe its planned snapshot source", item.ID)
	}
	seen := make(map[string]bool)
	for _, name := range item.Binding.TouchedPaths {
		if _, allowed := workset.files[name]; !allowed || seen[name] {
			return fmt.Errorf("review %s substituted a planned snapshot path %q", item.ID, name)
		}
		seen[name] = true
		covered[name] = true
		limited[name] = limited[name] || len(review.Gaps) > 0
	}
	for _, finding := range review.Findings {
		name, lineText, ok := splitSnapshotLocation(finding.Location)
		line, err := strconv.Atoi(lineText)
		if !ok || err != nil || !snapshotLineAssigned(workset.files[name], line) {
			return fmt.Errorf("finding location %q is outside the assigned snapshot fragment", finding.Location)
		}
	}
	workset.accepted, workset.limited = true, len(review.Gaps) > 0
	return nil
}

func snapshotLineAssigned(spans []snapshotSpan, line int) bool {
	for _, span := range spans {
		if line >= span.StartLine && line <= span.EndLine {
			return true
		}
	}
	return false
}
