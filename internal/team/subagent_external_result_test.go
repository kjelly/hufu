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

func scopedAttemptRequest(provider, taskID string) AttemptRequest {
	const runID = "run-test"
	const attempt = 1
	return AttemptRequest{
		Provider: provider, RunID: runID, TaskID: taskID, Attempt: attempt,
		ArtifactScope: &ArtifactAccessScope{RunID: runID, TaskID: taskID, Attempt: attempt},
	}
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
	request := scopedAttemptRequest("codex", "real-task")
	request.Attempt = 3
	request.ArtifactScope.Attempt = 3
	request.Task = TaskDef{Agent: "real-agent"}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		request,
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
		scopedAttemptRequest("codex", "t"), AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, t.TempDir())
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
		scopedAttemptRequest("codex", "t"), AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, t.TempDir())
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
		scopedAttemptRequest("codex", "t"), AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, t.TempDir())
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
	request := scopedAttemptRequest("codex", "t")

	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"proposed_files":[{"path":"out.txt"}]`)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		request, AttemptResult{ResultProposal: proposal}, delta, root)
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
	request := scopedAttemptRequest("codex", "t")

	// The proposal omits the real change entirely and instead claims a file
	// that was never touched.
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"proposed_files":[{"path":"fabricated.txt"}]`)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
		request, AttemptResult{ResultProposal: proposal}, delta, root)
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
		request := scopedAttemptRequest("codex", "t")
		request.Task = TaskDef{Execution: ExecutionContract{RequiresGroundedResult: true}}
		_, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
			request,
			AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Fatalf("Canonicalize error = %v, want an outside-workspace rejection", err)
		}
	})

	t.Run("non-grounded task drops the claim instead of trusting it", func(t *testing.T) {
		request := scopedAttemptRequest("codex", "t")
		result, err := NewExternalResultCanonicalizer().Canonicalize(context.Background(),
			request, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
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
		scopedAttemptRequest("codex", "t"), AttemptResult{ResultProposal: proposal}, delta, root)
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
		scopedAttemptRequest("codex", "t"), AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FilesRead) != 1 || result.FilesRead[0].Path != "unchanged.go" {
		t.Fatalf("FilesRead = %#v, want exactly unchanged.go verified against the live workspace", result.FilesRead)
	}
}

func TestExternalResultRejectsMissingOrMismatchedScopeForWorkspaceFilesRead(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "reviewed.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["reviewed.go"]`)))
	if err != nil {
		t.Fatal(err)
	}
	base := scopedAttemptRequest("codex", "consumer")
	cases := []struct {
		name    string
		request AttemptRequest
	}{
		{name: "missing scope", request: func() AttemptRequest {
			request := base
			request.ArtifactScope = nil
			return request
		}()},
		{name: "missing request run", request: func() AttemptRequest {
			request := base
			request.RunID = ""
			return request
		}()},
		{name: "mismatched scope task", request: func() AttemptRequest {
			request := base
			request.ArtifactScope = cloneArtifactAccessScope(base.ArtifactScope)
			request.ArtifactScope.TaskID = "other-task"
			return request
		}()},
		{name: "mismatched scope attempt", request: func() AttemptRequest {
			request := base
			request.ArtifactScope = cloneArtifactAccessScope(base.ArtifactScope)
			request.ArtifactScope.Attempt = 2
			return request
		}()},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), test.request,
				AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root); err == nil {
				t.Fatal("canonicalization accepted workspace evidence without an exact attempt scope")
			}
		})
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
		func() AttemptRequest {
			request := scopedAttemptRequest("codex", "t")
			request.Task = TaskDef{Execution: ExecutionContract{RequiresGroundedResult: true}}
			return request
		}(),
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
		scopedAttemptRequest("codex", "t"), AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.FilesRead) != 0 {
		t.Fatalf("FilesRead = %#v, want none: an unverifiable claim must never become a trusted FilesRead entry", result.FilesRead)
	}
}

func TestExternalResultCanonicalizesOnlyAttemptScopedArtifactReferences(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Put(t.Context(), PutArtifactRequest{
		Kind: "workset_input", Role: "input", Path: "runtime/input.md", Content: []byte("assigned evidence"),
		RunID: "run-1", TaskID: "producer", Attempt: 1, Agent: "producer",
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := stored.ArtifactRef

	canonicalize := func(t *testing.T, request AttemptRequest, claimed string) (*TaskResult, error) {
		t.Helper()
		proposal, decodeErr := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["` + claimed + `"]`)))
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return NewExternalResultCanonicalizer().Canonicalize(t.Context(), request,
			AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	}

	request := AttemptRequest{
		Provider: "codex", RunID: "run-1", TaskID: "consumer", Attempt: 1,
		ArtifactScope: &ArtifactAccessScope{
			RunID: "run-1", TaskID: "consumer", Attempt: 1, StoreRoot: root,
			AuthorizedRefs: []ArtifactRef{ref},
		},
	}
	result, err := canonicalize(t, request, ref.ID)
	if err != nil {
		t.Fatalf("authorized opaque ref rejected: %v", err)
	}
	if len(result.FilesRead) != 1 || result.FilesRead[0] != (FileRef{Path: ref.ID, Purpose: "artifact"}) {
		t.Fatalf("FilesRead = %#v, want the authorized opaque artifact ref", result.FilesRead)
	}

	tests := []struct {
		name    string
		request AttemptRequest
		claimed string
	}{
		{name: "unknown", request: request, claimed: ref.ID + "-unknown"},
		{name: "wrong task scope", request: func() AttemptRequest {
			wrong := request
			wrong.TaskID = "other-consumer"
			return wrong
		}(), claimed: ref.ID},
		{name: "tampered digest", request: func() AttemptRequest {
			tampered := ref
			tampered.SHA256 = strings.Repeat("0", 64)
			wrong := request
			wrong.ArtifactScope = cloneArtifactAccessScope(request.ArtifactScope)
			wrong.ArtifactScope.AuthorizedRefs = []ArtifactRef{tampered}
			return wrong
		}(), claimed: ref.ID},
		{name: "missing request run", request: func() AttemptRequest {
			missing := request
			missing.RunID = ""
			return missing
		}(), claimed: ref.ID},
		{name: "missing scope run", request: func() AttemptRequest {
			missing := request
			missing.ArtifactScope = cloneArtifactAccessScope(request.ArtifactScope)
			missing.ArtifactScope.RunID = ""
			return missing
		}(), claimed: ref.ID},
		{name: "wrong run scope", request: func() AttemptRequest {
			wrong := request
			wrong.ArtifactScope = cloneArtifactAccessScope(request.ArtifactScope)
			wrong.ArtifactScope.RunID = "run-other"
			return wrong
		}(), claimed: ref.ID},
		{name: "stale path provenance", request: func() AttemptRequest {
			stale := ref
			stale.Path = "runtime/old-input.md"
			wrong := request
			wrong.ArtifactScope = cloneArtifactAccessScope(request.ArtifactScope)
			wrong.ArtifactScope.AuthorizedRefs = []ArtifactRef{stale}
			return wrong
		}(), claimed: ref.ID},
		{name: "stale run provenance", request: func() AttemptRequest {
			stale := ref
			stale.RunID = "run-old"
			wrong := request
			wrong.ArtifactScope = cloneArtifactAccessScope(request.ArtifactScope)
			wrong.ArtifactScope.AuthorizedRefs = []ArtifactRef{stale}
			return wrong
		}(), claimed: ref.ID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := canonicalize(t, test.request, test.claimed); err == nil {
				t.Fatal("canonicalization accepted an unauthorized or tampered opaque ref")
			}
		})
	}
}

func TestExternalResultCanonicalizesCrossRunDuplicateArtifactReference(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("assigned evidence")
	first, err := store.Put(t.Context(), PutArtifactRequest{
		Kind: "workset_input", Role: "input", Path: "runtime/input.md", Description: "assigned evidence",
		Content: content, RunID: "run-old", TaskID: "producer-old", Attempt: 1, Agent: "producer",
		Provider: "command:go",
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.Put(t.Context(), PutArtifactRequest{
		Kind: "workset_input", Role: "input", Path: "runtime/input.md", Description: "assigned evidence",
		Content: content, RunID: "run-current", TaskID: "producer-current", Attempt: 2, Agent: "producer",
		Provider: "command:go",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != current.ID {
		t.Fatalf("duplicate content IDs = %q and %q, want one content-addressed ID", first.ID, current.ID)
	}
	storedMetadata, err := store.Get(t.Context(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if storedMetadata.RunID != "run-old" || storedMetadata.TaskID != "producer-old" {
		t.Fatalf("CAS metadata occurrence = %q/%q, want first writer occurrence", storedMetadata.RunID, storedMetadata.TaskID)
	}

	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["` + current.ID + `"]`)))
	if err != nil {
		t.Fatal(err)
	}
	request := AttemptRequest{
		Provider: "codex", RunID: "run-current", TaskID: "consumer", Attempt: 1,
		ArtifactScope: &ArtifactAccessScope{
			RunID: "run-current", TaskID: "consumer", Attempt: 1, StoreRoot: root,
			AuthorizedRefs: []ArtifactRef{current.ArtifactRef},
		},
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), request,
		AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err != nil {
		t.Fatalf("same-content artifact from current occurrence rejected: %v", err)
	}
	if len(result.FilesRead) != 1 || result.FilesRead[0] != (FileRef{Path: current.ID, Purpose: "artifact"}) {
		t.Fatalf("FilesRead = %#v, want the current authorized opaque artifact ref", result.FilesRead)
	}
}

func TestExternalResultRejectsUnscopedCustomArtifactIDThatCollidesWithWorkspacePath(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	const customID = "review.md"
	if err := os.WriteFile(filepath.Join(root, customID), []byte("workspace file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(t.Context(), PutArtifactRequest{
		ID: customID, Kind: "task_output", Role: "evidence", Path: customID, Content: []byte("artifact bytes"),
	}); err != nil {
		t.Fatal(err)
	}
	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["` + customID + `"]`)))
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewExternalResultCanonicalizer().Canonicalize(t.Context(), AttemptRequest{
		Provider: "codex", RunID: "run-1", TaskID: "consumer", Attempt: 1,
		Task:          TaskDef{Execution: ExecutionContract{RequiresGroundedResult: true}},
		ArtifactScope: &ArtifactAccessScope{RunID: "run-1", TaskID: "consumer", Attempt: 1, StoreRoot: root},
	}, AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err == nil || !strings.Contains(err.Error(), "unauthorized artifact") {
		t.Fatalf("custom CAS ID collision error = %v, want fail-closed unauthorized artifact", err)
	}
}

func TestExternalResultCanonicalizesOnlyAttemptScopedManagedSkill(t *testing.T) {
	root := t.TempDir()
	store, err := NewFileArtifactStore(root, root)
	if err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(t.TempDir(), "hufu-runtime-code-review", "SKILL.md")
	content := []byte("immutable review instructions")
	stored, err := store.Put(t.Context(), PutArtifactRequest{
		ID: "skill-" + strings.Repeat("a", 64) + "-path", Kind: "skill", Role: "instruction", Path: skillPath,
		Description: "hufu-runtime-code-review", Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	ref := stored.ArtifactRef
	request := AttemptRequest{
		Provider: "codex", RunID: "run-1", TaskID: "consumer", Attempt: 1,
		Task: TaskDef{Execution: ExecutionContract{RequiresGroundedResult: true}},
		ArtifactScope: &ArtifactAccessScope{
			RunID: "run-1", TaskID: "consumer", Attempt: 1, StoreRoot: root,
			ManagedSkillRefs: []ArtifactRef{ref},
		},
	}

	proposal, err := DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["` + skillPath + `"]`)))
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), request,
		AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root)
	if err != nil {
		t.Fatalf("authorized managed skill rejected: %v", err)
	}
	if len(result.FilesRead) != 1 || result.FilesRead[0] != (FileRef{Path: ref.ID, Purpose: "skill"}) {
		t.Fatalf("FilesRead = %#v, want the opaque managed-skill ref", result.FilesRead)
	}

	proposal, err = DecodeWorkerResultProposal([]byte(validProposalJSON(`"files_read":["` + filepath.Join(t.TempDir(), "unregistered", "SKILL.md") + `"]`)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewExternalResultCanonicalizer().Canonicalize(t.Context(), request,
		AttemptResult{ResultProposal: proposal}, WorkspaceDelta{}, root); err == nil {
		t.Fatal("canonicalization accepted an arbitrary absolute skill path")
	}
}
