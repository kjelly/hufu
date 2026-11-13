package team

import "fmt"

// AuthorizeSessionRestore binds task restoration to an explicitly selected,
// immutable canonical session. It does not change the execution profile or
// authorize policy drift, old journal/cache seeds, or cross-request memory.
// Call before SetSessionData and before starting a run.
func (c *Coordinator) AuthorizeSessionRestore(source *ResumeSource) error {
	if c == nil || c.session == nil || source == nil || source.Session == nil ||
		source.workspace != c.session.Workspace || source.BranchID == "" || source.eventHead == "" || c.executionRunID != "" {
		return fmt.Errorf("session restore requires an exact canonical source before execution")
	}
	if digest, err := collectionDigest(source.Session); err != nil || digest != source.sessionSHA256 {
		return fmt.Errorf("session restore source projection changed")
	}
	profile := c.ExecutionProfile()
	if profile.DisableHistoricalTaskReuse || profile.DisableJournalRestore {
		policy := source.Session.ExecutionPolicySnapshot
		if policy == nil || policy.Launch == nil || policy.Launch.Options.ExecutionProfile != string(profile.Name) {
			return fmt.Errorf("fresh-session restore requires the original saved execution profile")
		}
		var selected *RunInputSnapshot
		for i := range source.Session.RunInputSnapshots {
			snapshot := &source.Session.RunInputSnapshots[i]
			if snapshot.ID == source.Session.ActiveRunInputSnapshotID {
				if selected != nil || ValidateRunInputSnapshot(snapshot) != nil {
					return fmt.Errorf("session restore requires one verified frozen input snapshot")
				}
				selected = snapshot
			}
		}
		if selected == nil {
			return fmt.Errorf("fresh-session restore requires frozen inputs")
		}
		for _, item := range source.Session.Tasks {
			if item != nil && !IsPrimaryOccurrence(item) && (item.RunInputSnapshotID != selected.ID || item.RunInputSnapshotHash != selected.SnapshotHash) {
				return fmt.Errorf("fresh-session restore refuses task %s from a different request", item.ID)
			}
		}
	}
	c.sessionResume.Store(true)
	return nil
}

func (c *Coordinator) restoresSessionTasks() bool {
	profile := c.ExecutionProfile()
	return c.sessionResume.Load() || (!profile.DisableHistoricalTaskReuse && !profile.DisableJournalRestore)
}
