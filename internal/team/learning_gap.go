package team

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

// isAutoRepairableGap reports whether repairMemoryLearningGaps can resolve
// gap without an operator: it needs the durable idempotency key and either a
// replayable event or an already-appended aggregate event to reduce. Every
// other unresolved gap is an incident that stays visible but is never retried.
func isAutoRepairableGap(gap LearningGap) bool {
	if isManualCreditReviewGap(gap) || gap.IdempotencyKey == "" {
		return false
	}
	return gap.RepairEvent != nil || gap.EventType == "memory_aggregate_repair"
}

func (c *Coordinator) recordLearningGap(event RunEvent, appendErr error) {
	if c == nil || appendErr == nil {
		return
	}
	var repairEvent *RunEvent
	if strings.HasPrefix(event.Type, "memory_") && len(event.Payload) > 0 {
		copyEvent := event
		if redacted, err := utils.RedactJSON(event.Payload); err == nil {
			copyEvent.Payload = redacted
		}
		// The store stamps an unbranched event with whatever branch it is
		// bound to at append time. Pin the branch the event failed on so a
		// repair after a checkout cannot write it into another lineage.
		if copyEvent.BranchID == "" {
			copyEvent.BranchID = c.learningGapBranchID()
		}
		repairEvent = &copyEvent
	}
	c.persistLearningGap(LearningGap{
		EventType: event.Type, TaskID: event.TaskID, IdempotencyKey: event.IdempotencyKey,
		Reason: appendErr.Error(), PendingRepair: true, RepairEvent: repairEvent,
	})
}

// learningGapBranchID names the branch an unbranched append was bound to. An
// unbound store appends to the implicit main lineage, so main is explicit here.
func (c *Coordinator) learningGapBranchID() string {
	if c.eventStore != nil {
		if branchID := c.eventStore.BranchID(); branchID != "" {
			return branchID
		}
	}
	return "main"
}

// A failed credit lookup cannot produce a replayable event: its effective
// weight depends on the validated ledger that was unavailable. Keep the
// incident durable and visible, but do not send it to automatic event repair.
func (c *Coordinator) recordMemoryCreditReviewGap(taskID, signal, direction string, readErr error) {
	if c == nil || readErr == nil {
		return
	}
	c.persistLearningGap(LearningGap{
		EventType: "memory_outcome_recorded", TaskID: taskID,
		Reason:               fmt.Sprintf("outcome credit lookup failed for signal %q direction %q: %v; no outcome event was appended; manual review required", signal, direction, readErr),
		ManualReviewRequired: true,
	})
}

func (c *Coordinator) persistLearningGap(gap LearningGap) {
	c.dualWriteFailures.Add(1)
	gap.Reason = utils.RedactSecrets(gap.Reason)
	gap.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := c.mutateSessionData(func(sd *SessionData) error {
		sd.LearningGaps = append(sd.LearningGaps, gap)
		return nil
	}); err != nil {
		return
	}
	// Persist before returning: a crash before the next checkpoint would lose
	// either the replayable event or the manual-review incident (spec §7
	// HF-MEM4-000 item 4, §9).
	if err := c.persistSession("persist learning gap"); err != nil {
		log.Printf("warning: persist learning gap checkpoint failed: %v", err)
	}
}

func (c *Coordinator) repairMemoryLearningGaps(branchOpts ...any) {
	if c == nil || c.eventStore == nil || c.sessionData == nil {
		return
	}
	var st *SessionTree
	activeBranch := "main"
	for _, opt := range branchOpts {
		switch v := opt.(type) {
		case *SessionTree:
			st = v
		case string:
			if v != "" {
				activeBranch = v
			}
		}
	}
	if st != nil && st.ActiveBranch != "" && len(branchOpts) == 1 {
		activeBranch = st.ActiveBranch
	}
	var gaps []LearningGap
	c.viewSessionData(func(sd *SessionData) { gaps = append(gaps, sd.LearningGaps...) })
	originalGapCount := len(gaps)
	changed := false
	for i := range gaps {
		gap := &gaps[i]
		if isManualCreditReviewGap(*gap) {
			if gap.PendingRepair || !gap.ManualReviewRequired {
				gap.PendingRepair = false
				gap.ManualReviewRequired = true
				changed = true
			}
			continue
		}
		if !gap.PendingRepair || !isAutoRepairableGap(*gap) {
			continue
		}
		if gap.RepairEvent != nil {
			if _, err := c.emitEventOnce(gap.IdempotencyKey, *gap.RepairEvent); err != nil {
				continue
			}
		}
		if c.reduceRepairedGap(st, activeBranch, *gap) {
			gap.PendingRepair = false
			changed = true
		}
	}
	if !changed {
		return
	}
	_ = c.mutateSessionData(func(sd *SessionData) error {
		if len(sd.LearningGaps) > originalGapCount {
			gaps = append(gaps, sd.LearningGaps[originalGapCount:]...)
		}
		sd.LearningGaps = gaps
		return nil
	})
	_ = c.persistSession("persist repaired learning gaps")
}

// reduceRepairedGap reports whether gap's event is durable. An event visible
// on the active branch is reduced now. An event pinned to another branch is
// reduced when that branch's lineage is next hydrated at startup, so it
// resolves the gap without touching the active branch's projection.
func (c *Coordinator) reduceRepairedGap(st *SessionTree, activeBranch string, gap LearningGap) bool {
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		return false
	}
	durable, found, visible := locateRepairedGapEvent(events, FilterEventsForBranch(events, st, activeBranch), gap)
	if !found {
		return false
	}
	if visible {
		c.reduceMemoryEvent(durable)
	}
	return true
}

// locateRepairedGapEvent finds gap's durable event, first in the active
// lineage and then, for an event pinned to a branch, anywhere on that branch.
// A gap recorded before branch pinning matches by key in the active lineage
// only, as it always did.
func locateRepairedGapEvent(events, lineage []RunEvent, gap LearningGap) (durable RunEvent, found, visible bool) {
	pinnedBranch := ""
	if gap.RepairEvent != nil && gap.RepairEvent.BranchID != "" {
		pinnedBranch = effectiveEventBranchID(*gap.RepairEvent)
	}
	matches := func(event RunEvent) bool {
		return event.IdempotencyKey == gap.IdempotencyKey &&
			(pinnedBranch == "" || effectiveEventBranchID(event) == pinnedBranch)
	}
	for _, event := range lineage {
		if matches(event) {
			return event, true, true
		}
	}
	if pinnedBranch == "" {
		return RunEvent{}, false, false
	}
	for _, event := range events {
		if matches(event) {
			return event, true, false
		}
	}
	return RunEvent{}, false, false
}
