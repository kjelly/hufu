package main

import (
	"encoding/json"
	"fmt"
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
	// Keep gap fields at request level so explicit ranges fit the generic
	// runtime output depth limit without dropping any accepted source data.
	Description       string              `json:"description"`
	MissingEvidence   string              `json:"missing_evidence"`
	Kind              string              `json:"kind"`
	RequestedEvidence []requestedEvidence `json:"requested_evidence"`
	Limitations       []sourceLimitation  `json:"limitations"`
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
		if request.Path == "" || len(request.Path) > 1024 || request.Ranges == nil || len(request.Ranges) > 32 {
			return gap, fmt.Errorf("requested evidence requires bounded path and range data")
		}
	}
	// An accepted source can contain an unsupported path or range request.
	// Preserve it as data: materialization validates against the frozen tree
	// before any requested file is read, returning an invalid_request limitation
	// rather than discarding the gap or stopping every other review source.
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
func collectSupplementRequests(current sourceSession) (supplementRequests, error) {
	result := supplementRequests{Round: 1, SnapshotID: current.InputID, Requests: []supplementRequest{}}
	inventory, err := collectReviewHandoffs(current)
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
			value, err := accepted(item)
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
				result.Requests = append(result.Requests, supplementRequest{SourceTaskID: item.ID, SourceHash: source.PayloadHash, GapIndex: index, ReviewRevision: r.Revision, Bindings: item.Binding.Bindings,
					Description: gap.Description, MissingEvidence: gap.MissingEvidence, Kind: gap.Kind, RequestedEvidence: gap.RequestedEvidence, Limitations: gap.Limitations})
			}
		}
	}
	return result, nil
}
