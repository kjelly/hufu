package team

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/runtimestore"
)

func TestMediaTurnAdmissionTracksCanonicalCommitIndependentlyOfFailure(t *testing.T) {
	for _, scenario := range []string{"success", "cancel before checkpoint", "attestation failure", "unreadable checkpoint"} {
		t.Run(scenario, func(t *testing.T) {
			c := newMediaPolicyCoordinator(t)
			ctx := openMediaInvocation(t, c)
			path := filepath.Join(c.projectDir, "source.png")
			if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
				t.Fatal(err)
			}
			snapshot, err := c.admitMediaTurnInput(ctx, "Inspect staged input.", MediaTurnRequest{TurnID: "admission-turn", Images: []string{path, path}})
			if err != nil {
				t.Fatal(err)
			}
			want, wantError := MediaTurnCommitted, false
			switch scenario {
			case "cancel before checkpoint":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
				want, wantError = MediaTurnNotCommitted, true
			case "attestation failure":
				c.eventStore.commitHook = func() error { return errors.New("injected attestation failure") }
				defer func() { c.eventStore.commitHook = nil }()
				wantError = true
			case "unreadable checkpoint":
				putRuntimeDocument(t, c.session.Workspace, runtimestore.DocumentConversationCompactionState, []byte("invalid state"))
				want, wantError = MediaTurnRecoveryRequired, true
			}
			var notices []MediaTurnAdmission
			err = c.persistMediaInputHistoryWithAdmission(ctx, snapshot, func(notice MediaTurnAdmission) { notices = append(notices, notice) })
			if (err != nil) != wantError || len(notices) != 1 || notices[0].State != want || notices[0].TurnID != snapshot.TurnID || notices[0].SnapshotID != snapshot.ID || notices[0].ImageCount != 2 {
				t.Fatalf("admission state: notices=%+v error=%v", notices, err)
			}
			if scenario == "attestation failure" {
				c.eventStore.commitHook = nil
				runID, sessionID := c.eventStore.runID, c.eventStore.sessionID
				if err := c.eventStore.Close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := NewEventStore(c.session.Workspace, runID, sessionID)
				if err != nil {
					t.Fatal(err)
				}
				reopened.SetBranchID("main")
				c.eventStore = reopened
				c.SetEventJournal(eventStoreJournal{store: reopened})
				if err := c.reconcileCompactionState(context.Background(), "main"); err != nil {
					t.Fatal("committed input could not recover after failed notification evidence", err)
				}
				if err := c.restoreCanonicalCompactionForBranch("main"); err != nil || len(historyReferences(c.conversationHistory)) != 2 {
					t.Fatal("committed input was lost after failed attestation", err)
				}
			}
		})
	}
}

func TestStagedMediaIngressRejectsMixedOrReleasedSources(t *testing.T) {
	c := newMediaPolicyCoordinator(t)
	ctx := openMediaInvocation(t, c)
	path := filepath.Join(c.projectDir, "source.png")
	if err := os.WriteFile(path, mediaStorePNG(t), 0o600); err != nil {
		t.Fatal(err)
	}
	staged, err := media.StageImage(ctx, path, media.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(staged.Close)
	_, err = c.admitMediaTurnInput(ctx, "Inspect input.", MediaTurnRequest{Images: []string{path}, StagedImages: []*media.StagedImage{staged}})
	requireMediaCode(t, err, media.BindingConflict)
	staged.Close()
	_, err = c.admitMediaTurnInput(ctx, "Inspect input.", MediaTurnRequest{StagedImages: []*media.StagedImage{staged}})
	requireMediaCode(t, err, media.InputMissing)
	view, err := c.mediaLineage(ctx)
	if err != nil || len(view.snapshots) != 0 {
		t.Fatal("rejected staging sources created an admitted input snapshot", err)
	}
}
