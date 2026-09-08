package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Phase 2 tests (spec.md §36 PR-04): the untrusted WorkerResultProposal
// boundary. Every test here uses a fake external provider's raw JSON output
// (never a real Codex process) plus a genuine WorkspaceSnapshot/Delta from a
// real temp workspace, so the canonicalizer is exercised against actual
// bytes, not stand-ins.

func validProposalJSON(extra string) string {
	base := `{"status":"success","summary":"did the work"`
	if extra != "" {
		base += "," + extra
	}
	return base + "}"
}

// TestExternalProviderCannotSetTaskID proves TaskID (and RunID/Attempt/Agent)
// have no field on WorkerResultProposal at all: supplying one fails strict
// decode, and the canonical result's identity always comes from
// AttemptRequest regardless of what a well-formed proposal claims.
func TestExternalProviderCannotSetTaskID(t *testing.T) {
	for _, key := range []string{`"task_id":"forged-task"`, `"run_id":"forged-run"`, `"attempt":99`, `"agent":"forged-agent"`} {
		if _, err := DecodeWorkerResultProposal([]byte(validProposalJSON(key))); err == nil {
			t.Fatalf("decode accepted forged field %s, want strict rejection", key)
		}
	}

	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON("")))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "real-task", RunID: "real-run", Attempt: 3, Task: TaskDef{Agent: "real-agent"}},
		AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if result.TaskID != "real-task" || result.Attempt != 3 || result.Agent != "real-agent" {
		t.Fatalf("canonical identity = %#v, want it sourced only from AttemptRequest", result)
	}
}

// TestExternalProviderCannotForgeReceipt proves ExecutionReceipt/ReceiptIDs
// are unreachable through the proposal: no field exists to smuggle one in,
// and canonicalization never populates ReceiptIDs (§9.1, §19: "populated
// only later by Hufu-owned logic").
func TestExternalProviderCannotForgeReceipt(t *testing.T) {
	if _, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"receipt_ids":["forged-receipt"]`))); err == nil {
		t.Fatal("decode accepted a forged receipt_ids field")
	}
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON("")))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ReceiptIDs) != 0 {
		t.Fatalf("ReceiptIDs = %#v, want none from canonicalization alone", result.ReceiptIDs)
	}
}

// TestExternalProviderCannotForgeEvidence proves EvidenceRef (with its
// SystemHMAC) is unreachable through the proposal.
func TestExternalProviderCannotForgeEvidence(t *testing.T) {
	if _, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"evidence":[{"type":"forged","description":"forged"}]`))); err == nil {
		t.Fatal("decode accepted a forged evidence field")
	}
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON("")))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("Evidence = %#v, want none from canonicalization alone", result.Evidence)
	}
}

// TestExternalProviderCannotForgeVerification proves VerificationResult is
// unreachable through the proposal, and Verification is only ever populated
// by Hufu's own later verification step, never by canonicalization.
func TestExternalProviderCannotForgeVerification(t *testing.T) {
	if _, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"verification":[{"exit_code":0}]`))); err == nil {
		t.Fatal("decode accepted a forged verification field")
	}
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON("")))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Verification) != 0 {
		t.Fatalf("Verification = %#v, want none: only Hufu verification may set this", result.Verification)
	}
}

// TestExternalProviderCannotForgeArtifactHash proves ProposedFile has no
// SHA256/byte-size field to forge, and the canonical ArtifactRef's hash
// always comes from Hufu's own observed WorkspaceDelta, computed from actual
// bytes — never from anything the provider claims.
func TestExternalProviderCannotForgeArtifactHash(t *testing.T) {
	if _, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"proposed_files":[{"path":"out.txt","sha256":"deadbeef"}]`))); err == nil {
		t.Fatal("decode accepted a forged sha256 field on a proposed file")
	}

	root := t.TempDir()
	realContent := []byte("the actual bytes hufu observed")
	if err := os.WriteFile(filepath.Join(root, "out.txt"), realContent, 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := NewWorkspaceSnapshotter().Snapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	realState, err := snap.fileState("out.txt")
	if err != nil {
		t.Fatal(err)
	}
	delta := WorkspaceDelta{Added: []WorkspaceFileState{realState}}

	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"proposed_files":[{"path":"out.txt"}]`)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, delta, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Artifacts) != 1 || result.Artifacts[0].SHA256 != realState.SHA256 || result.Artifacts[0].SHA256 == "deadbeef" {
		t.Fatalf("canonical artifact = %#v, want the real observed hash %q", result.Artifacts, realState.SHA256)
	}
}

// TestExternalResultCanonicalUsesActualWorkspaceDelta proves FilesModified
// always reflects Hufu's observed delta regardless of what the provider
// mentions: an omitted modified file is still recorded, and a fabricated
// file the delta never observed is not (§19, §9.4).
func TestExternalResultCanonicalUsesActualWorkspaceDelta(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "real-change.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshotter := NewWorkspaceSnapshotter()
	baseline, err := snapshotter.Snapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real-change.txt"), []byte("v2, actually modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := snapshotter.Snapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := snapshotter.Diff(context.Background(), baseline, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(delta.Modified) != 1 || delta.Modified[0].Path != "real-change.txt" {
		t.Fatalf("test setup delta = %#v, want exactly real-change.txt modified", delta)
	}

	// The proposal omits the real change entirely and instead claims a file
	// that was never touched.
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"proposed_files":[{"path":"fabricated.txt"}]`)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, delta, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FilesModified) != 1 || result.FilesModified[0].Path != "real-change.txt" {
		t.Fatalf("FilesModified = %#v, want the actual delta's real-change.txt despite the provider omitting it", result.FilesModified)
	}
	if len(result.Artifacts) != 0 {
		t.Fatalf("Artifacts = %#v, want none: the fabricated file has no corroborating delta or disk evidence", result.Artifacts)
	}
}

// TestExternalResultRejectsOutsideWorkspaceArtifact proves a proposed path
// escaping the authorized workspace root fails canonicalization for a
// grounded-result task, and is at minimum dropped (never trusted as an
// artifact) otherwise (§9.4, §SEC-09).
func TestExternalResultRejectsOutsideWorkspaceArtifact(t *testing.T) {
	root := t.TempDir()
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"proposed_files":[{"path":"../../etc/passwd"}]`)))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("grounded result task fails closed", func(t *testing.T) {
		_, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
			AttemptRequest{Provider: "codex", TaskID: "t", Task: TaskDef{Execution: ExecutionContract{RequiresGroundedResult: true}}},
			AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("Canonicalize error = %v, want an outside-workspace rejection", err)
		}
	})

	t.Run("non-grounded task drops the claim instead of trusting it", func(t *testing.T) {
		result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
			AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Artifacts) != 0 {
			t.Fatalf("Artifacts = %#v, want none: an outside-workspace path must never become a trusted artifact", result.Artifacts)
		}
	})
}

// TestExternalResultCanonicalizesFilesReadFromDelta proves the §38/§9.1
// files_read extension: a claimed files_read path that is corroborated by
// the actually-observed delta becomes a canonical FilesRead entry.
func TestExternalResultCanonicalizesFilesReadFromDelta(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "reviewed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := NewWorkspaceSnapshotter().Snapshot(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	state, err := snap.fileState("reviewed.go")
	if err != nil {
		t.Fatal(err)
	}
	delta := WorkspaceDelta{Added: []WorkspaceFileState{state}}

	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["reviewed.go"]`)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, delta, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FilesRead) != 1 || result.FilesRead[0].Path != "reviewed.go" {
		t.Fatalf("FilesRead = %#v, want exactly reviewed.go", result.FilesRead)
	}
}

// TestExternalResultCanonicalizesFilesReadFromLiveWorkspace proves a
// files_read claim for a pre-existing file the delta never touched (the
// provider read it but did not modify it) is still verified — against the
// live workspace, not the delta alone — and accepted.
func TestExternalResultCanonicalizesFilesReadFromLiveWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unchanged.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["unchanged.go"]`)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FilesRead) != 1 || result.FilesRead[0].Path != "unchanged.go" {
		t.Fatalf("FilesRead = %#v, want exactly unchanged.go verified against the live workspace", result.FilesRead)
	}
}

// TestExternalResultRejectsFabricatedFilesReadUnderGroundedResult proves a
// files_read claim naming a file that never existed at all fails a
// grounded-result attempt closed, the same provider_claimed_missing_file
// treatment §9.4 already gives ProposedFiles.
func TestExternalResultRejectsFabricatedFilesReadUnderGroundedResult(t *testing.T) {
	root := t.TempDir()
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["never-existed.go"]`)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t", Task: TaskDef{Execution: ExecutionContract{RequiresGroundedResult: true}}},
		AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("Canonicalize error = %v, want a does-not-exist rejection", err)
	}
}

// TestExternalResultDropsFabricatedFilesReadWhenNotGrounded proves the same
// fabricated claim is merely dropped, not trusted, for a non-grounded task.
func TestExternalResultDropsFabricatedFilesReadWhenNotGrounded(t *testing.T) {
	root := t.TempDir()
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["never-existed.go"]`)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		AttemptRequest{Provider: "codex", TaskID: "t"}, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FilesRead) != 0 {
		t.Fatalf("FilesRead = %#v, want none: an unverifiable claim must never become a trusted FilesRead entry", result.FilesRead)
	}
}
