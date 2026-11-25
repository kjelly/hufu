package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
)

var errAmbiguousFindingLine = errors.New("source excerpt matches equally near lines")

type findingSource struct {
	TaskID string `json:"task_id"`
	Index  int    `json:"finding_index"`
}

type canonicalFinding struct {
	Location         string          `json:"location"`
	SourceSide       string          `json:"source_side,omitempty"`
	SourceRevision   string          `json:"source_revision,omitempty"`
	Sources          []findingSource `json:"sources"`
	RequiresCritique bool            `json:"requires_critique"`
}

type findingEvidenceInventory struct {
	Revision string `json:"review_revision"`
	Sources  []struct {
		Path  string `json:"path"`
		Role  string `json:"role"`
		Spans []struct {
			Start         int64 `json:"bundle_start"`
			End           int64 `json:"bundle_end"`
			StartLine     int   `json:"start_line"`
			EndLine       int   `json:"end_line"`
			GoSyntaxLines []int `json:"go_syntax_lines"`
			FirstFullLine int   `json:"first_full_line"`
			LastFullLine  int   `json:"last_full_line"`
		} `json:"available_spans"`
	} `json:"sources"`
}

func canonicalDraftFindings(current sourceSession, tasks map[string]sourceTask, records map[string]record) ([]canonicalFinding, error) {
	ids := make([]string, 0, len(records))
	for id, record := range records {
		if record.Kind == "review" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	groups := []canonicalFinding{}
	index := make(map[[6]string]int)
	for _, id := range ids {
		record, err := resolveDraftLocations(current, tasks[id], records[id])
		if err != nil {
			return nil, err
		}
		for i, f := range record.Findings {
			key := [6]string{f.Location, f.SourceSide, f.SourceRevision, f.Severity, strings.TrimSpace(f.Summary), strings.TrimSpace(f.Detail)}
			position, found := index[key]
			if !found {
				position = len(groups)
				index[key] = position
				groups = append(groups, canonicalFinding{Location: f.Location, SourceSide: f.SourceSide, SourceRevision: f.SourceRevision})
			}
			groups[position].RequiresCritique = groups[position].RequiresCritique || f.RequiresCritique
			groups[position].Sources = append(groups[position].Sources, findingSource{TaskID: id, Index: i})
		}
	}
	return groups, nil
}

func resolveDraftLocations(current sourceSession, item sourceTask, review record) (record, error) {
	supplement := item.ContractID == "review-supplement-workset"
	if len(review.Findings) == 0 || item.Binding == nil || (item.ContractID != "review-primary-workset" && !supplement) {
		return review, nil
	}
	// Keep the accepted payload and its finding indices unchanged for audit.
	review.Findings = append([]finding(nil), review.Findings...)
	var inventory *findingEvidenceInventory
	var bundle []byte
	var deletions deletedFindingLines
	for i, f := range review.Findings {
		// Historical source records can lack a path list. Their original
		// location is still checked by their frozen coverage policy.
		if len(item.Binding.TouchedPaths) == 0 {
			if supplement || f.SourceExcerpt != "" || f.SourceSide != "" || f.SourceRevision != "" {
				return review, fmt.Errorf("finding %d has no assigned source", i)
			}
			continue
		}
		name, line, err := validateDraftLocation(f, item.Binding.TouchedPaths, supplement)
		if err != nil {
			return review, fmt.Errorf("finding %d %w", i, err)
		}
		var headLine int
		if f.SourceSide != "old" {
			if inventory == nil {
				inventory, bundle, err = readFindingEvidence(current, item, review.Revision)
				if err != nil {
					return review, err
				}
			}
			if f.SourceExcerpt != "" {
				headLine, err = snapFindingLine(inventory, bundle, name, line, f.SourceExcerpt)
				if errors.Is(err, errAmbiguousFindingLine) {
					return review, fmt.Errorf("finding %d location %q: %w", i, f.Location, err)
				}
			} else if verifyFindingLine(inventory, bundle, name, line) == nil {
				headLine = line
			}
		}
		var oldLine deletedSourceLine
		var oldMatch, ambiguous bool
		if !supplement && f.SourceSide != "head" && citesAssignedDiff(item, f.Evidence) {
			if deletions == nil {
				deletions, err = readDeletedFindingLines(current, item)
				if err != nil {
					return review, err
				}
			}
			oldLine, oldMatch, ambiguous = deletions.resolve(name, line, f.SourceExcerpt, f.SourceRevision)
		}
		switch {
		case headLine > 0 && oldMatch:
			return review, fmt.Errorf("finding %d location %q matches both sides; specify source_side", i, f.Location)
		case ambiguous:
			return review, fmt.Errorf("finding %d location %q matches multiple deleted source lines; specify source_revision and an exact excerpt when available", i, f.Location)
		case oldMatch:
			line = oldLine.number
			review.Findings[i].SourceSide = "old"
			review.Findings[i].SourceRevision = oldLine.revision
		case headLine > 0:
			line = headLine
			review.Findings[i].SourceSide = "head"
		default:
			return review, fmt.Errorf("finding %d location %q is outside the cited frozen source side", i, f.Location)
		}
		review.Findings[i].Location = name + ":" + strconv.Itoa(line)
	}
	return review, nil
}

func validateDraftLocation(f finding, assigned []string, supplement bool) (string, int, error) {
	name, text, ok := splitSnapshotLocation(strings.TrimSpace(f.Location))
	line, err := strconv.Atoi(strings.TrimSpace(text))
	if !ok || err != nil || line < 1 {
		return "", 0, fmt.Errorf("has an invalid source location %q", f.Location)
	}
	name = canonicalDraftPath(name, assigned)
	if name == "" {
		return "", 0, fmt.Errorf("location %q is outside the assigned files", f.Location)
	}
	if f.SourceExcerpt != "" && path.Ext(name) != ".go" {
		return "", 0, errors.New("source excerpt requires a Go file")
	}
	if f.SourceSide != "" && f.SourceSide != "head" && f.SourceSide != "old" {
		return "", 0, fmt.Errorf("unknown source side %q", f.SourceSide)
	}
	if supplement && f.SourceSide == "old" {
		return "", 0, errors.New("supplement requires the frozen head source")
	}
	if f.SourceRevision != "" && (f.SourceSide != "old" || !validOldRevision(f.SourceRevision)) {
		return "", 0, errors.New("source_revision requires a full old-side Git revision")
	}
	return name, line, nil
}

func canonicalDraftPath(value string, assigned []string) string {
	value = strings.ReplaceAll(strings.Trim(strings.TrimSpace(value), "`"), "\\", "/")
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return ""
		}
	}
	name := path.Clean(value)
	candidate := ""
	for _, assignedPath := range assigned {
		if name == assignedPath {
			return name
		}
		if path.IsAbs(name) && strings.HasSuffix(name, "/"+assignedPath) {
			if candidate != "" {
				return ""
			}
			candidate = assignedPath
		}
	}
	return candidate
}

func readFindingEvidence(current sourceSession, item sourceTask, revision string) (*findingEvidenceInventory, []byte, error) {
	var inventory findingEvidenceInventory
	var bundle []byte
	for _, ref := range item.Binding.Inputs {
		if ref.Kind != "review_evidence_inventory" && ref.Kind != "review_source_snapshot" {
			continue
		}
		data, err := readAssignedFindingArtifact(current, item, ref)
		if err != nil {
			return nil, nil, err
		}
		switch ref.Kind {
		case "review_evidence_inventory":
			if inventory.Revision != "" {
				return nil, nil, fmt.Errorf("review %s has multiple evidence inventories", item.ID)
			}
			if err := json.Unmarshal(data, &inventory); err != nil {
				return nil, nil, err
			}
		case "review_source_snapshot":
			if bundle != nil {
				return nil, nil, fmt.Errorf("review %s has multiple source snapshots", item.ID)
			}
			bundle = data
		}
	}
	if bundle == nil || inventory.Revision == "" || inventory.Revision != revision {
		return nil, nil, fmt.Errorf("review %s lacks a same-revision source bundle and inventory", item.ID)
	}
	return &inventory, bundle, nil
}

func readAssignedFindingArtifact(current sourceSession, item sourceTask, ref sourceArtifactRef) ([]byte, error) {
	if !observed(item, []string{ref.ID}) {
		return nil, fmt.Errorf("review %s did not observe %s", item.ID, ref.Kind)
	}
	for _, producer := range current.Tasks {
		if producer.ID == ref.TaskID && producer.VerifiedOutputs {
			// The assignment names immutable bytes. An invocation's working
			// copy can move or share a content ID with another occurrence.
			return readSnapshotArtifact(current.ArtifactDataDirectory, snapshotInventoryReference{
				ID: ref.ID, Kind: ref.Kind, SHA256: ref.SHA256, Bytes: ref.Bytes,
			})
		}
	}
	return nil, fmt.Errorf("review %s lacks a verified producer for %s", item.ID, ref.Kind)
}

func verifyFindingLine(inventory *findingEvidenceInventory, bundle []byte, name string, line int) error {
	for _, source := range inventory.Sources {
		if source.Path != name || source.Role != "primary" {
			continue
		}
		for _, span := range source.Spans {
			if span.Start >= 0 && span.End > span.Start && span.End <= int64(len(bundle)) &&
				span.StartLine >= 1 && line >= span.StartLine && line <= span.EndLine {
				return nil
			}
		}
	}
	return fmt.Errorf("line is outside the assigned frozen source spans")
}

func snapFindingLine(inventory *findingEvidenceInventory, bundle []byte, name string, target int, excerpt string) (int, error) {
	match := 0
	bestDistance := 3
	ambiguous := false
	needle := strings.TrimSpace(excerpt)
	if needle == "" || strings.ContainsAny(needle, "\r\n") {
		return 0, fmt.Errorf("source excerpt must quote one non-empty source line")
	}
	for _, source := range inventory.Sources {
		if source.Path != name || source.Role != "primary" {
			continue
		}
		for _, span := range source.Spans {
			if span.Start < 0 || span.End <= span.Start || span.End > int64(len(bundle)) || span.StartLine < 1 || span.EndLine < span.StartLine {
				return 0, fmt.Errorf("invalid frozen source span")
			}
			if span.StartLine-target > 2 || target-span.EndLine > 2 {
				continue
			}
			sourceText := string(bundle[span.Start:span.End])
			for offset, lineText := range strings.Split(sourceText, "\n") {
				line := span.StartLine + offset
				if line-target > 2 || target-line > 2 || line > span.EndLine || strings.TrimSpace(lineText) != needle {
					continue
				}
				if line == target && span.FirstFullLine > 0 && line >= span.FirstFullLine && line <= span.LastFullLine {
					return line, nil
				}
				index := sort.SearchInts(span.GoSyntaxLines, line)
				if index == len(span.GoSyntaxLines) || span.GoSyntaxLines[index] != line {
					continue
				}
				distance := findingLineDistance(line, target)
				switch {
				case distance < bestDistance:
					match, bestDistance, ambiguous = line, distance, false
				case distance == bestDistance && line != match:
					ambiguous = true
				}
			}
		}
	}
	if ambiguous {
		return 0, errAmbiguousFindingLine
	}
	if match == 0 {
		return 0, fmt.Errorf("source excerpt does not match an assigned Go syntax line within two lines")
	}
	return match, nil
}

func findingLineDistance(line, target int) int {
	if line < target {
		return target - line
	}
	return line - target
}
