package auditverify

import (
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

func sampleReceipt() team.ExecutionReceipt {
	exitCode := 0
	return team.ExecutionReceipt{
		RunID: "run-1", TaskID: "t1", Attempt: 1, ModelExecutionID: "exec-1",
		StartedAt:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC),
		ExitCode:   &exitCode, ProducerID: "worker", TranscriptRef: "sha256-abc",
	}
}

func TestHashExecutionReceiptSameInputSameHash(t *testing.T) {
	a, err := HashExecutionReceipt(sampleReceipt())
	if err != nil {
		t.Fatal(err)
	}
	b, err := HashExecutionReceipt(sampleReceipt())
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("identical receipts hashed differently: %s vs %s", a, b)
	}
}

func TestHashExecutionReceiptFieldChangeChangesHash(t *testing.T) {
	base, err := HashExecutionReceipt(sampleReceipt())
	if err != nil {
		t.Fatal(err)
	}
	changed := sampleReceipt()
	changed.Attempt = 2
	changedHash, err := HashExecutionReceipt(changed)
	if err != nil {
		t.Fatal(err)
	}
	if base == changedHash {
		t.Fatal("changing Attempt did not change the receipt hash")
	}
}

func TestNormalizeExecutionReceiptDoesNotMutateOriginal(t *testing.T) {
	original := sampleReceipt()
	original.RepairProvenance = &team.RepairProvenance{Attempted: true}
	clone := NormalizeExecutionReceipt(original)
	clone.RepairProvenance.Attempted = false
	if !original.RepairProvenance.Attempted {
		t.Fatal("mutating the normalized copy mutated the original receipt")
	}
}

func TestNormalizeExecutionReceiptPreservesToolDispositionOrder(t *testing.T) {
	receipt := sampleReceipt()
	receipt.ToolDispositions = []team.ToolExecutionDisposition{
		{ToolName: "bash", Kind: team.ToolExecutionExecuted},
		{ToolName: "read", Kind: team.ToolExecutionPolicyDenied},
	}
	normalized := NormalizeExecutionReceipt(receipt)
	if len(normalized.ToolDispositions) != 2 || normalized.ToolDispositions[0].ToolName != "bash" || normalized.ToolDispositions[1].ToolName != "read" {
		t.Fatalf("tool disposition order changed: %#v", normalized.ToolDispositions)
	}
}
