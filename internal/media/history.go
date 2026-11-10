package media

import "strings"

// HistoryReference is an inert description of an admitted image occurrence.
// It grants no bytes by itself; execution must resolve its canonical source
// and current task lease. TaskID is empty for an original input-turn reference.
type HistoryReference struct {
	Version     int       `json:"version"`
	Source      Selection `json:"source"`
	Ref         Ref       `json:"ref"`
	PolicyHash  string    `json:"policy_hash"`
	BindingHash string    `json:"binding_hash"`
	TaskID      string    `json:"task_id,omitempty"`
}

func (r HistoryReference) Validate() error {
	if r.Version != Version || !validHash(r.PolicyHash) || !validHash(r.BindingHash) || !validHash(r.Source.SnapshotHash) ||
		!validIdentity(r.Source.TurnID) || r.Source.PartIndex < 0 || r.Source.PartIndex >= 128 {
		return Reject(BindingConflict, "invalid history media reference")
	}
	digest := strings.TrimPrefix(r.Source.SnapshotHash, "sha256:")
	if r.Source.SnapshotID != "media-input-v1-"+digest {
		return Reject(IntegrityMismatch, "history source identity differs from its hash")
	}
	if r.TaskID == "" {
		if r.BindingHash != r.Source.SnapshotHash {
			return Reject(BindingConflict, "input history binding differs from its snapshot")
		}
	} else if !validIdentity(r.TaskID) {
		return Reject(BindingConflict, "invalid history task owner")
	}
	return r.Ref.Validate()
}
