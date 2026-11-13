package team

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
)

// ExecutionStepReceiptRegistry keeps authoritative receipts independent of
// worker summaries. It is safe to share between concurrently running tasks.
type ExecutionStepReceiptRegistry struct {
	mu       sync.RWMutex
	receipts map[stepReceiptScopeKey]ExecutionStepReceipt
}

// NewExecutionStepReceiptRegistry creates an empty authoritative receipt store.
func NewExecutionStepReceiptRegistry() *ExecutionStepReceiptRegistry {
	return &ExecutionStepReceiptRegistry{receipts: make(map[stepReceiptScopeKey]ExecutionStepReceipt)}
}

// Record stores one runtime-created receipt. Conflicting evidence within one
// occurrence is rejected; reused provider call IDs in other occurrences do not
// replace history.
func (r *ExecutionStepReceiptRegistry) Record(receipt ExecutionStepReceipt) error {
	if r == nil {
		return fmt.Errorf("execution receipt registry is nil")
	}
	if strings.TrimSpace(receipt.ID) == "" {
		return fmt.Errorf("execution receipt id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	key := stepReceiptRegistryKey(receipt)
	if old, ok := r.receipts[key]; ok {
		if !reflect.DeepEqual(old, receipt) {
			return fmt.Errorf("execution receipt %q conflicts with existing evidence", receipt.ID)
		}
		return nil
	}
	r.receipts[key] = cloneExecutionStepReceipt(receipt)
	return nil
}

// Get returns a detached receipt by ID. An unscoped ID reused by several
// occurrences is ambiguous; callers must first select an occurrence view.
func (r *ExecutionStepReceiptRegistry) Get(id string) (ExecutionStepReceipt, bool) {
	if r == nil {
		return ExecutionStepReceipt{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var found ExecutionStepReceipt
	exists := false
	for _, receipt := range r.receipts {
		if receipt.ID != id {
			continue
		}
		if exists {
			return ExecutionStepReceipt{}, false
		}
		found, exists = receipt, true
	}
	return cloneExecutionStepReceipt(found), exists
}

// ValidateClaims ensures every result claim cites an actual receipt belonging
// to exactly the same task and attempt. A claim for an unexecuted or other
// attempt's step therefore cannot become authoritative task evidence.
func (r *ExecutionStepReceiptRegistry) ValidateClaims(taskID string, attempt int, ids []string) error {
	if r == nil {
		return fmt.Errorf("execution receipt registry is nil")
	}
	if len(ids) == 0 {
		return fmt.Errorf("at least one execution receipt claim is required")
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return fmt.Errorf("execution receipt claim is empty")
		}
		if seen[id] {
			return fmt.Errorf("execution receipt claim %q is duplicated", id)
		}
		seen[id] = true
		receipt, ok := r.Get(id)
		if !ok {
			return fmt.Errorf("execution receipt claim %q does not exist", id)
		}
		if receipt.TaskID != taskID || receipt.Attempt != attempt {
			return fmt.Errorf("execution receipt claim %q belongs to task %q attempt %d, not task %q attempt %d", id, receipt.TaskID, receipt.Attempt, taskID, attempt)
		}
	}
	return nil
}

// ValidateCompleteClaims requires an exact claim set for one successful
// structured attempt. This prevents a worker from citing only the final happy
// receipt while omitting an earlier failed validator or repair invocation.
func (r *ExecutionStepReceiptRegistry) ValidateCompleteClaims(taskID string, attempt int, ids []string) error {
	if err := r.ValidateClaims(taskID, attempt, ids); err != nil {
		return err
	}
	expected := r.ReceiptIDs(taskID, attempt)
	claimed := make(map[string]bool, len(ids))
	for _, id := range ids {
		claimed[strings.TrimSpace(id)] = true
	}
	var missing []string
	for _, id := range expected {
		if !claimed[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("successful structured result omits execution receipt(s): %s", strings.Join(missing, ", "))
	}
	return nil
}

// ReceiptIDs returns stable receipt IDs for exactly one task attempt.
func (r *ExecutionStepReceiptRegistry) ReceiptIDs(taskID string, attempt int) []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	receipts := make([]ExecutionStepReceipt, 0)
	for _, receipt := range r.receipts {
		if receipt.TaskID == taskID && receipt.Attempt == attempt {
			receipts = append(receipts, receipt)
		}
	}
	r.mu.RUnlock()
	sort.Slice(receipts, func(i, j int) bool {
		if receipts[i].StartedAt.Equal(receipts[j].StartedAt) {
			return receipts[i].ID < receipts[j].ID
		}
		return receipts[i].StartedAt.Before(receipts[j].StartedAt)
	})
	ids := make([]string, len(receipts))
	for i, receipt := range receipts {
		ids[i] = receipt.ID
	}
	return ids
}

// FirstFailure returns the first causal non-zero receipt for one task attempt.
// Later reporting calls (including a rejected submit_result) cannot replace it.
func (r *ExecutionStepReceiptRegistry) FirstFailure(taskID string, attempt int) (ExecutionStepReceipt, bool) {
	if r == nil {
		return ExecutionStepReceipt{}, false
	}
	r.mu.RLock()
	var receipts []ExecutionStepReceipt
	for _, receipt := range r.receipts {
		if receipt.TaskID != taskID || (attempt > 0 && receipt.Attempt != attempt) {
			continue
		}
		receipts = append(receipts, receipt)
	}
	r.mu.RUnlock()
	sortExecutionReceipts(receipts)
	for i, receipt := range receipts {
		if receipt.ExitCode == 0 {
			continue
		}
		if receipt.ValidatorVerdict == "fail" {
			recovered := false
			for _, later := range receipts[i+1:] {
				if later.StepID == receipt.StepID && later.ExitCode == 0 && later.ValidatorVerdict == "pass" {
					recovered = true
					break
				}
			}
			if recovered {
				continue
			}
		}
		return cloneExecutionStepReceipt(receipt), true
	}
	return ExecutionStepReceipt{}, false
}

func cloneExecutionStepReceipt(receipt ExecutionStepReceipt) ExecutionStepReceipt {
	copyReceipt := receipt
	copyReceipt.ModelExecutionIDs = slices.Clone(receipt.ModelExecutionIDs)
	copyReceipt.Consumed = append([]ArtifactRef(nil), receipt.Consumed...)
	copyReceipt.Produced = append([]ArtifactRef(nil), receipt.Produced...)
	copyReceipt.ProducedFacts = append([]StructuredFactRef(nil), receipt.ProducedFacts...)
	copyReceipt.ResolvedRefs = append([]ResolvedStepRef(nil), receipt.ResolvedRefs...)
	copyReceipt.ConsumedDigests = cloneStringMap(receipt.ConsumedDigests)
	copyReceipt.ProducedDigests = cloneStringMap(receipt.ProducedDigests)
	return copyReceipt
}

func (e *structuredExecutionRun) recordReceipt(receipt ExecutionStepReceipt) (ExecutionStepReceipt, error) {
	if e.request.publishReceipt != nil {
		canonical, err := e.request.publishReceipt(e.ctx, receipt)
		if err != nil {
			e.result.Receipts = append(e.result.Receipts, receipt)
			return receipt, fmt.Errorf("persist structured step receipt: %w", err)
		}
		receipt = canonical
	}
	e.result.Receipts = append(e.result.Receipts, receipt)
	if e.request.Registry != nil {
		if err := e.request.Registry.Record(receipt); err != nil {
			return receipt, err
		}
	}
	return receipt, nil
}

type stepReceiptScopeKey struct {
	runID, branchID, taskID, receiptID string
	attempt, revision                  int
}

func stepReceiptSourceKey(receipt ExecutionStepReceipt) stepReceiptScopeKey {
	return stepReceiptScopeKey{runID: receipt.RunID, branchID: receipt.BranchID, taskID: receipt.TaskID, receiptID: receipt.ID, attempt: receipt.Attempt, revision: receipt.OccurrenceRevision}
}

func stepReceiptRegistryKey(receipt ExecutionStepReceipt) stepReceiptScopeKey {
	if receipt.RunID == "" && receipt.BranchID == "" && receipt.OccurrenceRevision == 0 {
		return stepReceiptScopeKey{receiptID: receipt.ID}
	}
	return stepReceiptSourceKey(receipt)
}
