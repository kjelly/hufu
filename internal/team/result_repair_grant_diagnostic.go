package team

import "encoding/json/v2"

// Report proof differences without printing schemas, prompts or payload bytes.
func recoveryGrantDiagnostic(a *ResultCollectionRecoveryAssessment) string {
	if a == nil {
		return "null"
	}
	checks, _ := collectionDigest(a.Checks)
	failed := []ResultCollectionRecoveryCheck{}
	for _, check := range a.Checks {
		if !check.Passed {
			failed = append(failed, check)
		}
	}
	raw, _ := json.Marshal(struct {
		Task       string                              `json:"task"`
		Branch     string                              `json:"branch"`
		Head       string                              `json:"head"`
		Checkpoint string                              `json:"checkpoint"`
		State      ResultCollectionRecoveryDisposition `json:"disposition"`
		Reason     RepairFailureReason                 `json:"reason"`
		Turns      [3]int                              `json:"turns"`
		Accepted   *int                                `json:"accepted"`
		Continues  bool                                `json:"continues"`
		Checks     string                              `json:"checks_sha256"`
		Failed     []ResultCollectionRecoveryCheck     `json:"failed"`
		Proof      string                              `json:"fingerprint"`
	}{a.TaskID, a.BranchID, a.JournalHeadID, a.CheckpointSHA256, a.Disposition, a.OriginalReason,
		[3]int{a.TurnsUsed, a.RemainingTurns, a.AdditionalTurns}, a.AcceptedTasks, a.ContinuesWorkflow, checks, failed, a.Fingerprint})
	return string(raw)
}
