package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RuntimeWorksetProjection struct {
	SchemaVersion int                     `json:"schema_version"`
	RunID         string                  `json:"run_id"`
	CompletedAt   string                  `json:"completed_at"`
	Pointers      []RuntimeWorksetPointer `json:"pointers"`
}

// validateRuntimeWorksetPointer binds the derived pointer to the canonical
// sealed run result. Matching only the pointer's self-reported digest would
// allow an outsider artifact to look like a member of the completed run.
func validateRuntimeWorksetPointer(pointer RuntimeWorksetPointer, result *RunResult) error {
	if result == nil || result.EvidenceManifest == nil {
		return fmt.Errorf("runtime workset pointer has no canonical evidence manifest")
	}
	manifest := result.EvidenceManifest
	if strings.TrimSpace(manifest.ManifestHash) == "" {
		return fmt.Errorf("canonical evidence manifest is not sealed")
	}
	sealed := *manifest
	sealed.ManifestHash = ""
	sealedBytes, err := json.Marshal(sealed)
	if err != nil {
		return fmt.Errorf("encode canonical evidence manifest: %w", err)
	}
	sealDigest := sha256.Sum256(append([]byte(manifest.PreviousHash), sealedBytes...))
	if hex := fmt.Sprintf("%x", sealDigest); hex != manifest.ManifestHash {
		return fmt.Errorf("canonical evidence manifest seal is invalid")
	}
	if manifest.RunID != result.RunID || pointer.RunID != result.RunID || pointer.RunID != manifest.RunID {
		return fmt.Errorf("runtime workset pointer run_id is not bound to canonical result")
	}
	if strings.TrimSpace(pointer.ManifestArtifactID) == "" || strings.TrimSpace(pointer.ManifestSHA256) == "" {
		return fmt.Errorf("runtime workset pointer artifact identity is incomplete")
	}
	for _, ref := range manifest.ArtifactRefs {
		if ref.ID == pointer.ManifestArtifactID && ref.SHA256 == pointer.ManifestSHA256 {
			return nil
		}
	}
	return fmt.Errorf("runtime workset pointer artifact %q is outside canonical evidence manifest", pointer.ManifestArtifactID)
}

// publishRuntimeWorksetProjection publishes only generic runtime metadata for
// an action-produced workset. The historical workspace/workset path is never
// read or written by this projection.
func (c *Coordinator) publishRuntimeWorksetProjection(actionRoot, actionID string, declared []ArtifactRef, artifacts []ArtifactRef) error {
	if c == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || c.session == nil {
		return nil
	}
	var manifest ArtifactRef
	var manifestSource string
	for index, artifact := range artifacts {
		if index >= len(declared) || !isRuntimeWorksetManifest(artifact, declared[index]) {
			continue
		}
		manifest = artifact
		manifestSource = declared[index].Path
		break
	}
	if strings.TrimSpace(manifest.ID) == "" || strings.TrimSpace(manifest.SHA256) == "" || strings.TrimSpace(manifestSource) == "" {
		return nil
	}
	manifestSourcePath, err := resolveArtifactSourcePath(actionRoot, manifestSource)
	if err != nil {
		return fmt.Errorf("resolve runtime workset manifest: %w", err)
	}
	data, err := os.ReadFile(manifestSourcePath)
	if err != nil {
		return fmt.Errorf("read runtime workset manifest: %w", err)
	}
	parsed, err := decodeWorksetManifest(data)
	if err != nil {
		return fmt.Errorf("decode runtime workset manifest: %w", err)
	}
	digest := sha256.Sum256(data)
	if fmt.Sprintf("%x", digest) != strings.TrimSpace(manifest.SHA256) {
		return fmt.Errorf("runtime workset manifest sha256 does not match artifact %q", manifest.ID)
	}

	worksetDir := filepath.Join(actionRoot, "workset")
	if err := os.MkdirAll(worksetDir, 0o755); err != nil {
		return fmt.Errorf("create runtime workset directory: %w", err)
	}
	manifestPath := filepath.Join(worksetDir, "manifest.json")
	if existing, readErr := os.ReadFile(manifestPath); readErr == nil {
		if !bytes.Equal(existing, data) {
			return fmt.Errorf("runtime workset manifest already exists with different content")
		}
	} else if os.IsNotExist(readErr) {
		if err := AtomicWriteFile(manifestPath, data, 0o644); err != nil {
			return fmt.Errorf("publish runtime workset manifest: %w", err)
		}
	} else {
		return fmt.Errorf("inspect runtime workset manifest: %w", readErr)
	}

	runID := coordinatorRuntimeRunID(c)
	runtimePath, err := filepath.Rel(c.session.Workspace, worksetDir)
	if err != nil {
		return fmt.Errorf("relativize runtime workset path: %w", err)
	}
	pointer := RuntimeWorksetPointer{
		SchemaVersion:      1,
		RunID:              runID,
		ActionInvocationID: actionID,
		ManifestArtifactID: manifest.ID,
		ManifestSHA256:     manifest.SHA256,
		RuntimePath:        filepath.ToSlash(runtimePath),
		ManifestPath:       filepath.ToSlash(filepath.Join(runtimePath, "manifest.json")),
		ItemCount:          len(parsed.Items),
	}
	encoded, err := json.MarshalIndent(pointer, "", "  ")
	if err != nil {
		return fmt.Errorf("encode runtime workset pointer: %w", err)
	}
	if err := AtomicWriteFile(filepath.Join(worksetDir, "summary.json"), encoded, 0o644); err != nil {
		return fmt.Errorf("publish runtime workset summary: %w", err)
	}
	// The action-local pointer is run-scoped. A concurrent action must never
	// overwrite a workspace-level current pointer; that pointer is published
	// once, after run_finished is confirmed.
	if err := AtomicWriteFile(filepath.Join(worksetDir, "current-workset.json"), encoded, 0o644); err != nil {
		return fmt.Errorf("publish run-scoped workset pointer: %w", err)
	}
	return nil
}

func isRuntimeWorksetManifest(artifact, declared ArtifactRef) bool {
	for _, value := range []string{artifact.Kind, artifact.Role, declared.Kind, declared.Role} {
		if strings.EqualFold(strings.TrimSpace(value), "workset_manifest") {
			return true
		}
	}
	return false
}

func (c *Coordinator) publishCompletedRuntimeWorksetProjection(result *RunResult) error {
	if c == nil || c.session == nil || result == nil || strings.TrimSpace(result.RunID) == "" || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return nil
	}
	runRoot := filepath.Join(c.session.Workspace, "runtime", "runs", result.RunID)
	actionsRoot := filepath.Join(runRoot, "actions")
	entries, err := os.ReadDir(actionsRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read completed workset actions: %w", err)
	}
	projection := RuntimeWorksetProjection{SchemaVersion: 1, RunID: result.RunID, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(actionsRoot, entry.Name(), "workset", "current-workset.json")
		data, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return fmt.Errorf("read run-scoped workset pointer: %w", readErr)
		}
		var pointer RuntimeWorksetPointer
		if err := json.Unmarshal(data, &pointer); err != nil {
			return fmt.Errorf("decode run-scoped workset pointer: %w", err)
		}
		if pointer.RunID != result.RunID || pointer.ManifestSHA256 == "" || pointer.ManifestArtifactID == "" {
			return fmt.Errorf("run-scoped workset pointer has inconsistent run identity")
		}
		if err := validateRuntimeWorksetPointer(pointer, result); err != nil {
			return err
		}
		manifestPath := filepath.Join(c.session.Workspace, filepath.FromSlash(pointer.ManifestPath))
		manifestData, err := os.ReadFile(manifestPath)
		if err != nil {
			return fmt.Errorf("read run-scoped workset manifest: %w", err)
		}
		digest := sha256.Sum256(manifestData)
		if fmt.Sprintf("%x", digest) != pointer.ManifestSHA256 {
			return fmt.Errorf("run-scoped workset manifest digest mismatch")
		}
		projection.Pointers = append(projection.Pointers, pointer)
	}
	if len(projection.Pointers) == 0 {
		return nil
	}
	// Monotonicity is evaluated from completed run identities; a late older run
	// cannot replace a newer workspace pointer. The write itself is atomic.
	workspacePointer := filepath.Join(c.session.Workspace, "runtime", "current-workset.json")
	if existing, readErr := os.ReadFile(workspacePointer); readErr == nil {
		var current RuntimeWorksetProjection
		if json.Unmarshal(existing, &current) == nil && current.RunID > projection.RunID {
			return nil
		}
	}
	encoded, err := json.MarshalIndent(projection, "", "  ")
	if err != nil {
		return fmt.Errorf("encode completed workset projection: %w", err)
	}
	return AtomicWriteFile(workspacePointer, encoded, 0o644)
}

// LoadRuntimeWorksetProjection reads only the completed workspace pointer and
// verifies every referenced manifest belongs to the requested run and digest.
// A stale or malformed pointer is never returned as current-run report data.
func LoadRuntimeWorksetProjection(workspace string, canonical any) (*RuntimeWorksetProjection, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, nil
	}
	var runID string
	var result *RunResult
	switch value := canonical.(type) {
	case string:
		runID = strings.TrimSpace(value)
	case *RunResult:
		result = value
		if result != nil {
			runID = strings.TrimSpace(result.RunID)
		}
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported canonical workset identity %T", canonical)
	}
	if runID == "" {
		return nil, nil
	}
	path := filepath.Join(workspace, "runtime", "current-workset.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var projection RuntimeWorksetProjection
	if err := json.Unmarshal(data, &projection); err != nil {
		return nil, fmt.Errorf("decode completed workset projection: %w", err)
	}
	if projection.RunID != runID {
		return nil, fmt.Errorf("workset projection run_id %q does not match %q", projection.RunID, runID)
	}
	for _, pointer := range projection.Pointers {
		if pointer.RunID != runID || pointer.ManifestArtifactID == "" || pointer.ManifestSHA256 == "" {
			return nil, fmt.Errorf("workset projection contains an invalid artifact identity")
		}
		if result != nil {
			if err := validateRuntimeWorksetPointer(pointer, result); err != nil {
				return nil, err
			}
		}
		manifestPath := filepath.Join(workspace, filepath.FromSlash(pointer.ManifestPath))
		rel, relErr := filepath.Rel(workspace, manifestPath)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("workset projection escapes workspace")
		}
		manifestData, readErr := os.ReadFile(manifestPath)
		if readErr != nil {
			return nil, readErr
		}
		digest := sha256.Sum256(manifestData)
		if fmt.Sprintf("%x", digest) != pointer.ManifestSHA256 {
			return nil, fmt.Errorf("workset projection manifest digest mismatch")
		}
	}
	return &projection, nil
}
