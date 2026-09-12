package auditverify

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestVerifyWorkspaceRunDoesNotCreateMissingEventStore(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing-workspace")
	if _, err := VerifyWorkspaceRun(t.Context(), workspace, "run-missing", VerifyOptions{}); err == nil {
		t.Fatal("verification unexpectedly found a run")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("verification created workspace: %v", err)
	}
}

func TestVerifyLineageRunUsesCallerSelectedLineage(t *testing.T) {
	fixture := buildCompletedRunFixture(t)
	var lineage []team.RunEvent
	if err := team.StreamValidatedRunEvents(t.Context(), fixture.workspace, func(event team.RunEvent) error {
		lineage = append(lineage, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	result, err := VerifyLineageRun(t.Context(), fixture.workspace, fixture.runID, lineage)
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict != AuditVerdictPass {
		t.Fatalf("lineage verdict = %q, want %q", result.Verdict, AuditVerdictPass)
	}
}
