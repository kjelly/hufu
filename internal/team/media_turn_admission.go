package team

import (
	"context"
	"errors"

	"github.com/kjelly/hufu/internal/media"
)

type MediaTurnAdmissionState string

const (
	MediaTurnNotCommitted     MediaTurnAdmissionState = "not_committed"
	MediaTurnCommitted        MediaTurnAdmissionState = "committed"
	MediaTurnRecoveryRequired MediaTurnAdmissionState = "recovery_required"
)

// MediaTurnAdmission is a trusted host notification of the canonical input
// checkpoint, independent of provider success and whole-run acceptance.
type MediaTurnAdmission struct {
	State      MediaTurnAdmissionState
	TurnID     string
	SnapshotID string
	ImageCount int
}

func (c *Coordinator) persistMediaInputHistoryWithAdmission(ctx context.Context, snapshot media.InputSnapshot, observer func(MediaTurnAdmission)) error {
	err := c.persistMediaInputHistory(ctx, snapshot)
	if observer == nil {
		return err
	}
	notice := MediaTurnAdmission{State: MediaTurnNotCommitted, TurnID: snapshot.TurnID, SnapshotID: snapshot.ID, ImageCount: len(snapshot.Occurrences)}
	committed, proofErr := c.mediaTurnCheckpointCommitted(snapshot)
	switch {
	case proofErr != nil:
		// An unreadable checkpoint is not proof of an uncommitted turn. Hosts
		// retain a frozen input and must not silently resubmit its bytes.
		notice.State = MediaTurnRecoveryRequired
	case committed:
		notice.State = MediaTurnCommitted
	case err == nil:
		proofErr = media.Reject(media.DurabilityRequired, "image input is absent from its canonical checkpoint")
	}
	observer(notice)
	return errors.Join(err, proofErr)
}

func (c *Coordinator) mediaTurnCheckpointCommitted(snapshot media.InputSnapshot) (bool, error) {
	state, exists, err := LoadConversationCompactionState(c.sessionWorkspace())
	if err != nil || !exists {
		return false, err
	}
	checkpoint, exists := state.Branches[snapshot.BranchID]
	if !exists {
		return false, nil
	}
	message, err := mediaSnapshotMessage(snapshot)
	if err != nil {
		return false, err
	}
	return mediaTurnAlreadyRecorded(checkpoint.History, message, snapshot.ID)
}
