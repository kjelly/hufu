package team

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/tools"
)

const (
	EventArtifactReadCoverageObserved EventType = "artifact_read_coverage_observed"
	maxRequiredReadArtifacts                    = 32
	maxRequiredReadRanges                       = 4096
	maxArtifactReadObservationBytes             = 2 << 20
)

func cloneArtifactReadCoverage(source []tools.ArtifactReadCoverage) []tools.ArtifactReadCoverage {
	if source == nil {
		return nil
	}
	cloned := slices.Clone(source)
	for index := range cloned {
		cloned[index].Ranges = slices.Clone(source[index].Ranges)
	}
	return cloned
}

func validateArtifactReadCoverage(read tools.ArtifactReadCoverage, requireRanges bool) error {
	if read.ArtifactID == "" || strings.TrimSpace(read.ArtifactID) != read.ArtifactID || len(read.ArtifactID) > maxWorksetKeyBytes || !validCollectionDigest(read.SHA256) || read.ByteSize < 0 {
		return fmt.Errorf("invalid artifact content identity")
	}
	if len(read.Ranges) > maxRequiredReadRanges || (requireRanges && read.ByteSize > 0 && len(read.Ranges) == 0) {
		return fmt.Errorf("nonempty artifacts require bounded source byte ranges")
	}
	for _, span := range read.Ranges {
		if span.Start < 0 || span.End <= span.Start || span.End > read.ByteSize {
			return fmt.Errorf("invalid half-open source byte range [%d,%d) for size %d", span.Start, span.End, read.ByteSize)
		}
	}
	return nil
}

func validateRequiredArtifactReads(inputs []ArtifactRef, required []tools.ArtifactReadCoverage) error {
	if len(required) == 0 {
		return nil
	}
	if len(required) > maxRequiredReadArtifacts {
		return fmt.Errorf("at most %d required artifacts are allowed", maxRequiredReadArtifacts)
	}
	seen := make(map[string]bool, len(required))
	ranges := 0
	for _, read := range required {
		if err := validateArtifactReadCoverage(read, true); err != nil {
			return err
		}
		ranges += len(read.Ranges)
		if ranges > maxRequiredReadRanges || seen[read.ArtifactID] {
			return fmt.Errorf("duplicate artifact identity or too many required ranges")
		}
		seen[read.ArtifactID] = true
		matches := 0
		for _, input := range inputs {
			if input.ID != read.ArtifactID {
				continue
			}
			matches++
			size := input.ByteSize
			if size == 0 {
				size = input.Bytes
			}
			if input.SHA256 != read.SHA256 || size != read.ByteSize || input.Bytes < 0 || input.ByteSize < 0 || (input.Bytes != 0 && input.ByteSize != 0 && input.Bytes != input.ByteSize) {
				return fmt.Errorf("artifact %q does not match its input hash and byte size", read.ArtifactID)
			}
		}
		if matches != 1 {
			return fmt.Errorf("artifact %q must identify exactly one assigned input", read.ArtifactID)
		}
	}
	return nil
}

func taskRequiredArtifactReads(task TaskDef) []tools.ArtifactReadCoverage {
	if task.WorksetBinding == nil {
		return nil
	}
	return task.WorksetBinding.RequiredReads
}

// Required reads are delivery observations, never an assertion of comprehension.
// Tool-less actions, sidecars, native agents and model fanout cannot issue this
// local built-in view protocol and are rejected before execution instead.
func (c *Coordinator) validateRequiredArtifactReadExecution(ctx context.Context, task TaskDef, item *TodoItem, resolved ResolvedWorkerTools) error {
	required := taskRequiredArtifactReads(task)
	if len(required) == 0 {
		return nil
	}
	if err := validateRequiredArtifactReads(task.WorksetBinding.Inputs, required); err != nil {
		return fmt.Errorf("required_reads_invalid: %w", err)
	}
	if task.Sidecar || task.Action != nil || len(task.Execution.Steps) != 0 || len(task.Execution.ToolSequence) != 0 || len(task.ExecutionTopology) > 1 || ctx.Value(leafExecutionKey{}) != nil {
		return fmt.Errorf("required_reads_unsupported_execution: use a single normal worker with built-in view")
	}
	if c == nil || item == nil || !c.hasDurableEventJournal() {
		return fmt.Errorf("required_reads_journal_unavailable")
	}
	target := task.ResolvedExecutionTarget
	if target.IsZero() {
		return fmt.Errorf("required_reads_unsupported_execution: no admitted local LLM target")
	}
	if admitted, ok := ctx.Value(workerResultTargetKey{}).(execution.ExecutionTarget); ok && !admitted.IsZero() {
		target = admitted
	}
	backend, err := c.ExecutionRegistry().ResolveBackend(target.Backend)
	if err != nil || backend.Kind() != execution.BackendKindLLM {
		return fmt.Errorf("required_reads_unsupported_execution: built-in view requires a local LLM worker")
	}
	for _, candidate := range resolved.Tools {
		if candidate != nil && candidate.Info().Name == "view" && tools.IsTrustedArtifactPathTool(candidate) && slices.Contains(resolved.AuthorizedNames, "view") {
			return nil
		}
	}
	return fmt.Errorf("required_reads_view_unavailable: authorize the built-in view tool for worker execution")
}

type artifactReadObservation struct {
	Version            int                          `json:"version"`
	Scope              string                       `json:"scope"`
	ContractSHA256     string                       `json:"contract_sha256"`
	OccurrenceRevision int                          `json:"occurrence_revision"`
	DispatchID         string                       `json:"dispatch_id"`
	ModelExecutionID   string                       `json:"model_execution_id"`
	CallID             string                       `json:"call_id"`
	Output             ArtifactRef                  `json:"output"`
	Coverage           []tools.ArtifactReadCoverage `json:"coverage"`
}

type artifactReadResponse struct {
	Content  string `json:"content"`
	Metadata string `json:"metadata"`
}

func validateArtifactReadObservationEvent(event RunEvent) error {
	var observation artifactReadObservation
	if err := decodeStrictDecisionPayload(event.Payload, &observation); err != nil {
		return err
	}
	if observation.Version != 1 || !validCollectionDigest(observation.Scope) || !validCollectionDigest(observation.ContractSHA256) || observation.OccurrenceRevision < 1 || observation.DispatchID == "" || observation.ModelExecutionID == "" || observation.CallID == "" || len(observation.CallID) > maxWorksetKeyBytes || event.TaskID == "" || event.RunID == "" || event.Actor == "" || event.BranchID == "" || event.Attempt < 1 {
		return fmt.Errorf("invalid artifact read observation identity")
	}
	output := observation.Output
	if output.ID == "" || !validCollectionDigest(output.SHA256) || output.ByteSize <= 0 || output.ByteSize > maxArtifactReadObservationBytes || output.RunID != event.RunID || output.TaskID != event.TaskID || output.Attempt != event.Attempt || output.Agent != event.Actor || output.ToolCallID != observation.CallID || output.Kind != "artifact_read_observation" || len(observation.Coverage) == 0 || len(observation.Coverage) > maxRequiredReadArtifacts {
		return fmt.Errorf("invalid artifact read observation evidence")
	}
	ranges := 0
	for _, read := range observation.Coverage {
		if err := validateArtifactReadCoverage(read, false); err != nil {
			return err
		}
		ranges += len(read.Ranges)
	}
	if ranges > maxRequiredReadRanges {
		return fmt.Errorf("too many observed artifact byte ranges")
	}
	return nil
}

// Only the central policy gate calls this after an authentic successful view.
// The serialized response retains its content digest binding for process replay.
func (c *Coordinator) captureRequiredArtifactRead(ctx context.Context, inner fantasy.AgentTool, call fantasy.ToolCall, response fantasy.ToolResponse) error {
	if c == nil || response.IsError || inner.Info().Name != "view" || !tools.IsTrustedArtifactPathTool(inner) {
		return nil
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	if !ok {
		return nil
	}
	item := c.todoItemByID(metadata.TaskID)
	if item == nil || item.WorksetBinding == nil || len(item.WorksetBinding.RequiredReads) == 0 {
		return nil
	}
	coverage := tools.ViewArtifactReadCoverage(response)
	if len(coverage) == 0 {
		return nil
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, metadata.TaskID)
	if err != nil || identity.RunID != metadata.RunID || identity.Agent != metadata.AgentName || identity.Attempt != metadata.Attempt || metadata.ModelExecutionID == "" || call.ID == "" {
		return fmt.Errorf("artifact_read_observation_identity_invalid")
	}
	current, currentOK := c.activeTaskResultOccurrence(metadata.TaskID)
	if !currentOK || !sameTaskResultOccurrence(current, identity) {
		return fmt.Errorf("artifact_read_observation_identity_invalid")
	}
	if !c.hasDurableEventJournal() {
		return fmt.Errorf("artifact_read_observation_journal_unavailable")
	}
	for _, read := range coverage {
		ref, _, authorized := c.authorizedArtifactRef(ctx, read.ArtifactID)
		if !authorized || ref.SHA256 != read.SHA256 || ref.ByteSize != read.ByteSize {
			return fmt.Errorf("artifact_read_observation_source_invalid")
		}
	}
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return err
	}
	contractHash, err := decisionObjectHash(item.WorksetBinding)
	if err != nil {
		return err
	}
	observation := artifactReadObservation{Version: 1, Scope: decisionPrimitiveScope(events, c.activeBranchID(), metadata), ContractSHA256: contractHash, OccurrenceRevision: identity.OccurrenceRevision, DispatchID: identity.DispatchID, ModelExecutionID: metadata.ModelExecutionID, CallID: call.ID, Coverage: coverage}
	raw, err := json.Marshal(artifactReadResponse{Content: response.Content, Metadata: response.Metadata}, json.Deterministic(true))
	if err != nil || len(raw) > maxArtifactReadObservationBytes {
		return fmt.Errorf("artifact_read_observation_too_large")
	}
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		return err
	}
	stored, err := store.Put(context.WithoutCancel(ctx), PutArtifactRequest{Kind: "artifact_read_observation", Path: "artifact_read_observation", MediaType: "application/json", Content: raw, RunID: metadata.RunID, TaskID: metadata.TaskID, Attempt: metadata.Attempt, Agent: metadata.AgentName, ToolCallID: call.ID})
	if err != nil {
		return err
	}
	observation.Output = stored.ArtifactRef
	payload, err := json.Marshal(observation, json.Deterministic(true))
	if err != nil {
		return err
	}
	event := RunEvent{Type: string(EventArtifactReadCoverageObserved), RunID: metadata.RunID, BranchID: c.activeBranchID(), TaskID: metadata.TaskID, Attempt: metadata.Attempt, Actor: metadata.AgentName, Payload: payload}
	if err := validateArtifactReadObservationEvent(event); err != nil {
		return err
	}
	_, err = c.EventJournal().Append(context.WithoutCancel(ctx), event)
	return err
}

// Gaps carry the full immutable source identity; an empty artifact has an empty
// ranges list but still appears until a successful empty read was delivered.
type artifactReadCoverageGap struct {
	ArtifactID string                    `json:"artifact_id"`
	SHA256     string                    `json:"sha256"`
	ByteSize   int64                     `json:"byte_size"`
	Ranges     []tools.ArtifactByteRange `json:"ranges"`
}

type artifactReadCoverageIncomplete struct {
	Code         string                    `json:"code"`
	MissingReads []artifactReadCoverageGap `json:"missing_reads"`
}

func (e *artifactReadCoverageIncomplete) Error() string {
	raw, _ := json.Marshal(e)
	return string(raw)
}

func missingArtifactByteRanges(required, observed []tools.ArtifactByteRange) []tools.ArtifactByteRange {
	union := slices.Clone(observed)
	slices.SortFunc(union, func(a, b tools.ArtifactByteRange) int {
		return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.End, b.End))
	})
	var missing []tools.ArtifactByteRange
	for _, want := range required {
		cursor := want.Start
		for _, have := range union {
			if have.End <= cursor {
				continue
			}
			if have.Start >= want.End {
				break
			}
			if have.Start > cursor {
				missing = append(missing, tools.ArtifactByteRange{Start: cursor, End: have.Start})
			}
			cursor = max(cursor, have.End)
			if cursor >= want.End {
				break
			}
		}
		if cursor < want.End {
			missing = append(missing, tools.ArtifactByteRange{Start: cursor, End: want.End})
		}
	}
	return missing
}

func (c *Coordinator) validateRequiredArtifactReadSubmission(ctx context.Context, taskID string) error {
	if c == nil {
		return nil
	}
	item := c.todoItemByID(taskID)
	if item == nil || item.WorksetBinding == nil || len(item.WorksetBinding.RequiredReads) == 0 {
		return nil
	}
	binding := item.WorksetBinding
	if err := validateRequiredArtifactReads(binding.Inputs, binding.RequiredReads); err != nil {
		return fmt.Errorf("required_reads_invalid: %w", err)
	}
	metadata, ok := invocationMetadataFromContext(ctx)
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, taskID)
	if !ok || err != nil || metadata.TaskID != taskID || metadata.RunID != identity.RunID || metadata.Attempt != identity.Attempt || metadata.AgentName != identity.Agent || metadata.ModelExecutionID == "" {
		return fmt.Errorf("artifact_read_coverage_identity_invalid")
	}
	workRevision, workDispatch := identity.OccurrenceRevision, identity.DispatchID
	if checkpoint, collecting := ctx.Value(resultCollectionAdmissionKey{}).(*ResultCollectionCheckpoint); collecting && checkpoint != nil {
		workRevision, workDispatch = checkpoint.WorkOccurrenceRevision, checkpoint.WorkDispatchID
		if metadata.ModelExecutionID != checkpoint.ModelExecutionID {
			return fmt.Errorf("artifact_read_coverage_identity_invalid")
		}
	}
	if !c.hasDurableEventJournal() {
		return fmt.Errorf("artifact_read_observation_journal_unavailable")
	}
	sourceStore, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		return err
	}
	for _, want := range binding.RequiredReads {
		for _, input := range binding.Inputs {
			if input.ID == want.ArtifactID {
				if err := sourceStore.Verify(ctx, input); err != nil {
					return fmt.Errorf("artifact_read_source_invalid: %w", err)
				}
			}
		}
	}
	observed, err := c.observedRequiredArtifactReads(ctx, binding, identity, metadata, workRevision, workDispatch)
	if err != nil {
		return err
	}
	gaps := &artifactReadCoverageIncomplete{Code: "artifact_read_coverage_incomplete"}
	for _, want := range binding.RequiredReads {
		have, delivered := observed[want.ArtifactID]
		missing := missingArtifactByteRanges(want.Ranges, have)
		if len(missing) > 0 || !delivered {
			if missing == nil {
				missing = []tools.ArtifactByteRange{}
			}
			gaps.MissingReads = append(gaps.MissingReads, artifactReadCoverageGap{ArtifactID: want.ArtifactID, SHA256: want.SHA256, ByteSize: want.ByteSize, Ranges: missing})
		}
	}
	if len(gaps.MissingReads) > 0 {
		return gaps
	}
	return nil
}

// Revalidate the durable response binding; replay never infers delivery from a scan.
func (c *Coordinator) observedRequiredArtifactReads(ctx context.Context, binding *WorksetBinding, identity submitResultRuntimeIdentity, metadata InvocationMetadata, workRevision int, workDispatch string) (map[string][]tools.ArtifactByteRange, error) {
	events, err := c.readActiveLineageEvents(ctx)
	if err != nil {
		return nil, err
	}
	contractHash, err := decisionObjectHash(binding)
	if err != nil {
		return nil, err
	}
	scope := decisionPrimitiveScope(events, c.activeBranchID(), metadata)
	observed := make(map[string][]tools.ArtifactByteRange)
	for _, event := range events {
		if event.Type != string(EventArtifactReadCoverageObserved) || event.TaskID != identity.TaskID || event.RunID != identity.RunID || event.Attempt != identity.Attempt || event.Actor != identity.Agent {
			continue
		}
		if err := validateArtifactReadObservationEvent(event); err != nil {
			return nil, fmt.Errorf("artifact_read_observation_invalid: %w", err)
		}
		var observation artifactReadObservation
		if err := json.Unmarshal(event.Payload, &observation); err != nil {
			return nil, err
		}
		if observation.Scope != scope || observation.ContractSHA256 != contractHash || observation.OccurrenceRevision != workRevision || observation.DispatchID != workDispatch || observation.ModelExecutionID != metadata.ModelExecutionID {
			continue
		}
		raw, err := c.readDecisionArtifact(ctx, observation.Output, maxArtifactReadObservationBytes)
		if err != nil {
			return nil, fmt.Errorf("artifact_read_observation_invalid: %w", err)
		}
		var delivered artifactReadResponse
		if err := json.Unmarshal(raw, &delivered, json.RejectUnknownMembers(true)); err != nil {
			return nil, fmt.Errorf("artifact_read_observation_invalid: %w", err)
		}
		actual := tools.ViewArtifactReadCoverage(fantasy.ToolResponse{Content: delivered.Content, Metadata: delivered.Metadata})
		actualHash, err := decisionObjectHash(actual)
		if err != nil {
			return nil, err
		}
		declaredHash, err := decisionObjectHash(observation.Coverage)
		if err != nil || actualHash != declaredHash || len(actual) == 0 {
			return nil, fmt.Errorf("artifact_read_observation_invalid: delivered coverage differs")
		}
		for _, read := range actual {
			for _, want := range binding.RequiredReads {
				if read.ArtifactID == want.ArtifactID && read.SHA256 == want.SHA256 && read.ByteSize == want.ByteSize {
					if _, exists := observed[want.ArtifactID]; !exists {
						observed[want.ArtifactID] = []tools.ArtifactByteRange{}
					}
					observed[want.ArtifactID] = append(observed[want.ArtifactID], read.Ranges...)
				}
			}
		}
	}
	return observed, nil
}
