package main

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

type evidenceRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type requestedEvidence struct {
	Path   string          `json:"path"`
	Ranges []evidenceRange `json:"ranges"`
}

type sourceLimitation struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}

type sourceGap struct {
	Description       string              `json:"description"`
	MissingEvidence   string              `json:"missing_evidence"`
	Kind              string              `json:"kind"`
	RequestedEvidence []requestedEvidence `json:"requested_evidence"`
	Limitations       []sourceLimitation  `json:"limitations"`
}

type supplementRequest struct {
	SourceTaskID   string            `json:"source_task_id"`
	SourceHash     string            `json:"source_payload_sha256"`
	GapIndex       int               `json:"gap_index"`
	ReviewRevision string            `json:"review_revision"`
	Bindings       map[string]string `json:"bindings"`
	Gap            sourceGap         `json:"gap"`
}

type supplementRequests struct {
	Round      int                 `json:"round"`
	SnapshotID string              `json:"run_input_snapshot_id"`
	Requests   []supplementRequest `json:"requests"`
}

func decodeSourceGap(raw json.RawMessage) (sourceGap, error) {
	var gap sourceGap
	if err := json.Unmarshal(raw, &gap); err != nil {
		return gap, err
	}
	switch gap.Kind {
	case "unread", "truncated", "missing_dependency", "semantic_uncertainty":
	default:
		return gap, fmt.Errorf("invalid source gap kind %q", gap.Kind)
	}
	if gap.RequestedEvidence == nil || gap.Limitations == nil || len(gap.RequestedEvidence) > 16 || len(gap.Limitations) > 16 {
		return gap, fmt.Errorf("source gap requires bounded structured evidence and limitations")
	}
	if gap.Kind != "semantic_uncertainty" && len(gap.RequestedEvidence) == 0 && len(gap.Limitations) == 0 {
		return gap, fmt.Errorf("source gap lacks a structured request or limitation")
	}
	for _, request := range gap.RequestedEvidence {
		if request.Path == "" || len(request.Path) > 1024 || strings.ContainsAny(request.Path, "\\\x00\n\r") || strings.HasPrefix(request.Path, "/") || path.Clean(request.Path) != request.Path || request.Path == "." || strings.HasPrefix(request.Path, "../") || request.Ranges == nil || len(request.Ranges) > 32 {
			return gap, fmt.Errorf("invalid requested evidence path or ranges")
		}
		var end int64
		for _, span := range request.Ranges {
			if span.Start < end || span.End <= span.Start {
				return gap, fmt.Errorf("requested evidence ranges must be ordered nonempty half-open byte spans")
			}
			end = span.End
		}
	}
	for _, limitation := range gap.Limitations {
		switch limitation.Kind {
		case "unavailable_path", "out_of_scope", "version_mismatch", "budget_exhausted", "invalid_request":
		default:
			return gap, fmt.Errorf("invalid source limitation kind")
		}
		if strings.TrimSpace(limitation.Detail) == "" {
			return gap, fmt.Errorf("source limitation requires detail")
		}
	}
	return gap, nil
}

// Only the receipt-verified accepted records select requests. No model copies
// task identities, gap indices, hashes, snapshot grants, or frozen-source paths.
func collectSupplementRequests(current session, runID string) (supplementRequests, error) {
	result := supplementRequests{Round: 1, SnapshotID: current.SnapshotID, Requests: []supplementRequest{}}
	inventory, err := collectReviewHandoffs(current, runID)
	if err != nil {
		return result, err
	}
	for _, source := range inventory.Sources {
		for _, item := range current.Tasks {
			if item.ID != source.TaskID {
				continue
			}
			if item.ContractID == "review-supplement-workset" {
				continue
			}
			value, err := accepted(item, current, runID)
			if err != nil {
				return result, err
			}
			var r record
			if err := json.Unmarshal(value.Record, &r); err != nil {
				return result, err
			}
			for index, raw := range r.Gaps {
				gap, err := decodeSourceGap(raw)
				if err != nil {
					return result, fmt.Errorf("task %s gap %d: %w", item.ID, index, err)
				}
				if gap.Kind == "semantic_uncertainty" {
					continue
				}
				result.Requests = append(result.Requests, supplementRequest{SourceTaskID: item.ID, SourceHash: source.PayloadHash, GapIndex: index, ReviewRevision: r.Revision, Bindings: item.Binding.Bindings, Gap: gap})
			}
		}
	}
	return result, nil
}
