package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
)

// ResultCollectionSourceOrigin pins the accepted completion that preceded a
// schema-only retention. It never grants execution or substitutes a receipt.
type ResultCollectionSourceOrigin struct {
	EventID      string `json:"event_id"`
	EventHash    string `json:"event_hash"`
	TaskSHA256   string `json:"task_sha256"`
	ContractHash string `json:"contract_hash"`
}

type collectionOriginalEvent struct {
	Origin     ResultCollectionSourceOrigin
	TaskID     string
	BranchID   string
	Checkpoint string
	Contract   ResultContractRef
}

func supportsCollectionOriginRevision(cp *ResultCollectionCheckpoint, revision int) bool {
	return cp.ContractUpgrade == nil || cp.ContractUpgrade.SourceOrigin == nil || revision >= 14
}

func collectionOriginalCompletion(event RunEvent, canonical *TodoItem) (*TodoItem, *collectionOriginalEvent, error) {
	if event.Type != string(EventTaskCompleted) || event.ID == "" || !validCollectionDigest(event.Hash) {
		return nil, nil, errResultCollectionStale
	}
	item := cloneTodoItem(canonical)
	if item == nil || item.ID != event.TaskID || validateAcceptedResultRepairTarget(item) != nil || currentResultCollection(item).BranchID != effectiveEventBranchID(event) {
		return nil, nil, errResultCollectionStale
	}
	digest, err := collectionDigest(canonicalCollectionSources([]*TodoItem{item}))
	if err != nil {
		return nil, nil, err
	}
	checkpoint, err := collectionDigest(currentResultCollection(item))
	if err != nil {
		return nil, nil, err
	}
	return item, &collectionOriginalEvent{Origin: ResultCollectionSourceOrigin{event.ID, event.Hash, digest, item.ContractHash},
		TaskID: item.ID, BranchID: effectiveEventBranchID(event), Checkpoint: checkpoint, Contract: *item.ResultContract}, nil
}

// Reconstruct only metadata that a verified retention may have rebound. All
// work, receipts, inputs, payload bytes and semantic claims must remain exact.
func originalCollectionRepairItem(item *TodoItem, upgrade *ResultCollectionContractUpgrade) (*TodoItem, error) {
	if upgrade == nil || upgrade.SourceOrigin == nil {
		return item, nil
	}
	p := upgrade.SourceOrigin
	if upgrade.AcceptedResult == nil || item == nil || item.TypedResult == nil || item.TypedResult.StructuredPayload == nil ||
		p.EventID == "" || !validCollectionDigest(p.EventHash) || !validCollectionDigest(p.TaskSHA256) || !validCollectionDigest(p.ContractHash) ||
		!recoveryOriginalContractMatches(item, upgrade.OriginalContract) {
		return nil, errResultCollectionStale
	}
	original := cloneTodoItem(item)
	original.ResultContract = upgrade.OriginalContract.clone()
	original.ContractHash, original.ContractRevalidation = p.ContractHash, nil
	original.TypedResult.StructuredPayload.Contract = upgrade.OriginalContract
	digest, err := collectionDigest(canonicalCollectionSources([]*TodoItem{original}))
	if err != nil || digest != p.TaskSHA256 {
		return nil, fmt.Errorf("retained collection changed beyond schema metadata: %w", errResultCollectionStale)
	}
	return original, nil
}

func validateCollectionOriginalRecord(upgrade *ResultCollectionContractUpgrade, record *collectionOriginalEvent) error {
	if upgrade == nil || upgrade.SourceOrigin == nil {
		return nil
	}
	if record == nil || record.Origin != *upgrade.SourceOrigin || upgrade.Original == nil ||
		record.TaskID != upgrade.Original.TaskID || record.BranchID != upgrade.Original.BranchID || record.Contract != upgrade.OriginalContract {
		return fmt.Errorf("accepted collection origin is not in the selected lineage: %w", errResultCollectionStale)
	}
	digest, err := collectionDigest(upgrade.Original)
	if err != nil || digest != record.Checkpoint {
		return fmt.Errorf("accepted collection origin checkpoint changed: %w", errResultCollectionStale)
	}
	return nil
}

func validateCollectionOriginalEvents(events []RunEvent, plan *ContractRecoveryPlan) error {
	for _, patch := range plan.Tasks {
		p := patch.Collection
		if p == nil || p.SourceOrigin == nil {
			continue
		}
		found := false
		for i, event := range events {
			if event.ID != p.SourceOrigin.EventID {
				continue
			}
			tasks, err := ReplayTodoList(events[:i+1])
			if err != nil {
				return err
			}
			_, record, err := collectionOriginalCompletion(event, todoItemByID(tasks, patch.ID))
			if err != nil {
				return err
			}
			if err := validateCollectionOriginalRecord(p, record); err != nil {
				return err
			}
			policy, err := executionPolicySnapshotFromEvents(events[:i+1])
			if err != nil || policy == nil || policy.ConfigurationHash != p.Original.PolicySHA256 {
				return fmt.Errorf("accepted collection origin policy is unavailable: %w", errResultCollectionStale)
			}
			found = true
			break
		}
		if !found {
			return fmt.Errorf("accepted collection origin completion is unavailable: %w", errResultCollectionStale)
		}
	}
	return nil
}

// Resolve solely within the already verified selected lineage, including its
// fork cutoff. An independently changed ancestor branch is not consulted.
func retainedCollectionOrigin(source *ResumeSource, item *TodoItem) (*ResumeSource, *TodoItem, *ResultCollectionSourceOrigin, error) {
	cp := currentResultCollection(item)
	if cp == nil {
		return nil, nil, nil, errResultCollectionStale
	}
	if cp.BranchID == source.BranchID && cp.PolicySHA256 == source.Session.ExecutionPolicySnapshot.ConfigurationHash {
		return source, item, nil, nil
	}
	if validateAcceptedResultRepairTarget(item) != nil {
		return nil, nil, nil, fmt.Errorf("only an accepted retained collection can select an ancestor origin")
	}
	for i := len(source.lineage) - 1; i >= 0; i-- {
		event := source.lineage[i]
		if event.Type != string(EventTaskCompleted) || event.TaskID != item.ID || effectiveEventBranchID(event) != cp.BranchID {
			continue
		}
		var payload struct {
			Receipt *ExecutionReceipt `json:"execution_receipt"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.Receipt == nil || !sameCanonicalEvidence(payload.Receipt.ResultCollection, cp) {
			continue
		}
		prefix := source.lineage[:i+1]
		policy, err := executionPolicySnapshotFromEvents(prefix)
		if err != nil || policy == nil || policy.ConfigurationHash != cp.PolicySHA256 {
			return nil, nil, nil, fmt.Errorf("original collection policy cannot be authenticated: %w", errResultCollectionStale)
		}
		session := ReduceToSessionData(prefix)
		session.ExecutionPolicySnapshot = policy
		session.Tasks, err = ReplayTodoList(prefix)
		if err != nil {
			return nil, nil, nil, err
		}
		original, record, err := collectionOriginalCompletion(event, todoItemByID(session.Tasks, item.ID))
		if err != nil {
			return nil, nil, nil, err
		}
		if original == nil || !sameCanonicalEvidence(currentResultCollection(original), cp) {
			return nil, nil, nil, errResultCollectionStale
		}
		upgrade := &ResultCollectionContractUpgrade{SourceOrigin: &record.Origin, Original: cp, OriginalContract: record.Contract, AcceptedResult: &AcceptedResultRepair{}}
		if _, err := originalCollectionRepairItem(item, upgrade); err != nil {
			return nil, nil, nil, err
		}
		return &ResumeSource{BranchID: cp.BranchID, Session: session, workspace: source.workspace, lineage: prefix,
			eventHead: source.eventHead, eventHash: source.eventHash}, original, &record.Origin, nil
	}
	return nil, nil, nil, fmt.Errorf("original accepted completion is outside the selected lineage: %w", errResultCollectionStale)
}

type collectionOriginValidation struct {
	source *ResumeSource
	store  *EventStore
	cp     *ResultCollectionCheckpoint
}

func (c *Coordinator) collectionOriginLineage(ctx context.Context) ([]RunEvent, bool, error) {
	if c == nil {
		return nil, false, nil
	}
	scope := c.collectionOriginValidation
	if scope == nil {
		return nil, false, nil
	}
	if err := context.Cause(ctx); err != nil {
		return nil, true, err
	}
	if c.eventStore != scope.store || c.activeBranchID() != scope.source.BranchID || scope.store.BranchID() != scope.source.BranchID {
		return nil, true, errResultCollectionStale
	}
	return cloneRunEvents(scope.source.lineage), true, nil
}

func (c *Coordinator) validateAcceptedCollectionOrigin(ctx context.Context, cp *ResultCollectionCheckpoint) error {
	if cp.ContractUpgrade == nil || cp.ContractUpgrade.SourceOrigin == nil {
		return nil
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return err
	}
	return validateCollectionOriginalEvents(events, &ContractRecoveryPlan{Tasks: []ContractRecoveryTask{{ID: cp.TaskID, Collection: cp.ContractUpgrade}}})
}
