package auditverify

import (
	"context"

	"github.com/kjelly/hufu/internal/team"
)

func verifyRunMedia(ctx context.Context, workspace, runID string, lineage []team.RunEvent, terminals []team.RunEvent) error {
	cutoff := -1
	for index, event := range lineage {
		if event.RunID == runID {
			cutoff = index
		}
		if len(terminals) != 0 && event.ID == terminals[0].ID {
			cutoff = index
			break
		}
	}
	if cutoff < 0 {
		return nil
	}
	return team.VerifyNativeMediaLineage(ctx, workspace, lineage[:cutoff+1])
}

func mediaIntegrityFailure(runID string, err error) *AuditVerificationResult {
	result := &AuditVerificationResult{SchemaVersion: AuditSchemaVersion, RunID: runID}
	result.Integrity = AuditDimensionResult{Status: AuditDimensionFail, Reason: err.Error()}
	result.SemanticRegression = AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "native media integrity unavailable"}
	result.RunInputBinding = AuditDimensionResult{Status: AuditDimensionFail, Reason: err.Error()}
	result.addFinding(CodeBindingConflict, FindingSeverityCritical, err.Error(), "", 0, "")
	result.finalizeVerdict()
	return result
}
