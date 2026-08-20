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
	if err := c.validateWorksetInputs(manifest); err != nil {
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
		result := c.GetTaskResult(strings.TrimSpace(spec.SourceArtifact.TaskID))
		if result == nil {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact producer task %q has no typed result", spec.SourceArtifact.TaskID)
		}
		var ref ArtifactRef
		for _, candidate := range result.Artifacts {
			if candidate.ID == spec.SourceArtifact.Artifact || candidate.Description == spec.SourceArtifact.Artifact {
				ref = candidate
				break
			}
		}
		if ref.ID == "" {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact %q was not declared by task %q", spec.SourceArtifact.Artifact, spec.SourceArtifact.TaskID)
		}
		if ref.TaskID != "" && ref.TaskID != spec.SourceArtifact.TaskID {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact %q has mismatched producer task", ref.ID)
		}
		store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
		if err != nil {
			return nil, ArtifactRef{}, err
		}
		if err := store.Verify(context.Background(), ref); err != nil {
			return nil, ArtifactRef{}, fmt.Errorf("source_artifact %q failed integrity verification: %w", ref.ID, err)
		}
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
		stored, putErr := store.Put(context.Background(), PutArtifactRequest{SourcePath: spec.Source, Path: spec.Source, Kind: "workset_manifest", RunID: c.executionRunID, TaskID: task.ID})
		if putErr != nil {
			return nil, ArtifactRef{}, fmt.Errorf("snapshot workset source: %w", putErr)
		}
		ref = stored
	}
	return data, ref, nil
}

func normalizeStructuredWorkset(data []byte) (WorksetManifest, error) {
	trimmed := bytes.TrimSpace(data)
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

func (c *Coordinator) validateWorksetInputs(manifest WorksetManifest) error {
	if c == nil || c.session == nil {
		return nil
	}
	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return err
	}
	for _, item := range manifest.Items {
		for _, input := range item.Inputs {
			if err := store.Verify(context.Background(), input); err != nil {
				return fmt.Errorf("workset item %q input %q failed integrity verification: %w", item.Key, input.ID, err)
			}
			if c.executionRunID != "" && input.RunID != "" && input.RunID != c.executionRunID {
				return fmt.Errorf("workset item %q input %q belongs to run %q, want %q", item.Key, input.ID, input.RunID, c.executionRunID)
			}
		}
	}
	return nil
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
		receipt := receipts[binding.WorksetID]
		if receipt == nil {
			receipt = &WorksetExpansionReceipt{WorksetID: binding.WorksetID, RunID: runID, ParentTaskID: binding.ParentTaskID, SourceArtifactID: binding.SourceArtifactID, SourceSHA256: binding.SourceSHA256, Children: make(map[string]string)}
			receipts[binding.WorksetID] = receipt
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
