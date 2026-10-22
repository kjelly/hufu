package improver

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/cost"
	"github.com/kjelly/hufu/internal/improve/paired"
	"github.com/kjelly/hufu/internal/team"
)

// ChildEvidence contains imported canonical bytes, never reconstructed
// receipts. It is stored only after the executor has drained and closed its
// runtime. A missing import is not proof that a child was never dispatched.
type ChildEvidence struct {
	SchemaVersion int                    `json:"schema_version"`
	Identity      paired.RuntimeIdentity `json:"identity"`
	EventLog      paired.ArtifactRef     `json:"event_log_ref"`
	Artifacts     []ImportedArtifact     `json:"artifacts"`
	Roots         map[string]string      `json:"roots"`
	Policy        PolicyProjection       `json:"policy"`
	Overlay       RuntimeOverlay         `json:"overlay"`
}

type ImportedArtifact struct {
	Original team.ArtifactRef   `json:"original"`
	Content  paired.ArtifactRef `json:"content_ref"`
}

type childObservation struct {
	session        *team.SessionData
	events         []team.RunEvent
	tokens         int64
	usageKnown     bool
	calls          int
	providerFailed bool
}

type boundedEventLog struct{ bytes.Buffer }

func (b *boundedEventLog) Write(raw []byte) (int, error) {
	if len(raw) > paired.MaxTotalBytes-b.Len() {
		return 0, errors.New("child event log exceeds artifact limit")
	}
	return b.Buffer.Write(raw)
}

func importChildEvidence(ctx context.Context, objects Objects, runtime *probeRuntime, identity paired.RuntimeIdentity) (ChildEvidence, error) {
	if !runtime.closed.Load() {
		return ChildEvidence{}, errors.New("child must be drained and closed before evidence import")
	}
	var log boundedEventLog
	if err := team.ExportEventLog(ctx, runtime.session.Workspace, &log); err != nil {
		return ChildEvidence{}, err
	}
	// Validate before publishing any reference, including schema/idempotency
	// rules in addition to the exporter's hash-chain validation.
	events, err := team.DecodeEventLog(bytes.NewReader(log.Bytes()))
	if err != nil {
		return ChildEvidence{}, err
	}
	session, err := validateChildLineage(events, identity)
	if err != nil {
		return ChildEvidence{}, err
	}
	source, err := team.OpenFileArtifactStoreReadOnly(runtime.session.Workspace, runtime.session.Workspace)
	if err != nil {
		return ChildEvidence{}, err
	}
	if session.RunResult.EvidenceManifest == nil {
		return ChildEvidence{}, errors.New("child has no canonical evidence manifest")
	}
	if err := session.RunResult.EvidenceManifest.Verify(ctx, source); err != nil {
		return ChildEvidence{}, fmt.Errorf("canonical child manifest: %w", err)
	}
	evidence := ChildEvidence{
		SchemaVersion: 1, Identity: identity, Policy: runtime.policy, Overlay: runtime.overlay,
		Roots: map[string]string{"subject_root": runtime.session.Scope.SubjectRoot, "control_root": runtime.session.Workspace, "team_root": runtime.session.Dir},
	}
	// []byte encoding keeps payload bytes exact, including their original
	// JSON field order, so the exported chain remains verifiable offline.
	evidence.EventLog, err = objects.PutJSON("child_event_log", log.Bytes())
	if err != nil {
		return ChildEvidence{}, err
	}
	ids := map[string]bool{}
	for _, ref := range session.RunResult.EvidenceManifest.ArtifactRefs {
		ids[ref.ID] = true
	}
	for _, task := range session.Tasks {
		for _, receipt := range task.ExecutionReceipts {
			if receipt.TranscriptRef != "" {
				ids[receipt.TranscriptRef] = true
			}
		}
	}
	keys := make([]string, 0, len(ids))
	for id := range ids {
		keys = append(keys, id)
	}
	slices.Sort(keys)
	for _, id := range keys {
		original, err := source.Get(ctx, id)
		if err != nil {
			return ChildEvidence{}, err
		}
		if err := source.Verify(ctx, original); err != nil {
			return ChildEvidence{}, err
		}
		reader, err := source.Open(ctx, id)
		if err != nil {
			return ChildEvidence{}, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(reader, paired.MaxTotalBytes+1))
		if err := errors.Join(readErr, reader.Close()); err != nil {
			return ChildEvidence{}, err
		}
		if len(raw) > paired.MaxTotalBytes {
			return ChildEvidence{}, errors.New("child artifact exceeds import limit")
		}
		ref, err := objects.PutJSON("child_artifact", raw)
		if err != nil {
			return ChildEvidence{}, err
		}
		evidence.Artifacts = append(evidence.Artifacts, ImportedArtifact{Original: original, Content: ref})
	}
	if _, err := inspectChildEvidence(ctx, objects, evidence); err != nil {
		return ChildEvidence{}, err
	}
	return evidence, nil
}

func validateChildLineage(events []team.RunEvent, identity paired.RuntimeIdentity) (*team.SessionData, error) {
	if !opaqueID(identity.InvocationID) || !opaqueID(identity.RunID) || !opaqueID(identity.BranchID) {
		return nil, errors.New("child identity is incomplete")
	}
	starts, finishes := 0, 0
	for _, event := range events {
		if event.Type != "run_started" && event.Type != "run_finished" && event.TaskID == "" &&
			!strings.HasPrefix(event.Type, "cost_") {
			continue // Includes the pre-invocation frozen policy event.
		}
		if event.RunID != identity.RunID || event.InvocationID != identity.InvocationID || event.BranchID != identity.BranchID {
			return nil, errors.New("child contains foreign invocation evidence")
		}
		switch event.Type {
		case "run_started":
			starts++
		case "run_finished":
			finishes++
		}
	}
	session := team.ReduceToSessionData(events)
	if starts != 1 || finishes != 1 || session.RecoveryRequired || session.PendingTerminalCommit != nil ||
		session.RunResult == nil || session.RunResult.RunID != identity.RunID || session.ExecutionPolicySnapshot == nil {
		return nil, errors.New("child has incomplete or conflicting canonical completion")
	}
	if err := team.ValidateExecutionPolicySnapshot(session.ExecutionPolicySnapshot); err != nil {
		return nil, err
	}
	return session, nil
}

func inspectChildEvidence(ctx context.Context, objects Objects, evidence ChildEvidence) (childObservation, error) {
	if evidence.SchemaVersion != 1 || evidence.EventLog.Kind != "child_event_log" {
		return childObservation{}, errors.New("unsupported child evidence")
	}
	raw, err := readImportedBytes(objects, evidence.EventLog)
	if err != nil {
		return childObservation{}, err
	}
	events, err := team.DecodeEventLog(bytes.NewReader(raw))
	if err != nil {
		return childObservation{}, err
	}
	session, err := validateChildLineage(events, evidence.Identity)
	if err != nil {
		return childObservation{}, err
	}
	roots, err := RegisterRoots(evidence.Roots)
	if err != nil {
		return childObservation{}, err
	}
	policy, err := ProjectPolicy(session.ExecutionPolicySnapshot, roots)
	if err != nil {
		return childObservation{}, err
	}
	if policy.OriginalConfigurationHash != evidence.Policy.OriginalConfigurationHash ||
		policy.ComparisonDigest != evidence.Policy.ComparisonDigest || !bytes.Equal(policy.Policy, evidence.Policy.Policy) {
		return childObservation{}, errors.New("imported child policy mismatch")
	}
	if session.RunResult.EvidenceManifest == nil {
		return childObservation{}, errors.New("imported child has no evidence manifest")
	}
	store := importedArtifactStore{objects: objects, artifacts: evidence.Artifacts}
	if err := session.RunResult.EvidenceManifest.Verify(ctx, store); err != nil {
		return childObservation{}, err
	}
	// Validate unselected/failed attempt transcripts as well as manifest
	// members; a successful later attempt cannot erase earlier evidence.
	for _, artifact := range evidence.Artifacts {
		if err := store.Verify(ctx, artifact.Original); err != nil {
			return childObservation{}, err
		}
	}
	for _, task := range session.Tasks {
		for _, receipt := range task.ExecutionReceipts {
			if receipt.RunID != evidence.Identity.RunID || receipt.TaskID != task.ID || receipt.Attempt < 1 || receipt.FinishedAt.IsZero() {
				return childObservation{}, errors.New("child receipt has incomplete or foreign identity")
			}
			artifact, err := store.lookup(receipt.TranscriptRef)
			if err != nil {
				return childObservation{}, err
			}
			if err := store.Verify(ctx, artifact.Original); err != nil {
				return childObservation{}, err
			}
		}
	}
	var decoded []cost.Event
	for _, event := range events {
		if !strings.HasPrefix(event.Type, "cost_") {
			continue
		}
		value, err := cost.DecodeEvent(event.Type, event.Payload)
		if err != nil {
			return childObservation{}, err
		}
		decoded = append(decoded, value)
	}
	projection, err := cost.Reduce(decoded)
	if err != nil {
		return childObservation{}, err
	}
	observation := childObservation{session: session, events: events, usageKnown: projection.Available && len(projection.Invocations) > 0}
	for _, invocation := range projection.Invocations {
		observation.calls++
		settlement := invocation.Settlement
		if invocation.Reservation.RunID != evidence.Identity.RunID {
			return childObservation{}, errors.New("foreign provider usage")
		}
		if settlement == nil || settlement.Usage == nil {
			observation.usageKnown = false
			continue
		}
		observation.providerFailed = observation.providerFailed || settlement.Outcome == cost.OutcomeProviderError
		usage := settlement.Usage
		if usage.TotalTokens < 0 || usage.TotalTokens > math.MaxInt64-observation.tokens {
			return childObservation{}, errors.New("child usage overflow")
		}
		observation.tokens += usage.TotalTokens
	}
	return observation, nil
}

func readImportedBytes(objects Objects, ref paired.ArtifactRef) ([]byte, error) {
	raw, err := objects.Read(ref)
	if err != nil {
		return nil, err
	}
	var decoded []byte
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	if len(decoded) > paired.MaxTotalBytes {
		return nil, errors.New("imported bytes exceed limit")
	}
	return decoded, nil
}

// importedArtifactStore verifies the existing manifest against an offline
// bundle. It does not publish artifacts or manufacture new occurrence metadata.
type importedArtifactStore struct {
	objects   Objects
	artifacts []ImportedArtifact
}

func (s importedArtifactStore) Put(context.Context, team.PutArtifactRequest) (team.ArtifactPutResult, error) {
	return team.ArtifactPutResult{}, errors.New("imported evidence is read-only")
}

func (s importedArtifactStore) lookup(id string) (ImportedArtifact, error) {
	var result ImportedArtifact
	count := 0
	for _, artifact := range s.artifacts {
		if artifact.Original.ID == id {
			result = artifact
			count++
		}
	}
	if count != 1 || result.Content.Kind != "child_artifact" {
		return ImportedArtifact{}, errors.New("missing or duplicated imported artifact")
	}
	return result, nil
}

func (s importedArtifactStore) Verify(_ context.Context, ref team.ArtifactRef) error {
	artifact, err := s.lookup(ref.ID)
	if err != nil {
		return err
	}
	raw, err := readImportedBytes(s.objects, artifact.Content)
	if err != nil {
		return err
	}
	if ref.SHA256 == "" || ref.SHA256 != artifact.Original.SHA256 || ref.SHA256 != paired.BytesDigest(raw) ||
		ref.ByteSize != artifact.Original.ByteSize || ref.Bytes != artifact.Original.Bytes ||
		ref.ByteSize != int64(len(raw)) || ref.Bytes != int64(len(raw)) {
		return errors.New("imported artifact content or size mismatch")
	}
	return nil
}

func (s importedArtifactStore) Open(ctx context.Context, id string) (io.ReadCloser, error) {
	artifact, err := s.lookup(id)
	if err != nil {
		return nil, err
	}
	if err := s.Verify(ctx, artifact.Original); err != nil {
		return nil, err
	}
	raw, err := readImportedBytes(s.objects, artifact.Content)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}

func (s importedArtifactStore) Resolve(ctx context.Context, ref team.ArtifactRef) (team.ArtifactRef, error) {
	if err := s.Verify(ctx, ref); err != nil {
		return team.ArtifactRef{}, err
	}
	return ref, nil
}

func (s importedArtifactStore) ListByTask(_ context.Context, id string) ([]team.ArtifactRef, error) {
	var refs []team.ArtifactRef
	for _, artifact := range s.artifacts {
		if artifact.Original.TaskID == id {
			refs = append(refs, artifact.Original)
		}
	}
	return refs, nil
}
