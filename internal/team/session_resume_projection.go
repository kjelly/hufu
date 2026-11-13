package team

import "fmt"

// Switching branches rebuilds the checkpoint during activation. Resuming the
// already active branch keeps its checkpoint, so preview must validate the
// same projection that normal startup will compare, not just canonical tasks.
func validateResumeCheckpointProjection(source *ResumeSource) error {
	if source.BranchID != source.activeBranch {
		return nil
	}
	checkpoint, exists, err := LoadSessionReadOnly(source.workspace)
	if err != nil {
		return err
	}
	if !exists || checkpoint == nil {
		return nil
	}
	replayed := ReduceToSessionData(source.lineage)
	comparisonErr := CompareCanonicalProjection(checkpoint, source.lineage)
	if checkpoint.RecoveryRequired {
		if comparisonErr != nil ||
			!isRecordedProjectionRecovery(checkpoint, replayed, source.lineage) {
			return fmt.Errorf("resume preflight requires checkpoint recovery: %s", checkpoint.RecoveryReason)
		}
	}
	if !hasCurrentCanonicalProjectionEvents(source.lineage) ||
		(len(checkpoint.Entries) == 0 && len(checkpoint.Tasks) == 0 && checkpoint.ExecutionPolicySnapshot == nil) {
		return nil
	}
	if comparisonErr != nil &&
		!isProjectionPrefixOrRecoverable(checkpoint, replayed, source.Session.Tasks) {
		return fmt.Errorf("resume preflight checkpoint projection mismatch: %w", comparisonErr)
	}
	return nil
}
