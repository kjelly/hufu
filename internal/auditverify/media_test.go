package auditverify

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/team"
)

func mediaAuditWorkspace(t *testing.T) (string, media.Ref) {
	t.Helper()
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	ref, err := media.Validate(t.Context(), data.Bytes(), media.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	put, err := store.Put(t.Context(), team.PutArtifactRequest{Kind: "image", Content: data.Bytes()})
	if err != nil || put.ID != ref.ID {
		t.Fatalf("media publication: %v", err)
	}
	policy, err := (media.Policy{Enabled: true, AllowedTargets: []media.Target{{Backend: "fixture", ProviderKey: "fixture", Model: "vision", IdentityHash: "fixture-transport", ImageTokenUpperBound: 8192}}}).Resolve()
	if err != nil {
		t.Fatal(err)
	}
	frozen := &team.MediaPolicySnapshot{Version: media.Version, Policy: policy}
	frozen.Hash, err = media.Digest(frozen)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := &team.ExecutionPolicySnapshot{Version: 5, TeamMaxConcurrent: 1, DefaultLLMBackend: "fixture", Media: frozen,
		Backends:        []team.ExecutionBackendPolicySnapshot{{Backend: "fixture", ProviderKey: "fixture", Kind: "llm", MaxConcurrent: 1, IdentityHash: "fixture-transport"}},
		ModelRoutes:     []team.ExecutionModelRouteSnapshot{{Model: "fixture/vision", Backend: "fixture", ProviderKey: "fixture"}},
		ExecutionWorlds: []team.ExecutionWorldPolicySnapshot{{Backend: "fixture", ExecutionWorld: "local", ProjectRootHash: team.ExecutionPolicyProjectRootHash(workspace)}}}
	encoded, err := json.Marshal(snapshot, json.FormatNilSliceAsNull(true), json.FormatNilMapAsNull(true))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	snapshot.ConfigurationHash = hex.EncodeToString(digest[:])
	if err := team.ValidateExecutionPolicySnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	events, err := team.NewEventStore(workspace, "media-run", "media-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = events.Close() })
	events.SetBranchID("main")
	encoded, err = json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.AppendPersisted(team.RunEvent{Type: string(team.EventExecutionPolicySnapshot), Actor: "coordinator", Payload: encoded}); err != nil {
		t.Fatal(err)
	}
	input := media.InputSnapshot{RunID: "media-run", BranchID: "main", InvocationID: "media-invocation", TurnID: "turn", PolicyHash: frozen.Hash,
		Input: media.TurnInput{Parts: []media.Part{{Type: "text", Text: "inspect image"}, {Type: "image", MediaID: ref.ID}}}, Refs: []media.Ref{ref},
		Occurrences: []media.Occurrence{{RunID: "media-run", BranchID: "main", InvocationID: "media-invocation", TurnID: "turn", PartIndex: 1, MediaID: ref.ID, Origin: "trusted_artifact"}}}
	input, err = media.SealSnapshot(input, policy.Image)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := events.AppendPersisted(team.RunEvent{Type: string(team.EventMediaInputAdmitted), Actor: "user", InvocationID: input.InvocationID, Payload: encoded}); err != nil {
		t.Fatal(err)
	}
	return workspace, ref
}

func TestAuditVerifiesMediaContentBeforeTerminal(t *testing.T) {
	workspace, ref := mediaAuditWorkspace(t)
	result, err := VerifyWorkspaceRun(t.Context(), workspace, "media-run", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Integrity.Status != AuditDimensionPass || result.Completion.Status != AuditDimensionIncomplete {
		t.Fatal("valid media input was not independently verified")
	}
	path := filepath.Join(workspace, "logs", "artifacts", "media", "data", ref.ID)
	if err := os.WriteFile(path, []byte("corrupt media content"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err = VerifyWorkspaceRun(t.Context(), workspace, "media-run", VerifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Integrity.Status != AuditDimensionFail || result.RunInputBinding.Status != AuditDimensionFail {
		t.Fatal("audit trusted event metadata despite corrupt CAS bytes")
	}
}
