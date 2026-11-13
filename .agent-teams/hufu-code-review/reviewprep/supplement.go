package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/actionstate"
	"github.com/kjelly/hufu/internal/tools"
)

const maxSupplementItems = 8
const maxSupplementBytes = 2 * 1024 * 1024
const maxSupplementItemBytes = 393216
const maxSupplementSourceReadBytes = 2 * 1024 * 1024

type supplementEvidence struct {
	Path   string                    `json:"path"`
	Ranges []tools.ArtifactByteRange `json:"ranges"`
}
type supplementLimitation struct {
	Kind   string `json:"kind"`
	Detail string `json:"detail"`
}
type supplementGap struct {
	Description       string                 `json:"description"`
	MissingEvidence   string                 `json:"missing_evidence"`
	Kind              string                 `json:"kind"`
	RequestedEvidence []supplementEvidence   `json:"requested_evidence"`
	Limitations       []supplementLimitation `json:"limitations"`
}
type frozenSupplementRequest struct {
	SourceTaskID   string            `json:"source_task_id"`
	SourceHash     string            `json:"source_payload_sha256"`
	GapIndex       int               `json:"gap_index"`
	ReviewRevision string            `json:"review_revision"`
	Bindings       map[string]string `json:"bindings"`
	Gap            supplementGap     `json:"gap"`
}
type frozenSupplementRequests struct {
	Round      int                       `json:"round"`
	SnapshotID string                    `json:"run_input_snapshot_id"`
	Requests   []frozenSupplementRequest `json:"requests"`
}
type supplementOutcome struct {
	SourceTaskID string                 `json:"source_task_id"`
	GapIndex     int                    `json:"gap_index"`
	Kind         string                 `json:"kind"`
	WorksetKey   string                 `json:"workset_key"`
	Limitations  []supplementLimitation `json:"limitations"`
}

type supplementTask struct {
	ID         string
	Status     string
	ContractID string `json:"contract_id"`
	SnapshotID string `json:"run_input_snapshot_id"`
	Binding    *struct {
		Bindings map[string]string `json:"bindings"`
	} `json:"workset_binding"`
	Receipt *struct {
		RunID              string `json:"run_id"`
		TaskID             string `json:"task_id"`
		ActionID           string `json:"action_invocation_id"`
		ProducerID         string `json:"producer_id"`
		ModelExecutionID   string `json:"model_execution_id"`
		TranscriptRef      string `json:"transcript_ref"`
		RuntimeOutputsHash string `json:"runtime_outputs_hash"`
		Attempt            int    `json:"attempt"`
	} `json:"execution_receipt"`
	VerifyResult *struct {
		ExitCode    *int   `json:"exit_code"`
		TimedOut    bool   `json:"timed_out"`
		Fingerprint string `json:"fingerprint"`
	}
	Result *struct {
		Status             string                     `json:"status"`
		Outputs            map[string]json.RawMessage `json:"runtime_outputs"`
		RuntimeOutputsHash string                     `json:"runtime_outputs_hash"`
		Payload            *struct {
			Value json.RawMessage `json:"value"`
			Hash  string          `json:"sha256"`
		} `json:"structured_payload"`
	} `json:"typed_result"`
}

// The action accepts scope only. Runtime-owned checkpoint receipts select the
// request producer, original accepted gaps, and PREPARE's retained source tree.
func PrepareReviewSupplement(ctx context.Context, payload string) (actionResult, error) {
	var arguments struct {
		Scope resolverScope `json:"scope"`
	}
	if err := decodeStrictJSON([]byte(payload), &arguments); err != nil {
		return actionResult{}, err
	}
	if err := validateRequestedScope(arguments.Scope); err != nil {
		return actionResult{}, err
	}
	workspace := os.Getenv("HUFU_WORKSPACE")
	runID, actionID := os.Getenv("HUFU_RUN_ID"), os.Getenv("HUFU_ACTION_INVOCATION_ID")
	root := filepath.Clean(workspace)
	for count := 0; count < 5; count++ {
		root = filepath.Dir(root)
	}
	if !filepath.IsAbs(workspace) || runID == "" || actionID == "" || filepath.Join(root, "runtime", "runs", runID, "actions", actionID) != workspace {
		return actionResult{}, fmt.Errorf("unsupported supplement action identity")
	}
	data, err := actionstate.SessionCheckpoint(root)
	if err != nil {
		return actionResult{}, err
	}
	var current struct {
		SnapshotID string           `json:"active_run_input_snapshot_id"`
		Tasks      []supplementTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &current); err != nil {
		return actionResult{}, err
	}
	origins := map[string]string{}
	if raw := os.Getenv("HUFU_RECOVERED_TASK_RUNS"); raw != "" {
		if len(raw) > 64*1024 {
			return actionResult{}, fmt.Errorf("recovery origins exceed limit")
		}
		if err := json.Unmarshal([]byte(raw), &origins); err != nil {
			return actionResult{}, err
		}
	}
	valid := func(task supplementTask) bool { return validSupplementTask(task, current.SnapshotID, runID, origins) }
	producer, requestTask, err := selectSupplementPrerequisites(current.Tasks, current.SnapshotID, os.Getenv("HUFU_TASK_ID"), valid)
	if err != nil {
		return actionResult{}, err
	}
	var scope scopeAttestation
	if err := json.Unmarshal(producer.Result.Outputs["scope"], &scope); err != nil {
		return actionResult{}, err
	}
	if scope.Requested != arguments.Scope || !scope.Satisfied || scope.RequestedInputHash != scopeInputDigest(arguments.Scope) {
		return actionResult{}, fmt.Errorf("supplement scope differs from frozen producer")
	}
	var requests frozenSupplementRequests
	if err := decodeStrictJSON(requestTask.Result.Outputs["review_supplement_requests"], &requests); err != nil {
		return actionResult{}, err
	}
	if requests.Round != 1 || requests.SnapshotID != current.SnapshotID {
		return actionResult{}, fmt.Errorf("supplement request snapshot or round mismatch")
	}
	if err := validateSupplementSources(current.Tasks, requests, scope, valid); err != nil {
		return actionResult{}, err
	}
	if err := verifySupplementRuntimeOutputs(*producer); err != nil {
		return actionResult{}, err
	}
	if err := verifySupplementRuntimeOutputs(*requestTask); err != nil {
		return actionResult{}, err
	}
	producerRoot := filepath.Join(root, "runtime", "runs", producer.Receipt.RunID, "actions", producer.Receipt.ActionID)
	originals := 0
	for _, task := range current.Tasks {
		switch task.ContractID {
		case "review-primary-workset", "review-documentation-workset", "review-documentation-escalation":
			originals++
		}
	}
	// Keep the interpreted adapter compatible with Yaegi's built-in subset.
	remaining := 32 - originals
	if remaining < 0 {
		remaining = 0
	}
	if remaining > maxSupplementItems {
		remaining = maxSupplementItems
	}
	return materializeReviewSupplement(ctx, workspace, producerRoot, scope, requests, remaining)
}

func materializeReviewSupplement(ctx context.Context, artifactRoot, producerRoot string, scope scopeAttestation, requests frozenSupplementRequests, maxItems int) (actionResult, error) {
	outputDir := filepath.Join(artifactRoot, "supplement")
	if err := ensureEmptyOutputDir(outputDir); err != nil {
		return actionResult{}, err
	}
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return actionResult{}, err
	}
	items := []item{}
	artifacts := []artifact{}
	outcomes := []supplementOutcome{}
	seen := map[string]bool{}
	total := 0
	for _, request := range requests.Requests {
		if err := ctx.Err(); err != nil {
			return actionResult{}, err
		}
		identity := request.SourceTaskID + "/" + strconv.Itoa(request.GapIndex)
		if seen[identity] {
			return actionResult{}, fmt.Errorf("duplicate supplement gap request")
		}
		seen[identity] = true
		outcome := supplementOutcome{SourceTaskID: request.SourceTaskID, GapIndex: request.GapIndex, Kind: request.Gap.Kind, Limitations: append([]supplementLimitation{}, request.Gap.Limitations...)}
		if len(items) >= maxItems {
			outcome.Limitations = append(outcome.Limitations, supplementLimitation{Kind: "budget_exhausted", Detail: "The single supplement round permits at most eight items and 32 total review sources"})
			outcomes = append(outcomes, outcome)
			continue
		}
		candidate, snapshot, limitation := prepareSupplementBatch(producerRoot, request, maxSupplementBytes-total)
		if limitation != nil {
			outcome.Limitations = append(outcome.Limitations, *limitation)
			outcomes = append(outcomes, outcome)
			continue
		}
		directory := filepath.Join(outputDir, fmt.Sprintf("gap-%04d", len(items)))
		created, err := writeItems(artifactRoot, directory, []*batch{candidate}, snapshot)
		if err != nil {
			return actionResult{}, err
		}
		entry := created[0]
		entry.Key = fmt.Sprintf("supplement-%04d", len(items))
		entry.Bindings["key"] = entry.Key
		entry.Bindings["source_task_id"] = request.SourceTaskID
		entry.Bindings["source_gap_index"] = strconv.Itoa(request.GapIndex)
		entry.Bindings["source_payload_sha256"] = request.SourceHash
		entry.Bindings["reference_candidate_ids"] = "[]"
		entry.Bindings["supplement_kind"] = request.Gap.Kind
		entry.Bindings["supplement_round"] = "1"
		outcome.WorksetKey = entry.Key
		items = append(items, entry)
		artifacts = append(artifacts, entry.Inputs...)
		total += len(candidate.sourceBundle) + len(candidate.evidenceInventory) + candidate.diff.Len()
		outcomes = append(outcomes, outcome)
	}
	m := manifest{SchemaVersion: manifestSchemaVersion, Scope: scope, Items: items, Range: reviewRange{Start: scope.Resolved.Base, End: scope.Resolved.Head}, Observed: observedBudget{WorksetItems: len(items)}}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return actionResult{}, err
	}
	manifestPath := filepath.Join(outputDir, "workset-manifest.json")
	if err := os.WriteFile(manifestPath, append(data, '\n'), 0644); err != nil {
		return actionResult{}, err
	}
	manifestArtifact, err := fileArtifact(artifactRoot, manifestPath, "workset_manifest")
	if err != nil {
		return actionResult{}, err
	}
	manifestArtifact.Description = "supplemental review workset manifest"
	artifacts = append(artifacts, manifestArtifact)
	return actionResult{Outputs: map[string]any{"supplement_prepared": true, "supplement_round": 1, "supplement_outcomes": outcomes, "item_count": len(items), "scope": scope}, Artifacts: artifacts}, nil
}

func prepareSupplementBatch(producerRoot string, request frozenSupplementRequest, remaining int) (*batch, *reviewSourceSnapshot, *supplementLimitation) {
	fail := func(kind, detail string) (*batch, *reviewSourceSnapshot, *supplementLimitation) {
		return nil, nil, &supplementLimitation{Kind: kind, Detail: detail}
	}
	switch request.Gap.Kind {
	case "unread", "truncated", "missing_dependency":
	default:
		return fail("invalid_request", "Only typed source evidence gaps can request supplementation")
	}
	if len(request.Gap.RequestedEvidence) == 0 || len(request.Gap.RequestedEvidence) > 16 {
		return fail("invalid_request", "No bounded exact paths were requested; prose is not interpreted as a path selector")
	}
	manifestName := request.Bindings["frozen_source_manifest"]
	clean, err := cleanSnapshotPath(manifestName)
	if err != nil || manifestName == "" {
		return fail("unavailable_path", "Original review has no valid retained frozen-source manifest")
	}
	manifestPath := filepath.Join(producerRoot, clean)
	resolved, err := filepath.EvalSymlinks(manifestPath)
	if err != nil || !pathWithin(producerRoot, resolved) {
		return fail("out_of_scope", "Frozen manifest escapes the accepted producer workspace or is unavailable")
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return fail("unavailable_path", "Retained frozen-source manifest is unavailable")
	}
	if sha256Hex(data) != request.Bindings["frozen_source_sha256"] {
		return fail("version_mismatch", "Retained frozen-source manifest hash differs from the original accepted binding")
	}
	var frozen frozenSourceManifest
	if err := decodeStrictJSON(data, &frozen); err != nil {
		return fail("invalid_request", "Retained frozen-source manifest cannot be decoded")
	}
	if frozen.Revision != request.ReviewRevision {
		return fail("version_mismatch", "Retained source revision differs from the accepted review")
	}
	rootName, err := cleanSnapshotPath(request.Bindings["frozen_source_root"])
	if err != nil {
		return fail("out_of_scope", "Frozen-source root is outside the accepted grant")
	}
	sourceRoot := filepath.Join(producerRoot, rootName)
	resolvedRoot, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil || !pathWithin(producerRoot, resolvedRoot) {
		return fail("out_of_scope", "Frozen-source tree is unavailable or outside the producer grant")
	}
	return materializeSupplementBatch(sourceRoot, resolvedRoot, frozen, request, remaining)
}

func materializeSupplementBatch(sourceRoot, resolvedRoot string, frozen frozenSourceManifest, request frozenSupplementRequest, remaining int) (*batch, *reviewSourceSnapshot, *supplementLimitation) {
	fail := func(kind, detail string) (*batch, *reviewSourceSnapshot, *supplementLimitation) {
		return nil, nil, &supplementLimitation{Kind: kind, Detail: detail}
	}
	current := &batch{lens: "general", pathSeen: map[string]struct{}{}, sourceScopes: map[string][]tools.ArtifactByteRange{}, relatedBudget: 0}
	selectedBytes := 0
	for _, evidence := range request.Gap.RequestedEvidence {
		clean, err := cleanSnapshotPath(evidence.Path)
		if err != nil || filepath.ToSlash(clean) != evidence.Path || current.hasPath(evidence.Path) || evidence.Ranges == nil || len(evidence.Ranges) > 32 {
			return fail("invalid_request", "Requested paths and byte ranges must be exact, unique and bounded")
		}
		var file *snapshotFile
		for index := range frozen.Files {
			if frozen.Files[index].Path == evidence.Path {
				file = &frozen.Files[index]
				break
			}
		}
		if file == nil {
			return fail("unavailable_path", "Requested dependency is not present in the retained frozen-source inventory: "+evidence.Path)
		}
		if file.Mode == "120000" || file.Disposition == "excluded" {
			return fail("out_of_scope", "Requested path is not regular reviewable frozen source: "+evidence.Path)
		}
		if file.Bytes > maxSupplementSourceReadBytes {
			return fail("budget_exhausted", "Requested source exceeds the retained-file read byte ceiling: "+evidence.Path)
		}
		spans := evidence.Ranges
		if len(spans) == 0 && file.Bytes > 0 {
			spans = []tools.ArtifactByteRange{{Start: 0, End: file.Bytes}}
		}
		var end int64
		for _, span := range spans {
			if span.Start < end || span.End <= span.Start || span.End > file.Bytes {
				return fail("invalid_request", "Requested byte ranges are outside the frozen file or overlap: "+evidence.Path)
			}
			selectedBytes += int(span.End - span.Start)
			end = span.End
		}
		if selectedBytes+16384 > maxSupplementItemBytes || selectedBytes+16384 > remaining {
			return fail("budget_exhausted", "Requested evidence exceeds the single-round item or total byte limit")
		}
		filePath := filepath.Join(sourceRoot, clean)
		resolvedFile, err := filepath.EvalSymlinks(filePath)
		if err != nil || !pathWithin(resolvedRoot, resolvedFile) {
			return fail("out_of_scope", "Requested file is unavailable or escapes frozen source: "+evidence.Path)
		}
		content, err := os.ReadFile(resolvedFile)
		if err != nil {
			return fail("unavailable_path", "Retained frozen source is unavailable: "+evidence.Path)
		}
		if int64(len(content)) != file.Bytes || sha256Hex(content) != file.SHA256 {
			return fail("version_mismatch", "Retained file differs from its frozen source identity: "+evidence.Path)
		}
		current.paths = append(current.paths, evidence.Path)
		current.pathSeen[evidence.Path] = struct{}{}
		current.sourceScopes[evidence.Path] = spans
	}
	fmt.Fprintf(&current.diff, "Supplement original task %s gap %d (%s). Original findings and gap remain immutable.\n", request.SourceTaskID, request.GapIndex, request.Gap.Kind)
	fmt.Fprintf(&current.diff, "Requested evidence: %s\nMissing evidence: %s\n", request.Gap.Description, request.Gap.MissingEvidence)
	current.lines = strings.Count(current.diff.String(), "\n")
	snapshot := &reviewSourceSnapshot{root: sourceRoot, revision: frozen.Revision, baseRevision: frozen.BaseRevision, skipFrozenSource: true}
	bundle, inventory, err := snapshot.bundleDataWithScopes("supplement", current.paths, current.sourceScopes, 0)
	if err != nil {
		return fail("invalid_request", "Requested source ranges cannot be materialized: "+err.Error())
	}
	var observed evidenceInventory
	if err := json.Unmarshal(inventory, &observed); err != nil {
		return fail("invalid_request", "Supplement evidence inventory is invalid: "+err.Error())
	}
	for _, source := range observed.Sources {
		if source.Role == "primary" && (source.Omitted || source.Truncated) {
			return fail("unavailable_path", "Requested frozen source cannot be returned as readable evidence: "+source.Path)
		}
	}
	if len(bundle)+len(inventory)+current.diff.Len() > maxSupplementItemBytes || len(bundle)+len(inventory)+current.diff.Len() > remaining {
		return fail("budget_exhausted", "Materialized evidence metadata and source exceed the bounded byte limit")
	}
	current.sourceBundle, current.evidenceInventory = bundle, inventory
	return current, snapshot, nil
}

func verifySupplementRuntimeOutputs(task supplementTask) error {
	if task.Result == nil || task.Receipt == nil || task.Result.RuntimeOutputsHash == "" || task.Result.RuntimeOutputsHash != task.Receipt.RuntimeOutputsHash {
		return fmt.Errorf("supplement prerequisite lacks receipt-bound runtime outputs")
	}
	data, err := json.Marshal(task.Result.Outputs)
	if err != nil {
		return err
	}
	// Runtime output receipts hash the complete two-space-indented JSON map.
	// Structured result payloads below use compact JSON; their canonicalizer
	// must not be reused for a runtime output receipt.
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	canonical, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if "sha256:"+sha256Hex(canonical) != task.Result.RuntimeOutputsHash {
		return fmt.Errorf("supplement prerequisite runtime output digest mismatch")
	}
	return nil
}

func canonicalSupplementJSON(data []byte) ([]byte, error) {
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func validateSupplementSources(tasks []supplementTask, requests frozenSupplementRequests, scope scopeAttestation, valid func(supplementTask) bool) error {
	for _, request := range requests.Requests {
		found := false
		for _, task := range tasks {
			if task.ID != request.SourceTaskID {
				continue
			}
			if !valid(task) || task.Receipt.ProducerID == "" || task.Receipt.ModelExecutionID == "" || task.Receipt.TranscriptRef == "" || task.Result.Payload == nil || task.Binding == nil || task.Result.Payload.Hash != request.SourceHash {
				return fmt.Errorf("supplement source receipt mismatch")
			}
			canonical, err := canonicalSupplementJSON(task.Result.Payload.Value)
			if err != nil || sha256Hex(canonical) != request.SourceHash {
				return fmt.Errorf("supplement source payload digest mismatch")
			}
			switch task.ContractID {
			case "review-primary-workset", "review-documentation-workset", "review-documentation-escalation":
			default:
				return fmt.Errorf("supplement source is not an original review")
			}
			var value struct {
				Record struct {
					Revision string            `json:"review_revision"`
					Gaps     []json.RawMessage `json:"gaps"`
				} `json:"record"`
			}
			if err := json.Unmarshal(task.Result.Payload.Value, &value); err != nil {
				return err
			}
			if value.Record.Revision != request.ReviewRevision || request.ReviewRevision != scope.Resolved.Head || request.GapIndex < 0 || request.GapIndex >= len(value.Record.Gaps) {
				return fmt.Errorf("supplement source gap identity mismatch")
			}
			var gap supplementGap
			if err := decodeStrictJSON(value.Record.Gaps[request.GapIndex], &gap); err != nil {
				return err
			}
			a, _ := json.Marshal(gap)
			b, _ := json.Marshal(request.Gap)
			c, _ := json.Marshal(task.Binding.Bindings)
			d, _ := json.Marshal(request.Bindings)
			if string(a) != string(b) || string(c) != string(d) {
				return fmt.Errorf("supplement substituted accepted gap or frozen source grant")
			}
			found = true
		}
		if !found {
			return fmt.Errorf("supplement source task missing")
		}
	}
	return nil
}

func validSupplementTask(task supplementTask, snapshotID, runID string, origins map[string]string) bool {
	return task.Status == "done" && task.Result != nil && (task.Result.Status == "success" || task.Result.Status == "completed_with_gaps") && snapshotID != "" && (task.SnapshotID == "" || task.SnapshotID == snapshotID) && task.Receipt != nil && task.Receipt.TaskID == task.ID && task.Receipt.Attempt > 0 && (task.Receipt.RunID == runID || origins[task.ID] == task.Receipt.RunID && task.SnapshotID == snapshotID) && task.VerifyResult != nil && task.VerifyResult.ExitCode != nil && *task.VerifyResult.ExitCode == 0 && !task.VerifyResult.TimedOut && task.VerifyResult.Fingerprint != ""
}

func selectSupplementPrerequisites(tasks []supplementTask, snapshotID, taskID string, valid func(supplementTask) bool) (*supplementTask, *supplementTask, error) {
	var producer, requestTask *supplementTask
	active := false
	for index := range tasks {
		task := &tasks[index]
		if task.ContractID == "prepare-review-supplement" {
			if task.ID != taskID || task.Status != "in_progress" || task.SnapshotID != snapshotID || active {
				return nil, nil, fmt.Errorf("supplement is limited to one round")
			}
			active = true
		}
		if task.ContractID == "produce-workset" {
			if producer != nil || !valid(*task) || task.Result.Status != "success" || task.SnapshotID != snapshotID {
				return nil, nil, fmt.Errorf("invalid original producer")
			}
			producer = task
		}
		if task.ContractID == "inventory-review-supplement-requests" {
			if requestTask != nil || !valid(*task) || task.Result.Status != "success" || task.SnapshotID != snapshotID {
				return nil, nil, fmt.Errorf("invalid supplement request producer")
			}
			requestTask = task
		}
	}
	if !active || producer == nil || requestTask == nil || producer.Receipt.ActionID == "" || requestTask.Receipt.ActionID == "" {
		return nil, nil, fmt.Errorf("accepted supplement prerequisites missing")
	}
	return producer, requestTask, nil
}
