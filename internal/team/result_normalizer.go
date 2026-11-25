package team

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	resultNormalizerTimeout           = 30 * time.Second
	resultNormalizerMaxBytes          = 256 << 10
	resultNormalizationRecordMaxBytes = 3 * resultNormalizerMaxBytes
)

// Only the bounded, data-only Go runner is eligible. The ordinary provider
// Execute method carries workspace and process authority and is never used.
type resultNormalizerProvider interface {
	normalizeResult(context.Context, []byte) ([]byte, error)
}

func (p *golangActionProvider) normalizeResult(ctx context.Context, raw []byte) ([]byte, error) {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	result, err := golangruntime.ExecuteNormalizer(ctx, "", p.program, raw, resultNormalizerMaxBytes)
	if err != nil {
		return nil, errors.Join(err, errors.New(utils.RedactSecrets(string(result.Stderr))))
	}
	if result.StdoutTruncated {
		return nil, fmt.Errorf("result normalizer output exceeds %d bytes", resultNormalizerMaxBytes)
	}
	return result.Stdout, nil
}

func resultNormalizerBinding(session *TeamSession, capability string) (resultNormalizerProvider, string, error) {
	if !golangruntime.NormalizerSupported() {
		return nil, "", errors.New("result normalizers require the bounded Linux Go action runtime")
	}
	if session == nil || session.ProviderRegistry == nil {
		return nil, "", errors.New("result normalizer provider registry is missing")
	}
	registered, _ := session.ProviderRegistry.Get(capability)
	provider, ok := registered.(resultNormalizerProvider)
	digest := actionProviderIdentityHash(session, capability)
	if !ok || digest == "" {
		return nil, "", fmt.Errorf("result normalizer %q must be a registered trusted-static Go action", capability)
	}
	return provider, digest, nil
}

// ResultNormalizationEvidence identifies sealed normalization bytes even when
// committing the corresponding typed result was interrupted.
type ResultNormalizationEvidence struct {
	OccurrenceSHA256 string `json:"occurrence_sha256"`
	RawSHA256        string `json:"raw_sha256"`
	OutputSHA256     string `json:"output_sha256"`
	ProviderSHA256   string `json:"provider_sha256"`
	SubmissionSHA256 string `json:"submission_sha256,omitempty"`
}

func (s *resultNormalization) evidence() ResultNormalizationEvidence {
	if s == nil {
		return ResultNormalizationEvidence{}
	}
	return ResultNormalizationEvidence{
		OccurrenceSHA256: s.OccurrenceSHA256, RawSHA256: sha256HexBytes(s.Raw),
		OutputSHA256: s.OutputSHA256, ProviderSHA256: s.ProviderSHA256,
	}
}

// resultNormalization is sealed with the collection claims or local result.
// Byte slices preserve the submitted and emitted bytes across JSON projection.
type resultNormalization struct {
	OccurrenceSHA256 string `json:"occurrence_sha256"`
	ProviderSHA256   string `json:"provider_sha256"`
	OutputSHA256     string `json:"output_sha256"`
	Raw              []byte `json:"raw"`
	Output           []byte `json:"output"`

	pendingProjection bool
}

func (s *resultNormalization) clone() *resultNormalization {
	if s == nil {
		return nil
	}
	clone := *s
	clone.Raw, clone.Output = slices.Clone(s.Raw), slices.Clone(s.Output)
	return &clone
}

func validNormalization(ref ResultContractRef, s *resultNormalization) bool {
	return s != nil && ref.Normalizer != "" && ref.NormalizerSHA256 != "" &&
		ref.NormalizerSHA256 == s.ProviderSHA256 && validCollectionDigest(s.OccurrenceSHA256) && len(s.Raw) > 0 && len(s.Raw) <= resultNormalizerMaxBytes &&
		len(s.Output) > 0 && len(s.Output) <= resultNormalizerMaxBytes && s.OutputSHA256 == sha256HexBytes(s.Output)
}

func validNormalizedResultPayload(payload *ResultPayload) bool {
	if payload.Contract.Normalizer == "" {
		return payload.RawSHA256 == "" && payload.Normalization == nil
	}
	return validNormalization(payload.Contract, payload.Normalization) &&
		payload.RawSHA256 == sha256HexBytes(payload.Normalization.Raw) &&
		payload.SHA256 == sha256HexBytes(payload.Value)
}

func legacyNormalizedContract(ref ResultContractRef) bool {
	return ref.Normalizer != "" && ref.NormalizerSHA256 == ""
}

func receiptMatchesResultPayload(receipt *ExecutionReceipt, payload *ResultPayload) bool {
	if receipt.ResultPayloadSHA256 != payload.SHA256 || receipt.ResultRawPayloadSHA256 != payload.RawSHA256 {
		return false
	}
	// The prior runtime stored only the raw and canonical payload digests.
	// Honor an already validated receipt with exactly that historical shape.
	if legacyNormalizedContract(payload.Contract) {
		return payload.Normalization == nil && validCollectionDigest(payload.RawSHA256) && payload.SHA256 == sha256HexBytes(payload.Value) &&
			receipt.ResultValidation == ResultValidationValid && receipt.ResultContractID == payload.Contract.ID &&
			receipt.ResultContractSHA256 == payload.Contract.SchemaSHA256 &&
			receipt.ResultWorkDispatchID == "" && receipt.ResultWorkOccurrenceRevision == 0 &&
			receipt.ResultNormalizerOutputSHA256 == "" && receipt.ResultNormalizerOccurrenceSHA256 == ""
	}
	if !validNormalizedResultPayload(payload) {
		return false
	}
	if payload.Normalization == nil {
		return receipt.ResultNormalizerOutputSHA256 == "" && receipt.ResultNormalizerOccurrenceSHA256 == ""
	}
	return receipt.ResultNormalizerOutputSHA256 == payload.Normalization.OutputSHA256 &&
		receipt.ResultNormalizerOccurrenceSHA256 == payload.Normalization.OccurrenceSHA256
}

func sha256HexBytes(data []byte) string {
	return strings.TrimPrefix(runInputHash(data), "sha256:")
}

func draftResultPayloadSchema(ref ResultContractRef) map[string]any {
	return map[string]any{"type": "object", "description": fmt.Sprintf("Submit a JSON draft for result contract %s. The registered normalizer runs before final schema validation. Omit runtime-owned fields.", ref.ID)}
}

func validatedNormalizerDraft(raw []byte, compiled *CompiledResultContract) error {
	if len(raw) > resultNormalizerMaxBytes {
		return fmt.Errorf("result normalizer payload exceeds %d bytes", resultNormalizerMaxBytes)
	}
	value, err := decodeUniqueJSON(raw)
	if err != nil {
		return err
	}
	if _, ok := value.(map[string]any); !ok {
		return errors.New("result normalizer payload must be one JSON object")
	}
	pointers := make([]string, 0, len(compiled.runtimeInputs)+2)
	for _, binding := range compiled.runtimeInputs {
		pointers = append(pointers, binding.Target)
	}
	if evidence := compiled.evidenceInputs; evidence.runtimeOwned() {
		pointers = append(pointers, evidence.ItemsPointer)
		if evidence.RuntimeComplete {
			pointers = append(pointers, evidence.CompletePointer)
		}
	}
	for _, pointer := range pointers {
		if _, err := resolveJSONPointer(value, pointer); err == nil {
			return fmt.Errorf("%s is runtime-owned; omit it entirely", pointer)
		}
	}
	for _, collection := range compiled.fixedCollections {
		if err := collection.validateDraftOwnership(value); err != nil {
			return err
		}
	}
	return rejectModelDeliveryOverride(value, compiled.toolEvidence)
}

func modelNormalizerDraft(input SubmitResultInput) []byte {
	if input.normalizerDraft != nil {
		return input.normalizerDraft
	}
	return input.StructuredPayload
}

func submittedNormalizerDraft(raw []byte, legacyFields bool) (json.RawMessage, error) {
	if _, err := decodeUniqueJSON(raw); err != nil {
		return nil, err
	}
	var envelope struct {
		Draft json.RawMessage `json:"structured_payload"`
	}
	var err error
	if legacyFields {
		err = json.Unmarshal(raw, &envelope)
	} else {
		err = jsonv2.Unmarshal(raw, &envelope)
	}
	return envelope.Draft, err
}

func hasNormalizerDraft(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) != 0 && !bytes.Equal(raw, []byte("null"))
}

func normalizerBytesPersistable(data []byte) bool {
	if string(data) != utils.RedactSecrets(string(data)) {
		return false
	}
	value, err := decodeResultContractJSON(data)
	if err != nil {
		return false
	}
	canonical, err := json.Marshal(value)
	return err == nil && redactionStableJSON(canonical)
}

// Draft fields can use a representation that only the normalizer understands.
// Apply evidence keys to the surrounding submission and restore the draft.
func (c *Coordinator) resolveSubmissionEvidenceKeys(ctx context.Context, taskID, wire string, draft json.RawMessage) (string, error) {
	if draft == nil {
		return c.resolveResultEvidenceKeys(ctx, taskID, wire)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal([]byte(wire), &envelope); err != nil {
		return "", err
	}
	for name := range envelope {
		if strings.EqualFold(name, "structured_payload") {
			delete(envelope, name)
		}
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	resolved, err := c.resolveResultEvidenceKeys(ctx, taskID, string(body))
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal([]byte(resolved), &envelope); err != nil {
		return "", err
	}
	envelope["structured_payload"] = draft
	body, err = json.Marshal(envelope)
	return string(body), err
}

func normalizationOccurrence(identity submitResultRuntimeIdentity, cp *ResultCollectionCheckpoint) string {
	if cp != nil {
		identity = submitResultRuntimeIdentity{RunID: cp.SourceRunID, TaskID: cp.TaskID, Attempt: cp.Attempt,
			Agent: cp.Agent, OccurrenceRevision: cp.WorkOccurrenceRevision, DispatchID: cp.WorkDispatchID}
	}
	body, _ := json.Marshal(identity)
	return sha256HexBytes(body)
}

func normalizationDocument(ref ResultContractRef, occurrence, rawSHA256 string) string {
	body, _ := json.Marshal(ref)
	return "result-normalization/" + occurrence + "/" + sha256HexBytes(body) + "/" + rawSHA256
}

func (c *Coordinator) readStagedNormalization(ref ResultContractRef, occurrence, rawSHA256 string) (*resultNormalization, bool, error) {
	return readStagedNormalizationAt(workspaceDocs(c.session.Workspace), c.artifactStoreRootPath(), ref, occurrence, rawSHA256)
}

func readStagedNormalizationAt(docs runtimeDocs, root string, ref ResultContractRef, occurrence, rawSHA256 string) (*resultNormalization, bool, error) {
	key := normalizationDocument(ref, occurrence, rawSHA256)
	body, found, err := docs.getDocument(key)
	if err != nil {
		return nil, found, err
	}
	binding, err := readRuntimeEffectRecord(root, key)
	started := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, found, err
	}
	if started && binding != sha256HexBytes([]byte(key)) {
		return nil, true, errors.New("normalizer start does not match the original submission")
	}
	if !found || bytes.Equal(body, []byte("null")) {
		body = nil
	}
	sealed, published, err := readRuntimeEffectCompletion(root, key+"/output", resultNormalizationRecordMaxBytes)
	if err != nil {
		return nil, found || started, err
	}
	if err := validateRuntimeEffectProjection(sealed, body, published); err != nil {
		return nil, true, err
	}
	if sealed == nil {
		return nil, found || started, nil
	}
	if !started {
		return nil, true, errors.New("staged normalization lacks its original start")
	}
	var stage resultNormalization
	if err := jsonv2.Unmarshal(sealed, &stage, jsonv2.RejectUnknownMembers(true)); err != nil {
		return nil, true, err
	}
	if !validNormalization(ref, &stage) || stage.OccurrenceSHA256 != occurrence || sha256HexBytes(stage.Raw) != rawSHA256 {
		return nil, true, errors.New("staged normalization evidence is inconsistent")
	}
	stage.pendingProjection = !published
	return &stage, true, nil
}

func (c *Coordinator) publishStagedNormalization(ctx context.Context, ref ResultContractRef, stage *resultNormalization) error {
	if !stage.pendingProjection {
		return nil
	}
	key := normalizationDocument(ref, stage.OccurrenceSHA256, sha256HexBytes(stage.Raw))
	body, err := json.Marshal(stage)
	if err != nil {
		return rejectLocalResultPersistence(err)
	}
	projected, _, err := workspaceDocs(c.session.Workspace).getDocument(key)
	if err != nil {
		return rejectNormalizationRecoveryRead(err)
	}
	if !bytes.Equal(projected, body) {
		if projected != nil && !bytes.Equal(projected, []byte("null")) {
			return rejectResultSourceRead(errors.New("normalizer projection differs from its durable output"))
		}
		written, err := c.writeNormalizationStage(context.WithoutCancel(ctx), key, projected, body)
		if err != nil {
			return rejectLocalResultPersistence(err)
		}
		if !written {
			return rejectLocalResultPersistence(errors.New("normalizer reservation changed before output publication"))
		}
	}
	if err := publishRuntimeEffectProjection(c.artifactStoreRootPath(), key+"/output", sha256HexBytes(body)); err != nil {
		return err
	}
	stage.pendingProjection = false
	return nil
}

func (c *Coordinator) writeNormalizationStage(ctx context.Context, key string, expected, body []byte) (bool, error) {
	var written bool
	err := withRuntimeStore(c.session.Workspace, func(store *runtimestore.Store) error {
		return store.WithTx(ctx, func(tx *sql.Tx) error {
			current, found, err := runtimestore.GetDocument(ctx, tx, key)
			if err != nil {
				return err
			}
			if found != (expected != nil) || !bytes.Equal(current, expected) {
				return nil
			}
			if checkpoint, ok := ctx.Value(directNormalizationCheckpointKey{}).(*directNormalizationCheckpoint); ok && expected == nil {
				if err := checkpoint.store(ctx, tx); err != nil {
					return err
				}
			}
			written = true
			return runtimestore.PutDocument(ctx, tx, key, body)
		})
	})
	return written && err == nil, err
}

func (c *Coordinator) validateNormalizationSources(ctx context.Context, taskID string, compiled *CompiledResultContract) error {
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, taskID)
	if err != nil {
		return err
	}
	active := c.occurrenceController(taskID).snapshot()
	if !active.opened || active.reserved || active.result != nil || !sameTaskResultOccurrence(active.identity, identity) {
		return errTaskResultStale
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return c.validateIndependentSubmissionSources(ctx, taskID, compiled, resultSourceOmissions{})
}

func (c *Coordinator) stageResultNormalization(ctx context.Context, taskID string, raw []byte, cp *ResultCollectionCheckpoint) (*resultNormalization, error) {
	ref, compiled, err := c.boundResultContract(taskID)
	if err != nil {
		return nil, rejectLocalResult(RepairFailureContract, err)
	}
	if ref == nil || ref.Normalizer == "" || !hasNormalizerDraft(raw) {
		return nil, nil
	}
	// Lease activation, lifecycle publication and revocation share this fence.
	// A bounded action owns its lease through reservation, execution and seal.
	controller := c.occurrenceController(taskID)
	controller.projectionMu.Lock()
	defer controller.projectionMu.Unlock()
	if err := c.validateNormalizationSources(ctx, taskID, compiled); err != nil {
		return nil, rejectResultSourceRead(err)
	}
	if err := validatedNormalizerDraft(raw, compiled); err != nil {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, err)
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, taskID)
	if err != nil {
		return nil, rejectLocalResult(RepairFailureSource, err)
	}
	provider, digest, err := resultNormalizerBinding(c.session, ref.Normalizer)
	if err != nil {
		return nil, rejectLocalResult(RepairFailureContract, err)
	}
	if digest != ref.NormalizerSHA256 {
		return nil, rejectLocalResult(RepairFailureContract, errors.New("result normalizer provider changed after admission"))
	}
	occurrence := normalizationOccurrence(identity, cp)
	rawSHA256 := sha256HexBytes(raw)
	stage, found, err := c.readStagedNormalization(*ref, occurrence, rawSHA256)
	if err != nil {
		return nil, rejectResultSourceRead(err)
	}
	if stage != nil {
		if checkpoint, ok := ctx.Value(directNormalizationCheckpointKey{}).(*directNormalizationCheckpoint); ok {
			if err := withRuntimeStore(c.session.Workspace, func(store *runtimestore.Store) error {
				return store.WithTx(ctx, func(tx *sql.Tx) error { return checkpoint.store(ctx, tx) })
			}); err != nil {
				return nil, rejectLocalResult(RepairFailureCommit, err)
			}
			if err := checkpoint.recordDispatchStart(c.artifactStoreRootPath()); err != nil {
				return nil, err
			}
		}
		return stage, c.publishStagedNormalization(ctx, *ref, stage)
	}
	if found {
		return nil, rejectLocalResult(RepairFailureAction, errors.New("normalizer invocation has no durable output"))
	}
	if !normalizerBytesPersistable(raw) {
		return nil, rejectLocalResult(RepairFailureInvalidSchema, errors.New("omit credentials, tokens, and keys from the normalization draft"))
	}
	// Record ownership before execution. An interrupted reservation cannot
	// silently become a second execution. A successful output can be reused
	// even if the subsequent claims or result commit fails.
	key := normalizationDocument(*ref, occurrence, rawSHA256)
	pending := []byte("null")
	reserved, err := c.writeNormalizationStage(ctx, key, nil, pending)
	if err != nil {
		return nil, rejectLocalResult(RepairFailureCommit, err)
	}
	if !reserved {
		return nil, rejectLocalResult(RepairFailureCommit, errors.New("normalizer reservation changed during submission"))
	}
	root := c.artifactStoreRootPath()
	if checkpoint, ok := ctx.Value(directNormalizationCheckpointKey{}).(*directNormalizationCheckpoint); ok {
		if err := checkpoint.recordDispatchStart(root); err != nil {
			return nil, err
		}
	}
	if err := createRuntimeEffectRecord(root, key, sha256HexBytes([]byte(key))); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, rejectLocalResult(RepairFailureAction, errors.New("normalizer invocation has no durable output"))
		}
		return nil, rejectLocalResultPersistence(fmt.Errorf("persist normalizer start: %w", err))
	}
	actionCtx, cancel := context.WithTimeout(ctx, resultNormalizerTimeout)
	defer cancel()
	output, err := provider.normalizeResult(actionCtx, raw)
	if err == nil {
		err = actionCtx.Err()
	}
	if err != nil {
		return nil, rejectLocalResult(RepairFailureAction, ActionProviderError{Capability: ref.Normalizer, Cause: err})
	}
	if err := validatedNormalizerDraft(output, compiled); err != nil {
		return nil, rejectLocalResult(RepairFailureAction, ActionProviderError{Capability: ref.Normalizer, Cause: err})
	}
	if !normalizerBytesPersistable(output) {
		return nil, rejectLocalResult(RepairFailureAction, ActionProviderError{Capability: ref.Normalizer, Cause: errors.New("normalizer output contains secret-like data")})
	}
	stage = &resultNormalization{OccurrenceSHA256: occurrence, ProviderSHA256: digest, OutputSHA256: sha256HexBytes(output), Raw: raw, Output: output, pendingProjection: true}
	body, err := json.Marshal(stage)
	if err == nil {
		err = createRuntimeEffectBody(root, key+"/output", body)
	}
	if err != nil {
		return nil, rejectLocalResultPersistence(fmt.Errorf("persist normalizer output: %w", err))
	}
	return stage, c.publishStagedNormalization(ctx, *ref, stage)
}

func (c *Coordinator) bindStagedNormalization(ctx context.Context, taskID string, input SubmitResultInput) (SubmitResultInput, error) {
	ref, compiled, err := c.boundResultContract(taskID)
	if err != nil {
		return input, rejectLocalResult(RepairFailureContract, err)
	}
	if input.normalizationBound || ref == nil || ref.Normalizer == "" || !hasNormalizerDraft(modelNormalizerDraft(input)) {
		return input, nil
	}
	stage := input.resultNormalization
	cp, _ := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint)
	identity, _ := submitResultRuntimeIdentityFromContext(ctx, c, taskID)
	occurrence := normalizationOccurrence(identity, cp)
	if recovery, ok := ctx.Value(directNormalizationRecoveryKey{}).(*directNormalizationRecovery); ok && cp == nil {
		occurrence = recovery.stage.OccurrenceSHA256
	}
	if err := validateNormalizationStage(*ref, compiled, occurrence, modelNormalizerDraft(input), stage); err != nil {
		return input, rejectLocalResult(RepairFailureSource, err)
	}
	// Only the normalized output proceeds to runtime-owned evidence binding.
	wire := append([]byte(`{"structured_payload":`), stage.Output...)
	wire = append(wire, '}')
	resolved, resolveErr := c.resolveResultEvidenceKeys(ctx, taskID, string(wire))
	if resolveErr != nil && !isResultEvidenceFormatError(resolveErr) {
		return input, rejectResultSourceRead(resolveErr)
	}
	if resolved == "" {
		resolved = string(wire)
	}
	var claims struct {
		StructuredPayload json.RawMessage `json:"structured_payload"`
	}
	if err := json.Unmarshal([]byte(resolved), &claims); err != nil {
		return input, rejectLocalResult(RepairFailureSource, err)
	}
	if resolveErr != nil {
		probe := input
		probe.StructuredPayload = claims.StructuredPayload
		if err := c.validatePayloadSourceInput(ctx, taskID, compiled, resolved, probe, cp != nil); err != nil {
			return input, err
		}
		return input, rejectLocalResult(RepairFailureInvalidSchema, resolveErr)
	}
	input.StructuredPayload = claims.StructuredPayload
	input.normalizationBound = true
	return input, nil
}

func validateNormalizationStage(ref ResultContractRef, compiled *CompiledResultContract, occurrence string, draft []byte, stage *resultNormalization) error {
	if !validNormalization(ref, stage) || stage.OccurrenceSHA256 != occurrence || !bytes.Equal(stage.Raw, draft) {
		return errors.New("result normalization lacks occurrence-bound staged evidence")
	}
	if !normalizerBytesPersistable(stage.Raw) || !normalizerBytesPersistable(stage.Output) {
		return errors.New("staged normalization contains data that cannot be persisted")
	}
	if err := validatedNormalizerDraft(stage.Raw, compiled); err != nil {
		return err
	}
	return validatedNormalizerDraft(stage.Output, compiled)
}

// applyResultContractReceipt records the attempt's result contract outcome
// on its receipt.
func (c *Coordinator) applyResultContractReceipt(todoID string, result *TaskResult, receipt *ExecutionReceipt) {
	if receipt == nil {
		return
	}
	// Accumulate: a result-only repair re-applies the outcome to the same
	// attempt's receipt after the worker turn already recorded its own.
	receipt.ResultValidationFailures += c.takeResultValidationFailures(todoID)
	var ref *ResultContractRef
	if item := c.todoItemByID(todoID); item != nil {
		ref = item.ResultContract
	}
	if ref == nil {
		return
	}
	receipt.ResultContractID = ref.ID
	receipt.ResultContractSHA256 = ref.SchemaSHA256
	receipt.bindInterruptedRepairNormalization(ref)
	receipt.bindResultContractPayload(result)
}

func (receipt *ExecutionReceipt) bindResultContractPayload(result *TaskResult) {
	switch {
	case result != nil && result.StructuredPayload != nil:
		receipt.ResultPayloadSHA256 = result.StructuredPayload.SHA256
		receipt.ResultRawPayloadSHA256 = result.StructuredPayload.RawSHA256
		if stage := result.StructuredPayload.Normalization; stage != nil {
			receipt.ResultNormalizerOutputSHA256 = stage.OutputSHA256
			receipt.ResultNormalizerOccurrenceSHA256 = stage.OccurrenceSHA256
		}
		receipt.ResultValidation = ResultValidationValid
	case receipt.ResultValidationFailures > 0:
		receipt.ResultValidation = ResultValidationInvalid
	default:
		receipt.ResultValidation = ResultValidationAbsent
	}
}

// formatForContext renders a validated payload for the coordinator and
// downstream task context, bounded to keep context budgets predictable.
func (p *ResultPayload) formatForContext() string {
	if p == nil {
		return ""
	}
	header := fmt.Sprintf("Structured Payload (contract %s, sha256 %s):\n", p.Contract.ID, p.SHA256)
	if p.RawSHA256 != "" {
		header = fmt.Sprintf("Structured Payload (contract %s, sha256 %s, raw_sha256 %s):\n", p.Contract.ID, p.SHA256, p.RawSHA256)
	}
	if len(p.Value) <= resultPayloadContextMaxBytes {
		return header + string(p.Value) + "\n"
	}
	return header + utils.TruncateString(string(p.Value), resultPayloadContextMaxBytes) +
		fmt.Sprintf("\n[structured payload truncated at %d bytes; the complete value is stored in the durable typed result]\n", resultPayloadContextMaxBytes)
}

// accumulateResultContractMetrics projects rejected structured payloads from
// the run's receipts.
func accumulateResultContractMetrics(metrics *RunMetrics, receipt ExecutionReceipt, runID string) {
	if metrics == nil || (runID != "" && receipt.RunID != runID) {
		return
	}
	metrics.StructuredResultValidationFailures += receipt.ResultValidationFailures
}
