package main

import (
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
	SourceTaskID      string                 `json:"source_task_id"`
	SourceHash        string                 `json:"source_payload_sha256"`
	GapIndex          int                    `json:"gap_index"`
	ReviewRevision    string                 `json:"review_revision"`
	Bindings          map[string]string      `json:"bindings"`
	Description       string                 `json:"description"`
	MissingEvidence   string                 `json:"missing_evidence"`
	Kind              string                 `json:"kind"`
	RequestedEvidence []supplementEvidence   `json:"requested_evidence"`
	Limitations       []supplementLimitation `json:"limitations"`
}

func (r frozenSupplementRequest) gap() supplementGap {
	return supplementGap{Description: r.Description, MissingEvidence: r.MissingEvidence, Kind: r.Kind, RequestedEvidence: r.RequestedEvidence, Limitations: r.Limitations}
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
	ID              string
	Status          string
	ContractID      string `json:"contract_id"`
	VerifiedResult  bool   `json:"verified_result"`
	VerifiedOutputs bool   `json:"verified_outputs"`
	OutputsError    string `json:"outputs_error"`
	Workspace       string `json:"workspace"`
	Binding         *struct {
		Bindings map[string]string `json:"bindings"`
	} `json:"workset_binding"`
	Result *struct {
		Status  string                     `json:"status"`
		Outputs map[string]json.RawMessage `json:"runtime_outputs"`
		Payload *struct {
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
	data, err := actionstate.ActionSources(ctx)
	if err != nil {
		return actionResult{}, err
	}
	var current struct {
		Version    int              `json:"version"`
		SnapshotID string           `json:"input_id"`
		Tasks      []supplementTask `json:"tasks"`
	}
	if err := json.Unmarshal(data, &current); err != nil {
		return actionResult{}, err
	}
	if current.Version != 1 {
		return actionResult{}, fmt.Errorf("unsupported source view version")
	}
	producer, requestTask, err := selectSupplementPrerequisites(current.Tasks, os.Getenv("HUFU_TASK_ID"))
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
	if err := validateSupplementSources(current.Tasks, requests, scope); err != nil {
		return actionResult{}, err
	}
	producerRoot := producer.Workspace
	if producerRoot == "" {
		return actionResult{}, fmt.Errorf("verified producer workspace missing")
	}
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
	canonicalInputs := map[string]artifact{}
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
		outcome := supplementOutcome{SourceTaskID: request.SourceTaskID, GapIndex: request.GapIndex, Kind: request.Kind, Limitations: append([]supplementLimitation{}, request.Limitations...)}
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
		entry.Bindings["supplement_kind"] = request.Kind
		entry.Bindings["supplement_round"] = "1"
		// Content IDs select one producer declaration. Different gaps may ask
		// for identical bytes; reuse the first reference instead of publishing
		// conflicting paths for that ID. Keep each gap's identity and reads.
		for index, ref := range entry.Inputs {
			canonical, exists := canonicalInputs[ref.ID]
			if !exists {
				canonicalInputs[ref.ID] = ref
				artifacts = append(artifacts, ref)
				continue
			}
			comparison := canonical
			comparison.Path = ref.Path
			if comparison != ref {
				return actionResult{}, fmt.Errorf("supplement artifact %q has conflicting content metadata", ref.ID)
			}
			entry.Inputs[index] = canonical
			for name, value := range entry.Bindings {
				if value == ref.Path {
					entry.Bindings[name] = canonical.Path
				}
			}
		}
		outcome.WorksetKey = entry.Key
		items = append(items, entry)
		total += len(candidate.sourceBundle) + len(candidate.evidenceInventory) + candidate.diff.Len()
		outcomes = append(outcomes, outcome)
	}
	result := actionResult{Outputs: map[string]any{"supplement_prepared": true, "supplement_round": 1, "supplement_outcomes": outcomes, "item_count": len(items), "scope": scope}, Artifacts: artifacts}
	// An unmaterialized request remains typed audit evidence, not executable
	// work. Runtime projects every declared workset before action acceptance,
	// even when the coordinator would subsequently skip empty fan-out.
	if len(items) == 0 {
		return result, nil
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
	result.Artifacts = append(result.Artifacts, manifestArtifact)
	return result, nil
}

func prepareSupplementBatch(producerRoot string, request frozenSupplementRequest, remaining int) (*batch, *reviewSourceSnapshot, *supplementLimitation) {
	fail := func(kind, detail string) (*batch, *reviewSourceSnapshot, *supplementLimitation) {
		return nil, nil, &supplementLimitation{Kind: kind, Detail: detail}
	}
	switch request.Kind {
	case "unread", "truncated", "missing_dependency":
	default:
		return fail("invalid_request", "Only typed source evidence gaps can request supplementation")
	}
	if len(request.RequestedEvidence) == 0 || len(request.RequestedEvidence) > 16 {
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
	for _, evidence := range request.RequestedEvidence {
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
	fmt.Fprintf(&current.diff, "Supplement original task %s gap %d (%s). Original findings and gap remain immutable.\n", request.SourceTaskID, request.GapIndex, request.Kind)
	fmt.Fprintf(&current.diff, "Requested evidence: %s\nMissing evidence: %s\n", request.Description, request.MissingEvidence)
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

func validateSupplementSources(tasks []supplementTask, requests frozenSupplementRequests, scope scopeAttestation) error {
	for _, request := range requests.Requests {
		found := false
		for _, task := range tasks {
			if task.ID != request.SourceTaskID {
				continue
			}
			if !task.VerifiedResult || task.Result == nil || task.Result.Payload == nil || task.Binding == nil || task.Result.Payload.Hash != request.SourceHash {
				return fmt.Errorf("supplement source receipt mismatch")
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
			b, _ := json.Marshal(request.gap())
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

func selectSupplementPrerequisites(tasks []supplementTask, taskID string) (*supplementTask, *supplementTask, error) {
	var producer, requestTask *supplementTask
	active := false
	for index := range tasks {
		task := &tasks[index]
		if task.ContractID == "prepare-review-supplement" {
			if task.ID != taskID || task.Status != "in_progress" || active {
				return nil, nil, fmt.Errorf("supplement is limited to one round")
			}
			active = true
		}
		if task.ContractID == "produce-workset" {
			if producer != nil || !task.VerifiedOutputs || task.Result == nil {
				return nil, nil, fmt.Errorf("invalid original producer: %s", task.OutputsError)
			}
			producer = task
		}
		if task.ContractID == "inventory-review-supplement-requests" {
			if requestTask != nil || !task.VerifiedOutputs || task.Result == nil {
				return nil, nil, fmt.Errorf("invalid supplement request producer: %s", task.OutputsError)
			}
			requestTask = task
		}
	}
	if !active || producer == nil || requestTask == nil {
		return nil, nil, fmt.Errorf("accepted supplement prerequisites missing")
	}
	return producer, requestTask, nil
}
