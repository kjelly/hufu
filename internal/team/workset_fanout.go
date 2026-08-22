package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func usesStructuredWorkset(task TaskDef) bool {
	if task.FanOut == nil {
		return false
	}
	return strings.TrimSpace(task.FanOut.SourceArtifact.TaskID) != "" ||
		strings.TrimSpace(task.FanOut.SourceArtifact.Artifact) != "" ||
		strings.EqualFold(filepath.Ext(strings.TrimSpace(task.FanOut.Source)), ".json")
}

func (c *Coordinator) expandStructuredFanOutTask(task TaskDef) ([]TaskDef, error) {
	if task.FanOut == nil {
		return nil, fmt.Errorf("fan-out spec is nil")
	}
	spec := task.FanOut
	if strings.TrimSpace(spec.GoalTemplate) == "" {
		return nil, fmt.Errorf("goal_template is required")
	}
	data, source, err := c.readStructuredWorksetSource(task)
	if err != nil {
		return nil, err
	}
	manifest, err := normalizeStructuredWorkset(data)
	if err != nil {
		return nil, err
	}
	manifest, err = c.validateWorksetInputs(manifest, spec.SourceArtifact.TaskID)
	if err != nil {
		return nil, err
	}
	workset := worksetID(source.ID, source.SHA256, manifest.Items)
	for _, match := range fanOutPlaceholderPattern.FindAllStringSubmatch(spec.GoalTemplate, -1) {
		if _, ok := manifest.Items[0].Bindings[match[1]]; !ok {
			return nil, fmt.Errorf("goal_template placeholder {%s} does not match a workset binding", match[1])
		}
	}

	expanded := make([]TaskDef, 0, len(manifest.Items))
	for _, item := range manifest.Items {
		rowTask := task
		rowTask.FanOut = nil
		rowTask.Goal = fanOutPlaceholderPattern.ReplaceAllStringFunc(spec.GoalTemplate, func(match string) string {
			return item.Bindings[match[1:len(match)-1]]
		})
		rowTask.WorksetBinding = &WorksetBinding{
			WorksetID:        workset,
			ParentTaskID:     task.ID,
			ItemKey:          item.Key,
			Bindings:         cloneStringMap(item.Bindings),
			Inputs:           append([]ArtifactRef(nil), item.Inputs...),
			SourceArtifactID: source.ID,
			SourceSHA256:     source.SHA256,
			SourceArtifact:   source,
		}
		expanded = append(expanded, rowTask)
	}
	return expanded, nil
}

func (c *Coordinator) readStructuredWorksetSource(task TaskDef) ([]byte, ArtifactRef, error) {
	spec := task.FanOut
	if strings.TrimSpace(spec.SourceArtifact.TaskID) != "" || strings.TrimSpace(spec.SourceArtifact.Artifact) != "" {
		if c == nil || c.session == nil {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact requires a coordinator workspace")
		}
		logicalTaskID := strings.TrimSpace(spec.SourceArtifact.TaskID)
		runtimeTaskID, resolveErr := c.resolveTaskReference(logicalTaskID)
		if resolveErr != nil {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact producer task %q: %w", logicalTaskID, resolveErr)
		}
		result := c.GetTaskResult(runtimeTaskID)
		if result == nil {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact producer task %q has no typed result", logicalTaskID)
		}
		ref, resolveErr := resolveTaskResultArtifact(result, spec.SourceArtifact.Artifact)
		if resolveErr != nil {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact %q in task %q: %w", spec.SourceArtifact.Artifact, spec.SourceArtifact.TaskID, resolveErr)
		}
		if ref.TaskID != "" && ref.TaskID != runtimeTaskID {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact %q has mismatched producer task", ref.ID)
		}
		if err := c.validateCurrentProducerArtifactOccurrence(ref, runtimeTaskID, result); err != nil {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact %q has invalid current producer occurrence: %w", ref.ID, err)
		}
		currentOccurrence := ref
		store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
		if err != nil {
			return nil, ArtifactRef{}, err
		}
		sourceID := ref.ID
		ref, err = resolveWorksetArtifactContent(context.Background(), store, ref)
		if err != nil {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact %q failed integrity verification: %w", sourceID, err)
		}
		ref = artifactContentWithCurrentOccurrence(ref, currentOccurrence)
		reader, err := store.Open(context.Background(), ref.ID)
		if err != nil {
			return nil, ArtifactRef{}, err
		}
		data, readErr := io.ReadAll(io.LimitReader(reader, maxWorksetBytes+1))
		_ = reader.Close()
		if readErr != nil {
			return nil, ArtifactRef{}, readErr
		}
		if len(data) > maxWorksetBytes {
			return nil, ArtifactRef{}, fmt.Errorf("source artifact %q exceeds %d bytes", ref.ID, maxWorksetBytes)
		}
		return data, ref, nil
	}
	workspace := ""
	if c != nil && c.session != nil {
		workspace = c.session.Workspace
	}
	absSource, err := resolveWorkspaceRelativePath(workspace, strings.TrimSpace(spec.Source))
	if err != nil {
		return nil, ArtifactRef{}, err
	}
	data, err := os.ReadFile(absSource)
	if err != nil {
		return nil, ArtifactRef{}, err
	}
	if len(data) > maxWorksetBytes {
		return nil, ArtifactRef{}, fmt.Errorf("source %q exceeds %d bytes", spec.Source, maxWorksetBytes)
	}
	digest := sha256.Sum256(data)
	ref := ArtifactRef{ID: "sha256-" + hex.EncodeToString(digest[:]), Path: spec.Source, SHA256: hex.EncodeToString(digest[:]), ByteSize: int64(len(data)), Bytes: int64(len(data))}
	if c != nil && c.session != nil {
		store, storeErr := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
		if storeErr != nil {
			return nil, ArtifactRef{}, storeErr
		}
		putResult, putErr := store.Put(context.Background(), PutArtifactRequest{SourcePath: spec.Source, Path: spec.Source, Kind: "workset_manifest", RunID: c.executionRunID, TaskID: task.ID})
		if putErr != nil {
			return nil, ArtifactRef{}, fmt.Errorf("snapshot workset source: %w", putErr)
		}
		ref = putResult.ArtifactRef
	}
	return data, ref, nil
}

func normalizeStructuredWorkset(data []byte) (WorksetManifest, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return WorksetManifest{}, fmt.Errorf("workset manifest must not be empty")
	}
	if len(trimmed) > 0 && trimmed[0] == '{' {
		return decodeWorksetManifest(trimmed)
	}
	lines := strings.Split(string(data), "\n")
	var header []string
	var rows [][]string
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(strings.TrimSuffix(line, "\r"), "\t")
		if header == nil {
			fields[0] = strings.TrimPrefix(fields[0], "#")
			header = fields
			continue
		}
		rows = append(rows, fields)
	}
	if len(header) == 0 {
		return WorksetManifest{}, fmt.Errorf("workset TSV has no header row")
	}
	items := make([]WorksetItem, 0, len(rows))
	for index, row := range rows {
		if len(row) != len(header) {
			return WorksetManifest{}, fmt.Errorf("workset TSV row %d has %d field(s), want %d", index+1, len(row), len(header))
		}
		bindings := make(map[string]string, len(header))
		for column, name := range header {
			bindings[name] = row[column]
		}
		items = append(items, WorksetItem{Key: row[0], Bindings: bindings})
	}
	manifest := WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: items}
	if err := validateWorksetManifest(manifest); err != nil {
		return WorksetManifest{}, err
	}
	return manifest, nil
}

func (c *Coordinator) validateWorksetInputs(manifest WorksetManifest, producerTaskReference string) (WorksetManifest, error) {
	if c == nil || c.session == nil {
		return manifest, nil
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return WorksetManifest{}, err
	}
	producerTaskID := ""
	var producerResult *TaskResult
	if strings.TrimSpace(producerTaskReference) != "" {
		producerTaskID, err = c.resolveTaskReference(producerTaskReference)
		if err != nil {
			return WorksetManifest{}, fmt.Errorf("workset input producer task %q: %w", producerTaskReference, err)
		}
		producerResult = c.GetTaskResult(producerTaskID)
		if producerResult == nil {
			return WorksetManifest{}, fmt.Errorf("workset input producer task %q has no typed result", producerTaskReference)
		}
	}
	for itemIndex := range manifest.Items {
		for inputIndex := range manifest.Items[itemIndex].Inputs {
			input := manifest.Items[itemIndex].Inputs[inputIndex]
			if producerResult != nil {
				declaredRef, declared := artifactRefByID(producerResult, input.ID)
				if !declared {
					return WorksetManifest{}, fmt.Errorf("workset item %q input %q was not declared by producer task %q", manifest.Items[itemIndex].Key, input.ID, producerTaskReference)
				}
				if occurrenceErr := c.validateCurrentProducerArtifactOccurrence(declaredRef, producerTaskID, producerResult); occurrenceErr != nil {
					return WorksetManifest{}, fmt.Errorf("workset item %q input %q declaration has invalid current producer occurrence: %w", manifest.Items[itemIndex].Key, input.ID, occurrenceErr)
				}
				if occurrenceErr := validateSuppliedArtifactOccurrence(input, declaredRef); occurrenceErr != nil {
					return WorksetManifest{}, fmt.Errorf("workset item %q input %q has mismatched producer occurrence: %w", manifest.Items[itemIndex].Key, input.ID, occurrenceErr)
				}
			}
			resolved, resolveErr := store.Resolve(context.Background(), input)
			if resolveErr != nil {
				return WorksetManifest{}, fmt.Errorf("workset item %q input %q failed integrity verification: %w", manifest.Items[itemIndex].Key, input.ID, resolveErr)
			}
			if producerResult != nil {
				declaredRef, _ := artifactRefByID(producerResult, input.ID)
				resolved = artifactContentWithCurrentOccurrence(resolved, declaredRef)
			}
			manifest.Items[itemIndex].Inputs[inputIndex] = resolved
		}
	}
	return manifest, nil
}

// validateCurrentProducerArtifactOccurrence accepts only the occurrence that
// the current producer typed result committed. Content-addressed metadata may
// be resolved separately, but missing provenance is never repaired from
// coordinator state.
func (c *Coordinator) validateCurrentProducerArtifactOccurrence(ref ArtifactRef, producerTaskID string, result *TaskResult) error {
	if c == nil || result == nil {
		return fmt.Errorf("producer typed result is missing")
	}
	if strings.TrimSpace(ref.ID) == "" {
		return fmt.Errorf("artifact id is missing")
	}
	if strings.TrimSpace(result.TaskID) == "" || result.TaskID != producerTaskID {
		return fmt.Errorf("typed result task %q does not match producer task %q", result.TaskID, producerTaskID)
	}
	if strings.TrimSpace(ref.RunID) == "" || strings.TrimSpace(c.executionRunID) == "" || ref.RunID != c.executionRunID {
		return fmt.Errorf("belongs to run %q, want current run %q", ref.RunID, c.executionRunID)
	}
	if strings.TrimSpace(ref.TaskID) == "" || ref.TaskID != producerTaskID {
		return fmt.Errorf("belongs to task %q, want %q", ref.TaskID, producerTaskID)
	}
	if result.Attempt <= 0 || ref.Attempt <= 0 || ref.Attempt != result.Attempt {
		return fmt.Errorf("belongs to attempt %d, want current attempt %d", ref.Attempt, result.Attempt)
	}
	if strings.TrimSpace(result.Agent) == "" || strings.TrimSpace(ref.Agent) == "" || ref.Agent != result.Agent {
		return fmt.Errorf("belongs to agent %q, want current agent %q", ref.Agent, result.Agent)
	}
	return nil
}

func validateSuppliedArtifactOccurrence(supplied, current ArtifactRef) error {
	if supplied.RunID != "" && supplied.RunID != current.RunID {
		return fmt.Errorf("belongs to run %q, want %q", supplied.RunID, current.RunID)
	}
	if supplied.TaskID != "" && supplied.TaskID != current.TaskID {
		return fmt.Errorf("belongs to task %q, want %q", supplied.TaskID, current.TaskID)
	}
	if supplied.Attempt != 0 && supplied.Attempt != current.Attempt {
		return fmt.Errorf("belongs to attempt %d, want %d", supplied.Attempt, current.Attempt)
	}
	if supplied.Agent != "" && supplied.Agent != current.Agent {
		return fmt.Errorf("belongs to agent %q, want %q", supplied.Agent, current.Agent)
	}
	return nil
}

// artifactContentWithCurrentOccurrence combines immutable content metadata
// resolved from the store with the already-validated occurrence committed by
// the producer typed result. It does not manufacture any provenance.
func artifactContentWithCurrentOccurrence(content, occurrence ArtifactRef) ArtifactRef {
	content.RunID = occurrence.RunID
	content.TaskID = occurrence.TaskID
	content.Attempt = occurrence.Attempt
	content.Agent = occurrence.Agent
	content.Provider = occurrence.Provider
	content.ToolCallID = occurrence.ToolCallID
	content.CreatedAt = occurrence.CreatedAt
	return content
}

func buildWorksetReceipts(tasks []TaskDef, ids []string, runID string) (map[string]*WorksetExpansionReceipt, error) {
	receipts := make(map[string]*WorksetExpansionReceipt)
	for index, task := range tasks {
		binding := task.WorksetBinding
		if binding == nil {
			continue
		}
		if strings.TrimSpace(binding.WorksetID) == "" || strings.TrimSpace(binding.ItemKey) == "" {
			return nil, fmt.Errorf("task %d has incomplete workset binding", index)
		}
		if strings.TrimSpace(binding.SourceArtifactID) == "" || strings.TrimSpace(binding.SourceSHA256) == "" {
			return nil, fmt.Errorf("task %d has incomplete workset source reference", index)
		}
		if err := validateWorksetSourceOccurrence(binding.SourceArtifact, binding.SourceArtifactID, binding.SourceSHA256, false); err != nil {
			return nil, fmt.Errorf("task %d has invalid workset source occurrence: %w", index, err)
		}
		receipt := receipts[binding.WorksetID]
		if receipt == nil {
			receipt = &WorksetExpansionReceipt{WorksetID: binding.WorksetID, RunID: runID, ParentTaskID: binding.ParentTaskID, SourceArtifactID: binding.SourceArtifactID, SourceSHA256: binding.SourceSHA256, SourceArtifact: binding.SourceArtifact, Children: make(map[string]string)}
			receipts[binding.WorksetID] = receipt
		} else if receipt.RunID != runID || receipt.ParentTaskID != binding.ParentTaskID || receipt.SourceArtifactID != binding.SourceArtifactID || receipt.SourceSHA256 != binding.SourceSHA256 || !sameArtifactOccurrence(receipt.SourceArtifact, binding.SourceArtifact) {
			return nil, fmt.Errorf("workset %q has inconsistent runtime binding metadata", binding.WorksetID)
		}
		if _, exists := receipt.Children[binding.ItemKey]; exists {
			return nil, fmt.Errorf("workset %q has duplicate child key %q", binding.WorksetID, binding.ItemKey)
		}
		receipt.Children[binding.ItemKey] = ids[index]
		receipt.ItemCount++
	}
	for _, receipt := range receipts {
		if receipt.ItemCount == 0 || len(receipt.Children) != receipt.ItemCount {
			return nil, fmt.Errorf("workset %q has inconsistent child mapping", receipt.WorksetID)
		}
	}
	return receipts, nil
}
