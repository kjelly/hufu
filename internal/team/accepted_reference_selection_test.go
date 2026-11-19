package team

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAcceptedReferenceSelections(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values []string
		valid  bool
	}{
		{"key", []string{"/findings/0=ev_observed"}, true},
		{"full_id", []string{"/findings/0=sha256-" + strings.Repeat("a", 64)}, true},
		{"missing_value", []string{"/findings/0="}, false},
		{"missing_pointer", []string{"=ev_observed"}, false},
		{"invalid_escape", []string{"/bad~2=ev_observed"}, false},
		{"duplicate", []string{"/a=one", "/a=two"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAcceptedReferenceSelections(tc.values)
			if (err == nil) != tc.valid {
				t.Fatalf("selection error: %v", err)
			}
		})
	}
}

func TestAcceptedReferenceSelectionRejectsForgedGrant(t *testing.T) {
	for _, scenario := range []string{"missing_projection", "foreign_pointer", "duplicate_pointer", "foreign_reference", "forged_response", "stale_head", "cancelled", "missing_original_claims"} {
		t.Run(scenario, func(t *testing.T) {
			c, source, id := contractCollectionFixture(t, researchCollectionResponse(t, 2), SideEffectNone, contractCollectionFixtureOptions{Accepted: true, ArtifactKeys: true})
			ctx := contractCollectionContext(t, id)
			plan, err := c.AssessContractRecovery(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			branch, err := c.ApplyContractRecovery(ctx, source, "controlled-reference", plan.SHA256)
			if err != nil {
				t.Fatal(err)
			}
			child, err := ReadResumeSource(c.session.Workspace, branch, "")
			if err != nil {
				t.Fatal(err)
			}
			c = reopenContractCollection(t, c, child)
			ctx = acceptedReferenceTestContext(t, c, t.Context(), id)
			choices := selectedAcceptedReferences(ctx)
			switch scenario {
			case "foreign_pointer":
				choices[0].Pointer = "/findings/1"
			case "duplicate_pointer":
				choices = append(choices, choices[0])
			case "foreign_reference":
				choices[0].Reference = "ev_unobserved"
			}
			ctx = WithAcceptedReferenceSelections(ctx, choices)
			a, err := c.AssessResultCollectionRecovery(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "foreign_pointer" || scenario == "duplicate_pointer" || scenario == "foreign_reference" {
				if a.Disposition != RecoveryBlocked {
					t.Fatal("invalid selection granted")
				}
				return
			}
			grant := c.collectionResultRepairGrant(a)
			if scenario == "missing_original_claims" {
				ref := a.Checkpoint.ContractUpgrade.Original.Prepared
				if err := os.Remove(filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", ref.ArtifactID)); err != nil {
					t.Fatal(err)
				}
				if _, err := c.prepareAcceptedReferenceProjection(ctx, a); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("original claims read error lost: %v", err)
				}
				return
			}
			if scenario != "missing_projection" {
				grant.ReferenceProjection, err = c.prepareAcceptedReferenceProjection(ctx, a)
				if err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "forged_response" {
				grant.ReferenceProjection.Response = grant.ReferenceProjection.Request
			}
			if scenario == "stale_head" {
				appendBranchEvent(t, c.eventStore, RunEvent{Type: string(EventRunStarted), RunID: c.executionRunID, Payload: mustProjectionJSON(t, map[string]any{"prompt": "later"})})
			}
			if scenario == "cancelled" {
				var cancel func()
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			before := currentResultCollection(c.todoItemByID(id))
			if err := c.CommitTaskTransition(ctx, id, TaskBlocked, TaskProtocolIncomplete, "forged selection", "", map[string]any{"result_repair_grant": grant}); err == nil {
				t.Fatal("invalid grant admitted")
			}
			after := currentResultCollection(c.todoItemByID(id))
			if before.Revision != after.Revision || before.TurnsUsed != after.TurnsUsed || c.todoItemByID(id).Status != TaskBlocked {
				t.Fatal("rejected selection changed live task")
			}
		})
	}
}
