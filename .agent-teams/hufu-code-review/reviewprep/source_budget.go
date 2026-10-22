package main

import (
	"bytes"
	"fmt"
	"path/filepath"
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

// This is a conservative planning estimate, not provider usage. Charging one
// token per two bytes includes UTF-8 bytes and artifact framing. The team's
// 500k attempt guard leaves room for instructions, reasoning, result submission,
// and repairs beyond the 200k input allowance.
func estimateReviewInputTokens(byteCount int) int {
	return byteCount/2 + byteCount%2
}

// Refine the diff-only batches before publishing any artifacts or scope
// attestation. Every prospective item uses the same immutable bundle and
// reference report bytes that will be written, including their final item key.
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
	key := fmt.Sprintf("unit-%04d", len(result))
	directory := filepath.Join(outputDir, "batches", key)
	partition, err := filepath.Rel(artifactRoot, directory)
	if err != nil {
		return nil, fmt.Errorf("identify source snapshot partition: %w", err)
	}
	source, err := snapshot.bundleData(filepath.ToSlash(partition), current.paths)
	if err != nil {
		return nil, err
	}
	tokens := estimateReviewInputTokens(len(source)) + estimateReviewInputTokens(current.diff.Len())
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
		return append(result, current), nil
	}
	left, right := splitReviewBatch(current)
	if left == nil {
		return nil, fmt.Errorf("workset_item_too_large: paths=%v source_snapshot_bytes=%d estimated_review_input_tokens=%d; configured caps: max_source_snapshot_bytes=%d max_review_input_tokens=%d; remediation: split the primary source file or raise an explicit input budget with attempt headroom",
			current.paths, len(source), tokens, config.MaxSourceSnapshotBytes, config.MaxReviewInputTokens)
	}
	result, err = appendBoundReviewBatch(result, left, artifactRoot, outputDir, snapshot, config, route, verification)
	if err != nil {
		return nil, err
	}
	return appendBoundReviewBatch(result, right, artifactRoot, outputDir, snapshot, config, route, verification)
}

// Split on primary paths first so large source files are not needlessly
// duplicated. If only one path remains, split its diff hunks; an indivisible
// source/hunk pair fails closed rather than truncating primary evidence.
func splitReviewBatch(current *batch) (*batch, *batch) {
	if len(current.parts) < 2 {
		return nil, nil
	}
	left := &batch{lens: current.lens, pathSeen: make(map[string]struct{}), preamble: current.preamble}
	right := &batch{lens: current.lens, pathSeen: make(map[string]struct{}), preamble: current.preamble}
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
	}
	return left, right
}
