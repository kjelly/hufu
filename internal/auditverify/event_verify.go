package auditverify

import (
	"context"
	"fmt"
	"reflect"

	"github.com/kjelly/hufu/internal/team"
)

// verifyScopedEventChain handles a projected branch whose neighboring visible
// events need not be neighbors in the physical journal. A gap is accepted only
// after revalidating the complete journal and reproducing this exact lineage;
// deleting an event from a supplied slice still fails closed.
func verifyScopedEventChain(ctx context.Context, workspace string, events []team.RunEvent) EventChainVerification {
	result := VerifyEventChain(events)
	if result.Valid || len(events) == 0 {
		return result
	}
	var global []team.RunEvent
	if err := team.StreamValidatedRunEvents(ctx, workspace, func(event team.RunEvent) error {
		global = append(global, event)
		return nil
	}); err != nil {
		return result
	}
	tree, err := team.LoadSessionTree(workspace)
	if err != nil {
		return result
	}
	branch := events[len(events)-1].BranchID
	if branch == "" {
		branch = "main"
	}
	projected, err := team.ProjectValidatedEventsForBranch(global, tree, branch)
	if err != nil {
		return result
	}
	// The caller's verified snapshot may precede a later append. Require its
	// complete canonical prefix through the supplied head, not a newer head.
	for i, event := range projected {
		if event.ID == result.HeadID {
			if reflect.DeepEqual(projected[:i+1], events) {
				result.Valid = true
				result.Findings = nil
			}
			return result
		}
	}
	return result
}

// EventChainVerification is the result of independently re-deriving the hash
// chain over a slice of events (spec.md §11). It never re-implements the hash
// algorithm itself: linkage is checked field-by-field and every hash is
// recomputed via team.ComputeEventHash.
//
// The check is scoped to internal linkage of the supplied slice: for a branch
// lineage that forks partway through the log, events[0] need not have an
// empty PreviousID/PreviousHash. Whether events[0] is itself a legitimate
// chain root is established by the caller (team.EventStore already refuses to
// open a file whose true first record fails that check).
type EventChainVerification struct {
	Valid bool

	Events int

	HeadID   string
	HeadHash string

	Findings []string
}

// VerifyEventChain re-verifies hash linkage over events. It is intentionally
// pure and file-I/O free so it can be exercised directly against crafted
// fixtures (mutated payload, changed previous_hash, deleted middle event) in
// unit tests, and reused unmodified for both workspace-mode and bundle-mode
// verification.
func VerifyEventChain(events []team.RunEvent) EventChainVerification {
	result := EventChainVerification{Events: len(events)}
	if len(events) == 0 {
		result.Valid = true
		return result
	}

	valid := true
	for i, event := range events {
		if i > 0 {
			prev := events[i-1]
			if event.PreviousID != prev.ID {
				result.Findings = append(result.Findings, fmt.Sprintf(
					"event %d (%s) previous_id %q does not match preceding event id %q", i, event.ID, event.PreviousID, prev.ID))
				valid = false
			}
			if event.PreviousHash != prev.Hash {
				result.Findings = append(result.Findings, fmt.Sprintf(
					"event %d (%s) previous_hash does not match preceding event hash", i, event.ID))
				valid = false
			}
		}
		expected := team.ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
		if event.Hash != expected {
			result.Findings = append(result.Findings, fmt.Sprintf(
				"event %d (%s) hash does not match its recomputed digest", i, event.ID))
			valid = false
		}
	}

	result.Valid = valid
	last := events[len(events)-1]
	result.HeadID = last.ID
	result.HeadHash = last.Hash
	return result
}
