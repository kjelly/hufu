package main

import (
	"encoding/json"
	"fmt"
	"strconv"
)

type supplementOutcome struct {
	SourceTaskID string             `json:"source_task_id"`
	GapIndex     int                `json:"gap_index"`
	Kind         string             `json:"kind"`
	WorksetKey   string             `json:"workset_key"`
	Limitations  []sourceLimitation `json:"limitations"`
}

func acceptedSupplementAction(item sourceTask) error {
	if !item.VerifiedOutputs {
		return fmt.Errorf("supplement action lacks verified runtime outputs: %s", item.OutputsError)
	}
	return nil
}

func collectSupplementOutcomes(current sourceSession) ([]supplementOutcome, error) {
	outcomes := []supplementOutcome{}
	requests, err := collectSupplementRequests(current)
	if err != nil {
		return nil, err
	}
	var inventory, producer *sourceTask
	for index := range current.Tasks {
		item := &current.Tasks[index]
		switch item.ContractID {
		case "inventory-review-supplement-requests":
			if inventory != nil {
				return nil, fmt.Errorf("supplement request inventory exceeded its single round")
			}
			inventory = item
		case "prepare-review-supplement":
			if producer != nil {
				return nil, fmt.Errorf("supplement preparation exceeded its single round")
			}
			producer = item
		}
	}
	if len(requests.Requests) == 0 && inventory == nil && producer == nil {
		for _, item := range current.Tasks {
			if item.ContractID == "review-supplement-workset" {
				return nil, fmt.Errorf("supplement review has no accepted producer")
			}
		}
		return outcomes, nil
	}
	if inventory == nil || producer == nil {
		return nil, fmt.Errorf("typed source gaps require the bounded supplement request and prepare actions before report inventory")
	}
	if err := acceptedSupplementAction(*inventory); err != nil {
		return nil, err
	}
	if err := acceptedSupplementAction(*producer); err != nil {
		return nil, err
	}
	expected, err := json.Marshal(requests)
	if err != nil || !equalJSON(expected, inventory.Result.RuntimeOutputs["review_supplement_requests"]) {
		return nil, fmt.Errorf("supplement requests differ from the accepted original gaps")
	}
	var round int
	var prepared bool
	if json.Unmarshal(producer.Result.RuntimeOutputs["supplement_round"], &round) != nil || round != 1 || json.Unmarshal(producer.Result.RuntimeOutputs["supplement_prepared"], &prepared) != nil || !prepared {
		return nil, fmt.Errorf("supplement producer lacks its single-round attestation")
	}
	if err := json.Unmarshal(producer.Result.RuntimeOutputs["supplement_outcomes"], &outcomes); err != nil || outcomes == nil || len(outcomes) != len(requests.Requests) {
		return nil, fmt.Errorf("supplement outcomes omit accepted requests")
	}
	byKey := map[string]frozenGapIdentity{}
	for index, outcome := range outcomes {
		request := requests.Requests[index]
		if outcome.SourceTaskID != request.SourceTaskID || outcome.GapIndex != request.GapIndex || outcome.Kind != request.Kind || outcome.Limitations == nil {
			return nil, fmt.Errorf("supplement outcome changed an original gap identity")
		}
		if outcome.WorksetKey == "" {
			if len(outcome.Limitations) == 0 {
				return nil, fmt.Errorf("unmaterialized supplement lacks typed limitations")
			}
			continue
		}
		if _, duplicate := byKey[outcome.WorksetKey]; duplicate {
			return nil, fmt.Errorf("duplicate supplemental workset identity")
		}
		byKey[outcome.WorksetKey] = frozenGapIdentity{TaskID: request.SourceTaskID, Index: request.GapIndex, Hash: request.SourceHash}
	}
	originals := 0
	for _, item := range current.Tasks {
		switch item.ContractID {
		case "review-primary-workset", "review-documentation-workset", "review-documentation-escalation":
			originals++
		}
	}
	if originals+len(byKey) > 32 {
		return nil, fmt.Errorf("supplement exceeds the 32-review source bound")
	}
	if len(byKey) > 8 {
		return nil, fmt.Errorf("supplement workset exceeds eight-item round limit")
	}
	if err := verifySupplementReviews(current, byKey); err != nil {
		return nil, err
	}
	return outcomes, nil
}

type frozenGapIdentity struct {
	TaskID string
	Index  int
	Hash   string
}

func verifySupplementReviews(current sourceSession, byKey map[string]frozenGapIdentity) error {
	for _, item := range current.Tasks {
		if item.ContractID != "review-supplement-workset" {
			continue
		}
		if item.Binding == nil {
			return fmt.Errorf("supplement review lacks its frozen binding")
		}
		binding := item.Binding.Bindings
		identity, ok := byKey[binding["key"]]
		if !ok || binding["source_task_id"] != identity.TaskID || binding["source_gap_index"] != strconv.Itoa(identity.Index) || binding["source_payload_sha256"] != identity.Hash || binding["supplement_round"] != "1" {
			return fmt.Errorf("supplement review does not match its accepted request")
		}
		if _, err := accepted(item); err != nil {
			return err
		}
		delete(byKey, binding["key"])
	}
	if len(byKey) != 0 {
		return fmt.Errorf("supplement workset has not completed incremental review")
	}
	return nil
}
