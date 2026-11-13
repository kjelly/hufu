package team

import (
	"bytes"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStructuredStepReplayFollowsForkBoundary(t *testing.T) {
	original, events, receipt := stepGenerationEvidence(t, false)
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	workspace := original.session.Workspace
	tree := NewSessionTree()
	for _, branch := range []struct{ name, fork string }{{"retained", events[len(events)-1].ID}, {"sibling", events[0].ID}} {
		tree.Branches[branch.name] = &SessionBranch{ID: branch.name, Name: branch.name, ParentID: "main", ForkEventID: branch.fork, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	}
	before, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		branch   string
		retained bool
	}{{"retained", true}, {"sibling", false}} {
		t.Run(tc.branch, func(t *testing.T) {
			tree.ActiveBranch = tc.branch
			if err := SaveSessionTree(workspace, tree); err != nil {
				t.Fatal(err)
			}
			restarted := newTestCoordinator(t)
			restarted.session = cloneSession(original.session, workspace)
			restarted.executionRunID = "new-" + tc.branch
			restarted.initEventStore()
			if restarted.SessionData().RecoveryRequired {
				t.Fatalf("healthy branch requires recovery: %s", restarted.SessionData().RecoveryReason)
			}
			stored, found := restarted.executionStepReceiptRegistry().Get(receipt.ID)
			if found != tc.retained {
				t.Fatalf("selected branch retained receipt=%t, want %t", found, tc.retained)
			}
			if found {
				after, err := json.Marshal(stored)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatal("inherited receipt bytes changed")
				}
			}
			if err := restarted.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStructuredStepReplayRejectsMissingOrCorruptCAS(t *testing.T) {
	for _, missing := range []bool{false, true} {
		name := "corrupt"
		if missing {
			name = "missing"
		}
		t.Run(name, func(t *testing.T) {
			original, events, receipt := stepGenerationEvidence(t, false)
			if err := original.Close(); err != nil {
				t.Fatal(err)
			}
			var source structuredStepReceiptRecorded
			for _, event := range events {
				if event.Type == string(EventStructuredStepReceiptRecorded) {
					var err error
					source, err = decodeStructuredStepReceiptEvent(event)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			path := filepath.Join(original.session.Workspace, logsDir, "artifacts", "data", source.Artifact.ID)
			if missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("corrupt fixture"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			restarted := newTestCoordinator(t)
			restarted.session = cloneSession(original.session, original.session.Workspace)
			restarted.initEventStore()
			if !restarted.SessionData().RecoveryRequired {
				t.Fatal("damaged receipt did not block startup admission")
			}
			if _, ok := restarted.executionStepReceiptRegistry().Get(receipt.ID); ok {
				t.Fatal("damaged evidence became authoritative")
			}
			if err := restarted.checkRunAdmission(); err == nil {
				t.Fatal("damaged receipt allowed execution admission")
			}
		})
	}
}
