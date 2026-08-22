package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const (
	WorksetSchemaVersion = 1
	maxWorksetItems      = 2000
	maxWorksetKeyBytes   = 256
	maxWorksetValueBytes = 4096
	maxWorksetBytes      = 4 * 1024 * 1024
)

// WorksetManifest is the provider-neutral normalized source for a fan-out.
// The raw manifest is kept in an artifact; only this bounded metadata crosses
// the task/event boundary.
type WorksetManifest struct {
	SchemaVersion int           `json:"schema_version" yaml:"schema-version"`
	Items         []WorksetItem `json:"items" yaml:"items"`
}

type WorksetItem struct {
	Key      string            `json:"key" yaml:"key"`
	Bindings map[string]string `json:"bindings,omitempty" yaml:"bindings,omitempty"`
	Inputs   []ArtifactRef     `json:"inputs,omitempty" yaml:"inputs,omitempty"`
}

// WorksetExpansionReceipt is immutable evidence that one source generation
// produced exactly this child mapping. Manifest content is intentionally not
// embedded in the receipt.
type WorksetExpansionReceipt struct {
	WorksetID        string            `json:"workset_id"`
	RunID            string            `json:"run_id,omitempty"`
	ParentTaskID     string            `json:"parent_task_id"`
	SourceArtifactID string            `json:"source_artifact_id"`
	SourceSHA256     string            `json:"source_sha256"`
	SourceArtifact   ArtifactRef       `json:"source_artifact"`
	ItemCount        int               `json:"item_count"`
	ItemKeysSHA256   string            `json:"item_keys_sha256"`
	Children         map[string]string `json:"children"`
}

// WorksetBinding is copied onto every child and is immutable after task
// creation. Inputs remain opaque artifact references; a path is never used as
// the binding identity.
type WorksetBinding struct {
	WorksetID        string            `json:"workset_id"`
	ParentTaskID     string            `json:"parent_task_id,omitempty"`
	ItemKey          string            `json:"item_key"`
	Bindings         map[string]string `json:"bindings,omitempty"`
	Inputs           []ArtifactRef     `json:"inputs,omitempty"`
	SourceArtifactID string            `json:"source_artifact_id"`
	SourceSHA256     string            `json:"source_sha256"`
	SourceArtifact   ArtifactRef       `json:"source_artifact"`
}

// WorksetGroupState is the bounded, content-free projection used by
// verification, resume, reports, and machine-readable output. It never
// embeds the manifest or worker output.
type WorksetGroupState struct {
	WorksetID        string `json:"workset_id"`
	ParentTaskID     string `json:"parent_task_id"`
	SourceArtifactID string `json:"source_artifact_id"`
	SourceSHA256     string `json:"source_sha256"`
	Expected         int    `json:"expected"`
	Completed        int    `json:"completed"`
	Verified         int    `json:"verified"`
	Failed           int    `json:"failed"`
	State            string `json:"state"`
}

func cloneWorksetBinding(src *WorksetBinding) *WorksetBinding {
	if src == nil {
		return nil
	}
	copyBinding := *src
	if src.Bindings != nil {
		copyBinding.Bindings = make(map[string]string, len(src.Bindings))
		for key, value := range src.Bindings {
			copyBinding.Bindings[key] = value
		}
	}
	copyBinding.Inputs = append([]ArtifactRef(nil), src.Inputs...)
	return &copyBinding
}

func cloneWorksetReceipt(src *WorksetExpansionReceipt) *WorksetExpansionReceipt {
	if src == nil {
		return nil
	}
	copyReceipt := *src
	copyReceipt.Children = make(map[string]string, len(src.Children))
	for key, taskID := range src.Children {
		copyReceipt.Children[key] = taskID
	}
	return &copyReceipt
}

// collectWorksetReceipts indexes every visible receipt by its immutable
// WorksetID. Equivalent replay copies are deduplicated; a differing copy is
// retained as a conflict instead of replacing the first observation.
func collectWorksetReceipts(receipts []*WorksetExpansionReceipt) (map[string]*WorksetExpansionReceipt, map[string]struct{}) {
	indexed := make(map[string]*WorksetExpansionReceipt)
	conflicts := make(map[string]struct{})
	for _, receipt := range receipts {
		if receipt == nil {
			continue
		}
		if existing, ok := indexed[receipt.WorksetID]; ok {
			if !equivalentWorksetReceipts(existing, receipt) {
				conflicts[receipt.WorksetID] = struct{}{}
			}
			continue
		}
		indexed[receipt.WorksetID] = cloneWorksetReceipt(receipt)
	}
	return indexed, conflicts
}

func worksetReceiptConflictError(conflicts map[string]struct{}) error {
	if len(conflicts) == 0 {
		return nil
	}
	ids := make([]string, 0, len(conflicts))
	for id := range conflicts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return fmt.Errorf("conflicting workset receipts for workset %q", ids[0])
}

func equivalentWorksetReceipts(first, second *WorksetExpansionReceipt) bool {
	if first == nil || second == nil {
		return first == second
	}
	if first.WorksetID != second.WorksetID ||
		first.RunID != second.RunID ||
		normalizeTaskReferenceID(first.ParentTaskID) != normalizeTaskReferenceID(second.ParentTaskID) ||
		first.SourceArtifactID != second.SourceArtifactID ||
		first.SourceSHA256 != second.SourceSHA256 ||
		!sameArtifactOccurrence(first.SourceArtifact, second.SourceArtifact) ||
		first.ItemCount != second.ItemCount ||
		first.ItemKeysSHA256 != second.ItemKeysSHA256 ||
		len(first.Children) != len(second.Children) {
		return false
	}
	for key, childID := range first.Children {
		secondChildID, exists := second.Children[key]
		if !exists || secondChildID != childID {
			return false
		}
	}
	return true
}

func validateWorksetManifest(manifest WorksetManifest) error {
	if manifest.SchemaVersion != WorksetSchemaVersion {
		return fmt.Errorf("unsupported workset schema_version %d (want %d)", manifest.SchemaVersion, WorksetSchemaVersion)
	}
	if len(manifest.Items) == 0 {
		return fmt.Errorf("workset manifest must contain at least one item")
	}
	if len(manifest.Items) > maxWorksetItems {
		return fmt.Errorf("workset manifest contains %d items, maximum is %d", len(manifest.Items), maxWorksetItems)
	}
	seen := make(map[string]struct{}, len(manifest.Items))
	for index, item := range manifest.Items {
		key := strings.TrimSpace(item.Key)
		if key == "" {
			return fmt.Errorf("workset item %d has an empty key", index)
		}
		if len(key) > maxWorksetKeyBytes {
			return fmt.Errorf("workset item %d key exceeds %d bytes", index, maxWorksetKeyBytes)
		}
		if _, exists := seen[key]; exists {
			return fmt.Errorf("workset item key %q is duplicated", key)
		}
		seen[key] = struct{}{}
		for name, value := range item.Bindings {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("workset item %q has an empty binding name", key)
			}
			if len(name) > maxWorksetKeyBytes || len(value) > maxWorksetValueBytes {
				return fmt.Errorf("workset item %q has an oversized binding", key)
			}
		}
		for inputIndex, input := range item.Inputs {
			if strings.TrimSpace(input.ID) == "" || strings.TrimSpace(input.SHA256) == "" {
				return fmt.Errorf("workset item %q input %d must have an opaque id and sha256", key, inputIndex)
			}
		}
	}
	return nil
}

func decodeWorksetManifest(data []byte) (WorksetManifest, error) {
	if len(data) > maxWorksetBytes {
		return WorksetManifest{}, fmt.Errorf("workset manifest exceeds %d bytes", maxWorksetBytes)
	}
	var manifest WorksetManifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&manifest); err != nil {
		return WorksetManifest{}, fmt.Errorf("decode workset manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return WorksetManifest{}, fmt.Errorf("workset manifest must contain exactly one JSON document")
	}
	if err := validateWorksetManifest(manifest); err != nil {
		return WorksetManifest{}, err
	}
	return manifest, nil
}

func worksetItemKeysDigest(items []WorksetItem) string {
	h := sha256.New()
	for _, item := range items {
		_, _ = h.Write([]byte(item.Key))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func worksetID(sourceID, sourceSHA string, items []WorksetItem) string {
	h := sha256.New()
	_, _ = h.Write([]byte(sourceID))
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write([]byte(sourceSHA))
	_, _ = h.Write([]byte{'\n'})
	_, _ = h.Write([]byte(worksetItemKeysDigest(items)))
	return "workset-" + hex.EncodeToString(h.Sum(nil))[:24]
}
