package main

import (
	"bytes"
	"fmt"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/tools"
)

const (
	defaultMaxSourceSnapshotBytes  = 384 * 1024
	defaultMaxReviewInputTokens    = 200_000
	maxRelatedSnapshotContentBytes = 64 * 1024
)

type batchPart struct {
	path  string
	diff  []byte
	lines int
}

// Conservative planning estimate including UTF-8 and artifact framing.
func estimateReviewInputTokens(byteCount int) int {
	return byteCount/2 + byteCount%2
}

func boundReviewBatches(artifactRoot, outputDir string, batches []*batch, snapshot *reviewSourceSnapshot, config Config, route string, verification *documentationVerification) ([]*batch, error) {
	var result []*batch
	for _, current := range batches {
		var err error
		result, err = appendBoundReviewBatch(result, current, artifactRoot, outputDir, snapshot, config, route, verification)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func appendBoundReviewBatch(result []*batch, current *batch, artifactRoot, outputDir string, snapshot *reviewSourceSnapshot, config Config, route string, verification *documentationVerification) ([]*batch, error) {
	if config.MaxWorksetItems > 0 && len(result) >= config.MaxWorksetItems {
		return nil, fmt.Errorf("scope_too_large: source partitioning exceeds max_workset_items=%d; remediation: narrow review.scope or split the review run", config.MaxWorksetItems)
	}
	key := fmt.Sprintf("unit-%04d", len(result))
	directory := filepath.Join(outputDir, "batches", key)
	partition, err := filepath.Rel(artifactRoot, directory)
	if err != nil {
		return nil, fmt.Errorf("identify source snapshot partition: %w", err)
	}
	// Keep caller/test content in every partition, while leaving most of each
	// finite item budget for primary source, inventory, and diff evidence.
	relatedBudget := maxRelatedSnapshotContentBytes
	if cap := config.MaxSourceSnapshotBytes / 8; cap < relatedBudget {
		relatedBudget = cap
	}
	if cap := config.MaxReviewInputTokens / 4; cap < relatedBudget {
		relatedBudget = cap
	}
	source, inventory, err := snapshot.bundleDataWithScopes(filepath.ToSlash(partition), current.paths, current.sourceScopes, relatedBudget)
	if err != nil {
		return nil, err
	}
	tokens := estimateReviewInputTokens(len(source)) + estimateReviewInputTokens(len(inventory)) + estimateReviewInputTokens(current.diff.Len())
	if verification != nil {
		entry := &item{Key: key, TouchedPaths: current.paths, Bindings: make(map[string]string)}
		report, reportErr := itemReferenceReportData(route, entry, *verification)
		if reportErr != nil {
			return nil, reportErr
		}
		tokens += estimateReviewInputTokens(len(report))
	}
	if len(source) <= config.MaxSourceSnapshotBytes && tokens <= config.MaxReviewInputTokens {
		current.sourceBundle = source
		current.evidenceInventory = inventory
		current.relatedBudget = relatedBudget
		return append(result, current), nil
	}
	left, right := splitReviewBatch(current)
	if left == nil && estimateReviewInputTokens(current.diff.Len()) > config.MaxReviewInputTokens/2 {
		left, right = splitDiffFragment(current)
	}
	if left == nil {
		left, right, err = splitPrimarySource(current, snapshot)
		if err != nil {
			return nil, err
		}
	}
	if left == nil {
		left, right = splitDiffFragment(current)
	}
	if left == nil {
		return nil, fmt.Errorf("workset_item_too_large: paths=%v source_snapshot_bytes=%d estimated_review_input_tokens=%d; configured caps: max_source_snapshot_bytes=%d max_review_input_tokens=%d; remediation: narrow the request or provide an explicit input budget sufficient for one source line and dependency inventory",
			current.paths, len(source), tokens, config.MaxSourceSnapshotBytes, config.MaxReviewInputTokens)
	}
	result, err = appendBoundReviewBatch(result, left, artifactRoot, outputDir, snapshot, config, route, verification)
	if err != nil {
		return nil, err
	}
	return appendBoundReviewBatch(result, right, artifactRoot, outputDir, snapshot, config, route, verification)
}

func cloneReviewBatch(current *batch) *batch {
	result := &batch{lens: current.lens, paths: append([]string(nil), current.paths...), pathSeen: make(map[string]struct{}), lines: current.lines, parts: append([]batchPart(nil), current.parts...), preamble: current.preamble, sourceScopes: make(map[string][]tools.ArtifactByteRange)}
	result.diff.Write(current.diff.Bytes())
	for _, path := range current.paths {
		result.pathSeen[path] = struct{}{}
	}
	for path, spans := range current.sourceScopes {
		result.sourceScopes[path] = append([]tools.ArtifactByteRange{}, spans...)
	}
	return result
}

// Paths and diff hunks remain the preferred boundaries. A later source split
// retains the complete assigned diff, callers, and focused-test inventory.
func splitReviewBatch(current *batch) (*batch, *batch) {
	if len(current.parts) < 2 {
		return nil, nil
	}
	left := &batch{lens: current.lens, pathSeen: make(map[string]struct{}), preamble: current.preamble, sourceScopes: make(map[string][]tools.ArtifactByteRange)}
	right := &batch{lens: current.lens, pathSeen: make(map[string]struct{}), preamble: current.preamble, sourceScopes: make(map[string][]tools.ArtifactByteRange)}
	for _, target := range []*batch{left, right} {
		target.diff.WriteString(current.preamble)
		target.lines = bytes.Count([]byte(current.preamble), []byte("\n"))
	}
	leftPaths := make(map[string]struct{})
	for _, path := range current.paths[:len(current.paths)/2] {
		leftPaths[path] = struct{}{}
	}
	for index, part := range current.parts {
		_, onLeft := leftPaths[part.path]
		if len(current.paths) == 1 {
			onLeft = index < len(current.parts)/2
		}
		target := right
		if onLeft {
			target = left
		}
		target.add(part.path, part.diff, part.lines)
		if scopes, ok := current.sourceScopes[part.path]; ok {
			target.sourceScopes[part.path] = append([]tools.ArtifactByteRange{}, scopes...)
		}
	}
	return left, right
}

// Partition the largest primary interval. Go declaration starts within the
// middle half are preferred; a large declaration falls back to original line
// boundaries. Overlap supplies adjacent context but always makes progress.
func splitPrimarySource(current *batch, snapshot *reviewSourceSnapshot) (*batch, *batch, error) {
	var chosen string
	var chosenData []byte
	var chosenSpans []tools.ArtifactByteRange
	chosenIndex := -1
	largest := int64(0)
	for _, path := range current.paths {
		data, err := snapshot.readRegularFile(path)
		if err != nil {
			continue
		} // unavailable/nonregular evidence is inventoried, not fabricated.
		spans, ok := current.sourceScopes[path]
		if !ok && len(data) > 0 {
			spans = []tools.ArtifactByteRange{{Start: 0, End: int64(len(data))}}
		}
		for index, span := range spans {
			if span.End-span.Start > largest {
				chosen, chosenData, chosenSpans, chosenIndex, largest = path, data, spans, index, span.End-span.Start
			}
		}
	}
	if chosenIndex < 0 {
		return nil, nil, nil
	}
	span := chosenSpans[chosenIndex]
	middle := span.Start + (span.End-span.Start)/2
	lower, upper := span.Start+(span.End-span.Start)/4, span.End-(span.End-span.Start)/4
	boundary := int64(-1)
	if strings.HasSuffix(chosen, ".go") {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, chosen, chosenData, parser.SkipObjectResolution)
		if err == nil {
			for _, declaration := range file.Decls {
				candidate := int64(fset.Position(declaration.Pos()).Offset)
				if candidate > lower && candidate < upper && (boundary < 0 || absSourceOffset(candidate-middle) < absSourceOffset(boundary-middle)) {
					boundary = candidate
				}
			}
		}
	}
	if boundary < 0 {
		for index := int(middle); index < int(span.End)-1; index++ {
			if chosenData[index] == '\n' {
				boundary = int64(index + 1)
				break
			}
		}
	}
	if boundary <= span.Start || boundary >= span.End {
		return nil, nil, nil
	}
	// Three source lines of overlap, limited to a quarter of this interval so
	// pathological long lines cannot reproduce the unchanged parent interval.
	leftEnd, rightStart := boundary, boundary
	for lines, index := 0, int(boundary); index < int(upper) && lines < 3; index++ {
		if chosenData[index] == '\n' {
			leftEnd = int64(index + 1)
			lines++
		}
	}
	for lines, index := 0, int(boundary)-2; index >= int(lower) && lines < 3; index-- {
		if chosenData[index] == '\n' {
			rightStart = int64(index + 1)
			lines++
		}
	}
	left, right := cloneReviewBatch(current), cloneReviewBatch(current)
	leftSpans := append([]tools.ArtifactByteRange{}, chosenSpans[:chosenIndex]...)
	leftSpans = append(leftSpans, tools.ArtifactByteRange{Start: span.Start, End: leftEnd})
	rightSpans := []tools.ArtifactByteRange{{Start: rightStart, End: span.End}}
	rightSpans = append(rightSpans, chosenSpans[chosenIndex+1:]...)
	left.sourceScopes[chosen], right.sourceScopes[chosen] = leftSpans, rightSpans
	return left, right, nil
}

func absSourceOffset(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// A very large new-file hunk also needs bounded diff evidence. Fragments retain
// the original patch header and hunk header, and explicitly identify their
// original patch byte interval; they are evidence, not executable patches.
func splitDiffFragment(current *batch) (*batch, *batch) {
	data := current.diff.Bytes()
	if len(data) < 256 {
		return nil, nil
	}
	start := bytes.Index(data, []byte("@@"))
	headerEnd := 0
	if start >= 0 {
		if end := bytes.IndexByte(data[start:], '\n'); end >= 0 {
			headerEnd = start + end + 1
		}
	}
	body := data[headerEnd:]
	cut := bytes.IndexByte(body[len(body)/2:], '\n')
	if cut < 0 {
		return nil, nil
	}
	cut += len(body)/2 + 1
	if cut >= len(body) {
		return nil, nil
	}
	left, right := cloneReviewBatch(current), cloneReviewBatch(current)
	for index, target := range []*batch{left, right} {
		target.diff.Reset()
		target.diff.Write(data[:headerEnd])
		from, to := 0, cut
		if index == 1 {
			from, to = cut, len(body)
		}
		fmt.Fprintf(&target.diff, "# review_diff_fragment original_parent_bytes=[%d,%d)\n", headerEnd+from, headerEnd+to)
		target.diff.Write(body[from:to])
		target.lines = bytes.Count(target.diff.Bytes(), []byte("\n"))
		target.parts = nil
	}
	if left.diff.Len() >= len(data) || right.diff.Len() >= len(data) {
		return nil, nil
	}
	return left, right
}
