package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/tools"
)

// AcceptedReferenceSelection is an explicit operator choice, never a fuzzy
// match or permission to change the accepted finding that cites the source.
type AcceptedReferenceSelection struct {
	Pointer   string `json:"pointer"`
	Reference string `json:"reference"`
}

type AcceptedReferenceChoice struct {
	Key      string `json:"key"`
	Artifact string `json:"artifact_id,omitempty"`
	Path     string `json:"path,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

type acceptedReferenceSelectionsKey struct{}

func validAcceptedReferenceAssessment(a *ResultCollectionRecoveryAssessment) bool {
	proof := acceptedRepairProof(a.Checkpoint)
	return a.ValidatorRevision >= 12 && a.AdditionalTurns == 0 && proof != nil && len(a.ReferenceSelections) > 0 &&
		validCollectionDigest(a.ReferenceClaimsSHA256) && (a.ValidatorRevision >= 13 || len(proof.PathPointers) == 0)
}

func WithAcceptedReferenceSelections(ctx context.Context, selections []AcceptedReferenceSelection) context.Context {
	return context.WithValue(ctx, acceptedReferenceSelectionsKey{}, slices.Clone(selections))
}

func ParseAcceptedReferenceSelections(values []string) ([]AcceptedReferenceSelection, error) {
	var selections []AcceptedReferenceSelection
	seen := make(map[string]bool)
	for _, value := range values {
		pointer, reference, ok := strings.Cut(value, "=")
		if !ok || validateJSONPointer(pointer) != nil || pointer == "" || reference == "" || len(reference) > 128 || len(pointer) > maxTaskResultPointerBytes || seen[pointer] {
			return nil, fmt.Errorf("--repair-reference requires a unique /payload/pointer=authorized_reference choice")
		}
		seen[pointer] = true
		selections = append(selections, AcceptedReferenceSelection{Pointer: pointer, Reference: reference})
	}
	if len(selections) > 128 {
		return nil, fmt.Errorf("too many reference selections")
	}
	slices.SortFunc(selections, func(a, b AcceptedReferenceSelection) int { return strings.Compare(a.Pointer, b.Pointer) })
	return selections, nil
}

func selectedAcceptedReferences(ctx context.Context) []AcceptedReferenceSelection {
	values, _ := ctx.Value(acceptedReferenceSelectionsKey{}).([]AcceptedReferenceSelection)
	return slices.Clone(values)
}

// Build claims from the immutable original, not a rejected recopy. Only the
// explicitly selected leaf values may change; ordinary admission still checks
// the complete canonical result, source bindings and semantic freeze.
func (c *Coordinator) acceptedReferenceClaims(ctx context.Context, cp *ResultCollectionCheckpoint, compiled *CompiledResultContract, selections []AcceptedReferenceSelection) ([]byte, error) {
	proof := acceptedRepairProof(cp)
	if proof == nil || compiled == nil || len(selections) != len(proof.Pointers) {
		return nil, fmt.Errorf("select exactly the declared invalid accepted references")
	}
	refs, err := c.verifiedResultEvidenceKeyRefs(ctx, cp.TaskID, compiled)
	if err != nil {
		return nil, err
	}
	original := cp.ContractUpgrade.Original
	ref, kind := original.Prepared, "claims"
	if ref == nil {
		ref, kind = original.Response, "response"
	}
	raw, err := c.readCollectionBlob(ctx, original, kind, ref)
	if err != nil {
		return nil, err
	}
	raw, _ = collectionResponseJSON(raw)
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return nil, err
	}
	claims, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("accepted baseline claims are not an object")
	}
	payload := claims["structured_payload"]
	var paths worksetPathSelection
	if len(proof.PathPointers) > 0 {
		paths, err = c.verifiedWorksetPathSelection(ctx, cp.TaskID, compiled.worksetPaths)
		if err != nil {
			return nil, err
		}
	}
	seen := make(map[string]bool)
	for _, selection := range selections {
		if seen[selection.Pointer] || !slices.Contains(proof.Pointers, selection.Pointer) {
			return nil, fmt.Errorf("reference pointer %q is outside the declared repair", selection.Pointer)
		}
		seen[selection.Pointer] = true
		if slices.Contains(proof.PathPointers, selection.Pointer) {
			if err := replaceAcceptedWorksetPath(payload, selection, paths); err != nil {
				return nil, err
			}
			continue
		}
		chosen, found := refs[selection.Reference]
		if !found {
			for _, candidate := range refs {
				if candidate.ID == selection.Reference {
					chosen, found = candidate, true
					break
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("reference %q was not observed by the original work", selection.Reference)
		}
		if err := replaceAcceptedReference(payload, fixedCollectionPointerTokens(selection.Pointer), chosen.ID); err != nil {
			return nil, err
		}
	}
	// These values have an independent runtime owner in the current schema.
	// The baseline is trusted data, but rebind them through that same owner.
	if runtimeOwnsFilesRead(compiled) {
		delete(claims, "files_read")
	}
	for _, spec := range compiled.runtimeInputs {
		omitAcceptedRuntimeValue(payload, fixedCollectionPointerTokens(spec.Target))
	}
	if spec := compiled.evidenceInputs; spec.runtimeOwned() {
		omitAcceptedRuntimeValue(payload, fixedCollectionPointerTokens(spec.ItemsPointer))
	}
	encoded, err := json.Marshal(claims, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	resolved, err := c.resolveResultEvidenceKeys(ctx, cp.TaskID, string(encoded))
	return []byte(resolved), err
}

func replaceAcceptedReference(value any, tokens []string, reference string) error {
	if len(tokens) == 0 {
		return fmt.Errorf("empty accepted reference pointer")
	}
	var child any
	switch object := value.(type) {
	case map[string]any:
		child = object[tokens[0]]
		if len(tokens) == 1 {
			if _, ok := child.(string); !ok {
				return fmt.Errorf("accepted reference must replace an existing string")
			}
			object[tokens[0]] = reference
			return nil
		}
	case []any:
		i, err := strconv.Atoi(tokens[0])
		if err != nil || i < 0 || i >= len(object) {
			return fmt.Errorf("accepted reference index is invalid")
		}
		child = object[i]
		if len(tokens) == 1 {
			if _, ok := child.(string); !ok {
				return fmt.Errorf("accepted reference must replace an existing string")
			}
			object[i] = reference
			return nil
		}
	default:
		return fmt.Errorf("accepted reference path is absent")
	}
	return replaceAcceptedReference(child, tokens[1:], reference)
}

func omitAcceptedRuntimeValue(value any, tokens []string) {
	if len(tokens) == 0 {
		return
	}
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	if len(tokens) == 1 {
		delete(object, tokens[0])
		return
	}
	omitAcceptedRuntimeValue(object[tokens[0]], tokens[1:])
}

func acceptedReferenceClaimsHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Local projection evidence is committed with the operator grant before a
// lease opens. No provider, model reservation or worker execution is involved.
type AcceptedReferenceProjection struct {
	InvocationID string              `json:"invocation_id"`
	Request      ResultCollectionRef `json:"request"`
	Response     ResultCollectionRef `json:"response"`
	Prepared     ResultCollectionRef `json:"prepared"`
}

func (c *Coordinator) prepareAcceptedReferenceProjection(ctx context.Context, a *ResultCollectionRecoveryAssessment) (*AcceptedReferenceProjection, error) {
	cp := cloneResultCollectionCheckpoint(a.Checkpoint)
	var err error
	ctx, err = c.acceptedReferenceContext(ctx, cp)
	if err != nil {
		return nil, err
	}
	_, compiled, err := c.boundResultContract(cp.TaskID)
	if err != nil {
		return nil, err
	}
	raw, err := c.acceptedReferenceClaims(ctx, cp, compiled, a.ReferenceSelections)
	if err != nil {
		return nil, fmt.Errorf("selected reference claims: %w", err)
	}
	if acceptedReferenceClaimsHash(raw) != a.ReferenceClaimsSHA256 {
		return nil, fmt.Errorf("selected reference claims changed: %w", errResultCollectionStale)
	}
	cp.InvocationID, cp.InvocationRunID = "operator-reference-"+newTaskDispatchID(), c.executionRunID
	request, err := acceptedReferenceRequest(a)
	if err != nil {
		return nil, err
	}
	refs := make([]*ResultCollectionRef, 0, 3)
	for _, blob := range []struct {
		kind string
		data []byte
	}{{"request", request}, {"response", raw}, {"claims", raw}} {
		ref, err := c.putCollectionBlob(ctx, cp, blob.kind, blob.data)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return &AcceptedReferenceProjection{InvocationID: cp.InvocationID, Request: *refs[0], Response: *refs[1], Prepared: *refs[2]}, nil
}

func (c *Coordinator) assessAcceptedReferenceSelection(ctx context.Context, a *ResultCollectionRecoveryAssessment, work *resultCollectionWork, compiled *CompiledResultContract, check func(string, error) bool) {
	refs, err := c.verifiedResultEvidenceKeyRefs(ctx, a.TaskID, compiled)
	if !check("evidence_keys", err) {
		return
	}
	for key, ref := range refs {
		a.ReferenceChoices = append(a.ReferenceChoices, AcceptedReferenceChoice{Key: key, Artifact: ref.ID, Path: ref.Path})
	}
	if proof := acceptedRepairProof(a.Checkpoint); proof != nil && len(proof.PathPointers) > 0 {
		paths, err := c.verifiedWorksetPathSelection(ctx, a.TaskID, compiled.worksetPaths)
		if !check("workset_paths", err) {
			return
		}
		for key, path := range paths.paths {
			a.ReferenceChoices = append(a.ReferenceChoices, AcceptedReferenceChoice{Key: key, Path: path, Kind: "workset_path"})
		}
	}
	slices.SortFunc(a.ReferenceChoices, func(x, y AcceptedReferenceChoice) int { return strings.Compare(x.Key, y.Key) })
	a.ReferenceSelections = selectedAcceptedReferences(ctx)
	if len(a.ReferenceSelections) == 0 {
		check("reference_selection", fmt.Errorf("accepted-result repair requires --repair-reference for each declared invalid pointer; choose an exact observed reference from reference_choices"))
		return
	}
	raw, err := c.acceptedReferenceClaims(ctx, a.Checkpoint, compiled, a.ReferenceSelections)
	if !check("reference_selection", err) {
		return
	}
	if !c.validateRecoveryIndependentClaims(ctx, a.Checkpoint, work, compiled, string(raw), check) {
		return
	}
	if !check("canonical_claims", c.validateRecoveryCanonicalClaims(ctx, a.Checkpoint, raw, nil, nil, nil)) {
		return
	}
	a.ReferenceClaimsSHA256 = acceptedReferenceClaimsHash(raw)
	a.Disposition, a.AdditionalTurns = RecoverySelectReferences, 0
}

// Reconstruct verification context without opening or changing an occurrence lease.
func (c *Coordinator) acceptedReferenceContext(ctx context.Context, cp *ResultCollectionCheckpoint) (context.Context, error) {
	item := c.todoItemByID(cp.TaskID)
	work, err := c.loadCollectionWork(ctx, item, cp)
	if err != nil {
		return ctx, err
	}
	evidence, err := c.restoreTranscriptEvidence(ctx, &work.Receipt, cp.SourceRunID)
	if err != nil {
		return ctx, err
	}
	ctx = context.WithValue(ctx, resultCollectionEvidenceKey{}, evidence)
	ctx = context.WithValue(ctx, tools.AgentNameKey, cp.Agent)
	ctx = context.WithValue(ctx, todoIDKey{}, cp.TaskID)
	ctx = context.WithValue(ctx, executionAttemptKey{}, cp.Attempt)
	ctx = withSubmitResultRuntimeIdentity(ctx, submitResultRuntimeIdentity{RunID: cp.SourceRunID, TaskID: cp.TaskID, Attempt: cp.Attempt, Agent: cp.Agent, OccurrenceRevision: cp.SubmissionRevision, DispatchID: cp.SubmissionDispatchID})
	ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: cp.SourceRunID, TaskID: cp.TaskID, Attempt: cp.Attempt, AgentName: cp.Agent, AgentRole: "worker", ModelExecutionID: cp.ModelExecutionID})
	manifest, err := collectionContextManifest(item, cp, &work.Receipt)
	if err != nil {
		return ctx, err
	}
	if manifest != nil {
		ctx = withInvocationMetadata(ctx, invocationMetadataFromManifest(manifest))
	}
	return c.withCollectedWorkArtifactScope(ctx, work), nil
}

func validateAcceptedReferenceProjection(grant *ResultRepairGrant, cp *ResultCollectionCheckpoint) error {
	p := grant.ReferenceProjection
	if p == nil || !strings.HasPrefix(p.InvocationID, "operator-reference-") || cp.State != ResultCollectionPrepared || cp.FailureReason != "" || cp.InvocationID != p.InvocationID || cp.InvocationRunID != grant.RunID || cp.Request == nil || *cp.Request != p.Request || cp.Response == nil || *cp.Response != p.Response || cp.Prepared == nil || *cp.Prepared != p.Prepared || cp.ObservedUsage != nil {
		return errResultCollectionStale
	}
	return nil
}

func acceptedReferenceRequest(a *ResultCollectionRecoveryAssessment) ([]byte, error) {
	return json.Marshal(struct {
		Kind       string                       `json:"kind"`
		Assessment string                       `json:"assessment_sha256"`
		Selections []AcceptedReferenceSelection `json:"selections"`
	}{"accepted_reference_selection", a.Fingerprint, a.ReferenceSelections}, json.Deterministic(true))
}

func (c *Coordinator) validateAcceptedReferenceProjectionBlobs(ctx context.Context, grant *ResultRepairGrant) error {
	a, p := grant.Recovery, grant.ReferenceProjection
	ctx, err := c.acceptedReferenceContext(ctx, a.Checkpoint)
	if err != nil {
		return err
	}
	_, compiled, err := c.boundResultContract(a.TaskID)
	if err != nil {
		return err
	}
	expected, err := c.acceptedReferenceClaims(ctx, a.Checkpoint, compiled, a.ReferenceSelections)
	if err != nil {
		return fmt.Errorf("selected reference claims: %w", err)
	}
	if acceptedReferenceClaimsHash(expected) != a.ReferenceClaimsSHA256 {
		return errResultCollectionStale
	}
	cp := cloneResultCollectionCheckpoint(a.Checkpoint)
	cp.InvocationID, cp.InvocationRunID = p.InvocationID, grant.RunID
	request, err := acceptedReferenceRequest(a)
	if err != nil {
		return err
	}
	for _, blob := range []struct {
		kind string
		ref  *ResultCollectionRef
		data []byte
	}{{"request", &p.Request, request}, {"response", &p.Response, expected}, {"claims", &p.Prepared, expected}} {
		raw, err := c.readCollectionBlob(ctx, cp, blob.kind, blob.ref)
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, blob.data) {
			return errResultCollectionStale
		}
	}
	return nil
}

func acceptedReferenceProjectionFor(item *TodoItem, cp *ResultCollectionCheckpoint) *AcceptedReferenceProjection {
	for _, grant := range collectionRepairGrants(collectionReceipt(item, cp)) {
		if p := grant.ReferenceProjection; p != nil && p.InvocationID == cp.InvocationID && grant.RunID == cp.InvocationRunID && grant.Recovery != nil && grant.Recovery.Disposition == RecoverySelectReferences {
			return p
		}
	}
	return nil
}

type referenceProjectionPreviewKey struct{}

func (c *Coordinator) commitAcceptedReferenceSelection(ctx context.Context, checkpoint *ResultCollectionCheckpoint) error {
	if checkpoint.State != ResultCollectionPrepared {
		return c.collectionFailure(ctx, checkpoint, RepairFailureSource, errResultCollectionStale)
	}
	if err := c.commitPreparedCollection(ctx, checkpoint); err != nil {
		return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(err), err)
	}
	return nil
}
