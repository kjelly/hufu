package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

type handoffSource struct {
	TaskID           string `json:"task_id"`
	ContractID       string `json:"contract_id"`
	PayloadHash      string `json:"payload_sha256"`
	FindingCount     int    `json:"finding_count"`
	RequiresCritique bool   `json:"requires_critique"`
}

type handoffInventory struct {
	Passed                bool            `json:"passed"`
	SourceTaskIDs         []string        `json:"source_task_ids"`
	RequiredCriticSources []string        `json:"required_critic_source_task_ids"`
	Sources               []handoffSource `json:"sources"`
}

// Source identity comes from the admitted contract and validated record, never
// from the agent's name: a critic can itself produce a documentation review.
func collectReviewHandoffs(current session, runID string) (handoffInventory, error) {
	inventory := handoffInventory{Passed: true, SourceTaskIDs: []string{}, RequiredCriticSources: []string{}, Sources: []handoffSource{}}
	seen := make(map[string]bool)
	for _, item := range current.Tasks {
		switch item.ContractID {
		case "review-primary-workset", "review-documentation-workset", "review-documentation-escalation":
		default:
			continue
		}
		if item.ID == "" || seen[item.ID] {
			return handoffInventory{}, fmt.Errorf("duplicate or empty review source ID")
		}
		seen[item.ID] = true
		value, err := accepted(item, current.SnapshotID, runID)
		if err != nil {
			return handoffInventory{}, err
		}
		var r record
		if err := json.Unmarshal(value.Record, &r); err != nil {
			return handoffInventory{}, err
		}
		if err := verifyReview(item, r); err != nil {
			return handoffInventory{}, err
		}
		requiresCritique := false
		for _, f := range r.Findings {
			requiresCritique = requiresCritique || f.RequiresCritique || f.Severity == "warning" || f.Severity == "error"
		}
		inventory.Sources = append(inventory.Sources, handoffSource{
			TaskID: item.ID, ContractID: item.ContractID, PayloadHash: item.Result.Payload.Hash,
			FindingCount: len(r.Findings), RequiresCritique: requiresCritique,
		})
	}
	if len(inventory.Sources) == 0 {
		return handoffInventory{}, fmt.Errorf("accepted review sources missing")
	}
	// The embedded Go action interpreter does not export generic SortFunc.
	sort.Slice(inventory.Sources, func(i, j int) bool {
		a, b := inventory.Sources[i], inventory.Sources[j]
		left, leftErr := strconv.ParseUint(a.TaskID, 10, 64)
		right, rightErr := strconv.ParseUint(b.TaskID, 10, 64)
		if leftErr == nil && rightErr == nil {
			return left < right
		}
		return a.TaskID < b.TaskID
	})
	for _, source := range inventory.Sources {
		inventory.SourceTaskIDs = append(inventory.SourceTaskIDs, source.TaskID)
		if source.RequiresCritique {
			inventory.RequiredCriticSources = append(inventory.RequiredCriticSources, source.TaskID)
		}
	}
	return inventory, nil
}

func verifyHandoffInventory(current session, runID string) error {
	expected, err := collectReviewHandoffs(current, runID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	found := false
	for _, item := range current.Tasks {
		if item.ContractID != "inventory-review-handoffs" {
			continue
		}
		if found || item.Status != "done" || item.Result == nil || item.Result.Status != "success" || item.Receipt == nil || item.Receipt.RunID != runID || item.Receipt.TaskID != item.ID || item.SnapshotID != current.SnapshotID || !equalJSON(encoded, item.Result.RuntimeOutputs["review_handoff_inventory"]) {
			return fmt.Errorf("missing, stale, or ambiguous review handoff inventory")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("review handoff inventory missing")
	}
	return nil
}
