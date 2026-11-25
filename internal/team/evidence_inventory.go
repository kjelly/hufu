package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
)

// EvidenceInventorySpec selects accepted task IDs and their payload hashes
// from a receipt-verified action output. A fan-in names the expansion whose
// accepted children it must also consume.
type EvidenceInventorySpec struct {
	TaskID        string `json:"task_id" yaml:"task-id"`
	Output        string `json:"output" yaml:"output"`
	IDsPointer    string `json:"ids_pointer" yaml:"ids-pointer"`
	HashesPointer string `json:"hashes_pointer" yaml:"hashes-pointer"`
	MaxSources    int    `json:"max_sources" yaml:"max-sources"`
	FromFanOut    string `json:"from_fan_out,omitempty" yaml:"from-fan-out,omitempty"`
	Goal          string `json:"goal,omitempty" yaml:"goal,omitempty"`
}

func (s *EvidenceInventorySpec) isExpansion() bool { return s != nil && s.FromFanOut == "" }

// The child occurrence owns the binding and its runtime ID. The complete
// native receipt remains on the inventory occurrence.
type EvidenceInventoryBinding struct {
	InventoryTaskID string `json:"inventory_task_id"`
	InvocationID    string `json:"action_invocation_id"`
	OutputsHash     string `json:"runtime_outputs_hash"`
	SourceTaskID    string `json:"source_task_id,omitempty"`
	SourceHash      string `json:"source_payload_sha256,omitempty"`
}

func (b *EvidenceInventoryBinding) clone() *EvidenceInventoryBinding {
	if b == nil {
		return nil
	}
	return new(*b)
}

func isExpandedInventoryTask(task TaskDef) bool {
	return task.EvidenceInventory.isExpansion() && task.EvidenceInventoryBinding != nil && task.EvidenceInventoryBinding.SourceTaskID != ""
}

func validateEvidenceInventoryContracts(session *TeamSession, declared map[string]TaskDef) error {
	for _, task := range session.ContractTasks {
		spec := task.EvidenceInventory
		if spec == nil {
			continue
		}
		if !session.Config.Delegation.BindTaskGoalContracts || task.FanOut != nil || task.Action != nil ||
			(spec.isExpansion() && (!task.Optional || !strings.Contains(spec.Goal, "{source_task_id}") ||
				len(task.DependsOnContracts) > 0 || task.OnFailureContract != "")) {
			return fmt.Errorf("contract %q has incompatible evidence-inventory configuration", task.ID)
		}
		source := declared[contractEdgeKey(spec.TaskID)]
		if source.Action == nil || source.ID == task.ID || strings.TrimSpace(spec.Output) == "" ||
			spec.MaxSources < 1 || spec.MaxSources > maxConfiguredEvidenceSources ||
			spec.IDsPointer == "" || spec.HashesPointer == "" ||
			validateJSONPointer(spec.IDsPointer) != nil || validateJSONPointer(spec.HashesPointer) != nil {
			return fmt.Errorf("contract %q requires a native inventory task, output, valid pointers and bounded max-sources", task.ID)
		}
		if !spec.isExpansion() {
			fanOut := declared[contractEdgeKey(spec.FromFanOut)].EvidenceInventory
			if !fanOut.isExpansion() || fanOut.TaskID != spec.TaskID || fanOut.Output != spec.Output || fanOut.HashesPointer != spec.HashesPointer {
				return fmt.Errorf("contract %q must name an expansion of the same evidence inventory", task.ID)
			}
		}
	}
	return nil
}

type acceptedInventory struct {
	receipt EvidenceInventoryBinding
	ids     []string
	hashes  map[string]string
	origins map[string]string
}

func (c *Coordinator) loadAcceptedInventory(ctx context.Context, spec *EvidenceInventorySpec) (acceptedInventory, error) {
	var inventory acceptedInventory
	if spec == nil {
		return inventory, fmt.Errorf("missing frozen evidence inventory declaration")
	}
	ref := FactRef{Name: "evidence inventory", TaskID: spec.TaskID, RuntimeOutput: spec.Output}
	value, source, receipt, err := c.resolveVerifiedRuntimeOutput(ctx, ref, spec.Output)
	if err != nil {
		return inventory, err
	}
	inventory.receipt = EvidenceInventoryBinding{
		InventoryTaskID: source.ID, InvocationID: receipt.ActionInvocationID, OutputsHash: receipt.RuntimeOutputsHash,
	}
	ids, err := resolveJSONPointer(value, spec.IDsPointer)
	if err != nil {
		return inventory, fmt.Errorf("inventory source IDs: %w", err)
	}
	encoded, err := json.Marshal(ids)
	if err != nil || json.Unmarshal(encoded, &inventory.ids) != nil || inventory.ids == nil {
		return inventory, fmt.Errorf("inventory %s must be an array of task IDs", spec.IDsPointer)
	}
	if len(inventory.ids) > spec.MaxSources {
		return inventory, fmt.Errorf("inventory %s has %d sources, exceeding the limit of %d", spec.IDsPointer, len(inventory.ids), spec.MaxSources)
	}
	seen := make(map[string]bool, len(inventory.ids))
	for _, id := range inventory.ids {
		if strings.TrimSpace(id) != id || id == "" || seen[id] {
			return inventory, fmt.Errorf("inventory %s contains a duplicate or invalid source ID %q", spec.IDsPointer, id)
		}
		seen[id] = true
	}
	hashes, err := resolveJSONPointer(value, spec.HashesPointer)
	if err != nil {
		return inventory, fmt.Errorf("inventory source hashes: %w", err)
	}
	var entries []struct {
		TaskID string `json:"task_id"`
		Hash   string `json:"payload_sha256"`
	}
	encoded, err = json.Marshal(hashes)
	if err != nil || json.Unmarshal(encoded, &entries) != nil || entries == nil || len(entries) > maxConfiguredEvidenceSources {
		return inventory, fmt.Errorf("inventory %s must contain a bounded array of source hashes", spec.HashesPointer)
	}
	inventory.hashes = make(map[string]string, len(entries))
	for _, entry := range entries {
		if _, exists := inventory.hashes[entry.TaskID]; exists || entry.TaskID == "" || entry.Hash == "" {
			return inventory, fmt.Errorf("inventory %s contains a duplicate or invalid hash entry for source %q", spec.HashesPointer, entry.TaskID)
		}
		inventory.hashes[entry.TaskID] = entry.Hash
	}
	inventory.origins, err = c.recoveredTaskOutputOrigins(ctx, c.taskTracker.TodoList().Items())
	if err != nil {
		return inventory, err
	}
	for _, id := range inventory.ids {
		item := c.todoItemByID(id)
		if item == nil || item.Status != TaskDone || item.TypedResult == nil || !taskResultStatusIsSuccessful(item.TypedResult.Status) {
			return inventory, fmt.Errorf("inventory source %q is missing or has no accepted result", id)
		}
		payload := item.TypedResult.StructuredPayload
		if payload == nil || inventory.hashes[id] == "" || payload.SHA256 != inventory.hashes[id] {
			return inventory, fmt.Errorf("inventory source %q payload hash mismatch", id)
		}
		if err := c.validateInventoryResultReceipt(item, inventory.origins); err != nil {
			return inventory, fmt.Errorf("inventory source %q: %w", id, err)
		}
		if err := c.validateAcceptedDependencyPayloadWithContext(ctx, item); err != nil {
			return inventory, fmt.Errorf("inventory source %q: %w", id, err)
		}
	}
	return inventory, nil
}

func (c *Coordinator) validateInventoryResultReceipt(item *TodoItem, origins map[string]string) error {
	snapshot, _ := c.terminalRunInputs(coordinatorRuntimeRunID(c))
	run := taskOutputOriginRun(coordinatorRuntimeRunID(c), item.ID, []map[string]string{origins})
	receipt := latestSuccessfulExecutionReceipt(item, run)
	if snapshot == nil || receipt == nil || receipt.TaskID != item.ID || item.TypedResult.TaskID != item.ID ||
		item.RunInputSnapshotID != snapshot.ID || item.RunInputSnapshotHash != snapshot.SnapshotHash ||
		receipt.RunInputSnapshotID != snapshot.ID || receipt.RunInputSnapshotHash != snapshot.SnapshotHash {
		return fmt.Errorf("no accepted receipt from this run and frozen inputs")
	}
	payload := item.TypedResult.StructuredPayload
	if payload == nil || !receiptMatchesResultPayload(receipt, payload) || receipt.ResultValidation != ResultValidationValid {
		return fmt.Errorf("accepted receipt payload hash or validation mismatch")
	}
	return nil
}

func (i acceptedInventory) childBinding(id string) EvidenceInventoryBinding {
	binding := i.receipt
	binding.SourceTaskID, binding.SourceHash = id, i.hashes[id]
	return binding
}

func (c *Coordinator) inventoryChildren(contractID string, inventory acceptedInventory) (map[string]*TodoItem, error) {
	children := make(map[string]*TodoItem, len(inventory.ids))
	for _, item := range c.taskTracker.TodoList().Items() {
		if item.ContractID != contractID {
			continue
		}
		binding := item.EvidenceInventoryBinding
		if binding == nil || !slices.Contains(inventory.ids, binding.SourceTaskID) ||
			*binding != inventory.childBinding(binding.SourceTaskID) ||
			!slices.Equal(item.EvidenceFrom, []string{binding.SourceTaskID}) {
			return nil, fmt.Errorf("inventory expansion %q has a stale or unrelated child %q", contractID, item.ID)
		}
		if children[binding.SourceTaskID] != nil {
			return nil, fmt.Errorf("inventory expansion %q has duplicate children for source %q", contractID, binding.SourceTaskID)
		}
		children[binding.SourceTaskID] = item
	}
	return children, nil
}

func (c *Coordinator) expandEvidenceInventory(ctx context.Context, task TaskDef) ([]TaskDef, error) {
	inventory, err := c.loadAcceptedInventory(ctx, task.EvidenceInventory)
	if err != nil {
		return nil, err
	}
	children, err := c.inventoryChildren(task.ContractID, inventory)
	if err != nil {
		return nil, err
	}
	var rows []TaskDef
	for _, id := range inventory.ids {
		if child := children[id]; child != nil {
			if !isTerminalTaskStatus(child.Status) {
				return nil, fmt.Errorf("inventory child %q for source %q is %s; finish or resume it before continuing", child.ID, id, child.Status)
			}
			// Ordering depends on settlement. The fan-in still requires
			// successful accepted evidence from every child.
			if child.Status != TaskDone {
				continue
			}
			if child.TypedResult == nil || !taskResultStatusIsSuccessful(child.TypedResult.Status) {
				return nil, fmt.Errorf("inventory child %q for source %q has no accepted result", child.ID, id)
			}
			if err := c.validateInventoryResultReceipt(child, inventory.origins); err != nil {
				return nil, fmt.Errorf("inventory child %q: %w", child.ID, err)
			}
			if err := c.validateAcceptedDependencyPayloadWithContext(ctx, child); err != nil {
				return nil, fmt.Errorf("inventory child %q: %w", child.ID, err)
			}
			continue
		}
		row := task
		row.Goal = strings.ReplaceAll(task.EvidenceInventory.Goal, "{source_task_id}", id)
		row.EvidenceFrom = []string{id}
		row.EvidenceInventoryBinding = new(inventory.childBinding(id))
		if len(rows) > 0 {
			row.OrderAfter = []int{len(rows) - 1}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (c *Coordinator) bindInventoryEvidence(ctx context.Context, tasks []TaskDef) ([]TaskDef, error) {
	for i := range tasks {
		task := &tasks[i]
		spec := task.EvidenceInventory
		if spec == nil || spec.isExpansion() {
			continue
		}
		if len(task.EvidenceFrom) > 0 {
			return nil, fmt.Errorf("contract %q owns evidence_from; omit it", task.ContractID)
		}
		ids, binding, err := c.inventoryFanIn(ctx, spec)
		if err != nil {
			return nil, err
		}
		task.EvidenceFrom, task.EvidenceInventoryBinding = ids, binding
	}
	return tasks, nil
}

func (c *Coordinator) inventoryFanIn(ctx context.Context, spec *EvidenceInventorySpec) ([]string, *EvidenceInventoryBinding, error) {
	inventory, err := c.loadAcceptedInventory(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	childContract := c.inventoryContract(spec.FromFanOut)
	required, err := c.loadAcceptedInventory(ctx, childContract.EvidenceInventory)
	if err != nil {
		return nil, nil, err
	}
	children, err := c.inventoryChildren(childContract.ID, required)
	if err != nil {
		return nil, nil, err
	}
	ids := slices.Clone(inventory.ids)
	for _, id := range required.ids {
		if !slices.Contains(inventory.ids, id) {
			return nil, nil, fmt.Errorf("required inventory source %q is absent from the fan-in list", id)
		}
		child := children[id]
		if child == nil || child.Status != TaskDone || child.TypedResult == nil || !taskResultStatusIsSuccessful(child.TypedResult.Status) {
			return nil, nil, fmt.Errorf("required inventory source %q has no accepted %s child", id, childContract.ID)
		}
		if err := c.validateInventoryResultReceipt(child, inventory.origins); err != nil {
			return nil, nil, fmt.Errorf("inventory child %q: %w", child.ID, err)
		}
		if err := c.validateAcceptedDependencyPayloadWithContext(ctx, child); err != nil {
			return nil, nil, fmt.Errorf("inventory child %q: %w", child.ID, err)
		}
		ids = append(ids, child.ID)
	}
	return ids, new(inventory.receipt), nil
}

func (c *Coordinator) inventoryContract(id string) TaskDef {
	for _, contract := range c.session.ContractTasks {
		if contractEdgeKey(contract.ID) == contractEdgeKey(id) {
			return contract
		}
	}
	return TaskDef{}
}

func (c *Coordinator) validateInventoryOccurrences(ctx context.Context, items []*TodoItem) error {
	validatedExpansions := make(map[string]bool)
	for _, item := range items {
		if item == nil || IsPrimaryOccurrence(item) {
			continue
		}
		if err := c.validateInventoryOccurrence(ctx, item, validatedExpansions); err != nil {
			return err
		}
	}
	return nil
}

func (c *Coordinator) validateInventoryOccurrence(ctx context.Context, item *TodoItem, validatedExpansions map[string]bool) error {
	binding := item.EvidenceInventoryBinding
	contract := c.inventoryContract(item.ContractID)
	spec := contract.EvidenceInventory
	if binding == nil && spec == nil {
		return nil
	}
	if binding == nil || spec == nil {
		return fmt.Errorf("task %q differs from the frozen evidence inventory contract", item.ID)
	}
	if spec.isExpansion() {
		if validatedExpansions[contract.ID] {
			return nil
		}
		inventory, err := c.loadAcceptedInventory(ctx, spec)
		if err != nil {
			return err
		}
		children, err := c.inventoryChildren(contract.ID, inventory)
		if err != nil {
			return err
		}
		child := children[binding.SourceTaskID]
		if child == nil || child.ID != item.ID {
			return fmt.Errorf("task %q has no canonical evidence inventory child binding", item.ID)
		}
		validatedExpansions[contract.ID] = true
		return nil
	}
	ids, expected, err := c.inventoryFanIn(ctx, spec)
	if err != nil {
		return err
	}
	if *binding != *expected || !slices.Equal(ids, item.EvidenceFrom) {
		return fmt.Errorf("task %q evidence no longer matches its frozen inventory binding", item.ID)
	}
	return nil
}
