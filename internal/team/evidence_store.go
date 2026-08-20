package team

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// PutArtifactRequest describes an immutable artifact to persist. Content is
// preferred for generated data; SourcePath supports artifacts already on disk.
type PutArtifactRequest struct {
	ID, Kind, Path, Description, MediaType string
	Content                                []byte
	SourcePath                             string
	RunID, TaskID, Agent, ToolCallID       string
	Attempt                                int
	CreatedAt                              time.Time
}

type ArtifactStore interface {
	Put(context.Context, PutArtifactRequest) (ArtifactRef, error)
	Verify(context.Context, ArtifactRef) error
	Open(context.Context, string) (io.ReadCloser, error)
	ListByTask(context.Context, string) ([]ArtifactRef, error)
}

// FileArtifactStore stores content and metadata under workspace/logs/artifacts.
// Artifact data is content-addressed and never overwritten.
type FileArtifactStore struct {
	root      string
	sourceDir string
}

func NewFileArtifactStore(workspace, sourceDir string) (*FileArtifactStore, error) {
	if workspace == "" {
		return nil, fmt.Errorf("artifact store: empty workspace")
	}
	root := filepath.Join(workspace, logsDir, "artifacts")
	for _, dir := range []string{filepath.Join(root, "data"), filepath.Join(root, "meta")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("artifact store mkdir: %w", err)
		}
	}
	return &FileArtifactStore{root: root, sourceDir: sourceDir}, nil
}

func (s *FileArtifactStore) Put(_ context.Context, req PutArtifactRequest) (ArtifactRef, error) {
	if s == nil {
		return ArtifactRef{}, fmt.Errorf("artifact store is nil")
	}
	data := req.Content
	if req.SourcePath != "" {
		path, err := resolveArtifactSourcePath(s.sourceDir, req.SourcePath)
		if err != nil {
			return ArtifactRef{}, err
		}
		data, err = os.ReadFile(path)
		if err != nil {
			return ArtifactRef{}, fmt.Errorf("read artifact %q: %w", req.SourcePath, err)
		}
	}
	if req.Path == "" && req.SourcePath != "" {
		req.Path = req.SourcePath
	}
	if req.Path == "" {
		return ArtifactRef{}, fmt.Errorf("artifact path is required")
	}
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	id := req.ID
	if id == "" {
		id = "sha256-" + digest
	}
	if !validArtifactID(id) {
		return ArtifactRef{}, fmt.Errorf("invalid artifact id %q", id)
	}
	ref := ArtifactRef{ID: id, Kind: req.Kind, Path: req.Path, Description: req.Description,
		Type: req.Kind, SHA256: digest, Bytes: int64(len(data)), ByteSize: int64(len(data)),
		MediaType: req.MediaType, RunID: req.RunID, TaskID: req.TaskID, Attempt: req.Attempt,
		Agent: req.Agent, ToolCallID: req.ToolCallID, CreatedAt: req.CreatedAt}
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = time.Now().UTC()
	}
	dataPath := filepath.Join(s.root, "data", id)
	if existing, err := os.ReadFile(dataPath); err == nil {
		if !bytes.Equal(existing, data) {
			return ArtifactRef{}, fmt.Errorf("artifact %q already exists with different content", id)
		}
	} else if os.IsNotExist(err) {
		f, createErr := os.OpenFile(dataPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if createErr != nil {
			return ArtifactRef{}, fmt.Errorf("create artifact %q: %w", id, createErr)
		}
		if _, writeErr := f.Write(data); writeErr != nil {
			_ = f.Close()
			return ArtifactRef{}, fmt.Errorf("write artifact %q: %w", id, writeErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return ArtifactRef{}, fmt.Errorf("close artifact %q: %w", id, closeErr)
		}
	} else {
		return ArtifactRef{}, fmt.Errorf("inspect artifact %q: %w", id, err)
	}
	metaPath := filepath.Join(s.root, "meta", id+".json")
	encoded, err := json.Marshal(ref)
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("marshal artifact metadata: %w", err)
	}
	if existing, readErr := os.ReadFile(metaPath); readErr == nil {
		var old ArtifactRef
		if json.Unmarshal(existing, &old) != nil || old.SHA256 != ref.SHA256 || old.ByteSize != ref.ByteSize {
			return ArtifactRef{}, fmt.Errorf("artifact %q metadata conflicts", id)
		}
	} else if os.IsNotExist(readErr) {
		f, createErr := os.OpenFile(metaPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if createErr != nil {
			return ArtifactRef{}, fmt.Errorf("create artifact metadata %q: %w", id, createErr)
		}
		if _, writeErr := f.Write(encoded); writeErr != nil {
			_ = f.Close()
			return ArtifactRef{}, fmt.Errorf("write artifact metadata %q: %w", id, writeErr)
		}
		if closeErr := f.Close(); closeErr != nil {
			return ArtifactRef{}, closeErr
		}
	} else {
		return ArtifactRef{}, fmt.Errorf("inspect artifact metadata %q: %w", id, readErr)
	}
	return ref, nil
}

func (s *FileArtifactStore) Verify(_ context.Context, ref ArtifactRef) error {
	if s == nil || !validArtifactID(ref.ID) {
		return fmt.Errorf("artifact reference has no id")
	}
	b, err := os.ReadFile(filepath.Join(s.root, "data", ref.ID))
	if err != nil {
		return fmt.Errorf("read artifact %q: %w", ref.ID, err)
	}
	h := sha256.Sum256(b)
	if hex.EncodeToString(h[:]) != ref.SHA256 || int64(len(b)) != ref.ByteSize {
		return fmt.Errorf("artifact %q hash or size mismatch", ref.ID)
	}
	return nil
}

func (s *FileArtifactStore) Open(_ context.Context, id string) (io.ReadCloser, error) {
	if s == nil || !validArtifactID(id) {
		return nil, fmt.Errorf("artifact id is required")
	}
	return os.Open(filepath.Join(s.root, "data", id))
}

// Get resolves immutable artifact metadata by opaque ID. Callers that already
// hold an artifact reference must use this instead of interpreting the ID as a
// filesystem path.
func (s *FileArtifactStore) Get(_ context.Context, id string) (ArtifactRef, error) {
	if s == nil || !validArtifactID(id) {
		return ArtifactRef{}, fmt.Errorf("artifact id is required")
	}
	b, err := os.ReadFile(filepath.Join(s.root, "meta", id+".json"))
	if err != nil {
		return ArtifactRef{}, fmt.Errorf("read artifact metadata %q: %w", id, err)
	}
	var ref ArtifactRef
	if err := json.Unmarshal(b, &ref); err != nil {
		return ArtifactRef{}, fmt.Errorf("decode artifact metadata %q: %w", id, err)
	}
	if ref.ID != id {
		return ArtifactRef{}, fmt.Errorf("artifact metadata %q has mismatched id %q", id, ref.ID)
	}
	return ref, nil
}

func validArtifactID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`)
}

func resolveArtifactSourcePath(sourceRoot, sourcePath string) (string, error) {
	if sourceRoot == "" {
		return "", fmt.Errorf("artifact source root is required")
	}
	root, err := filepath.Abs(sourceRoot)
	if err != nil {
		return "", fmt.Errorf("resolve artifact source root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve artifact source root: %w", err)
	}
	candidate := sourcePath
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve artifact source %q: %w", sourcePath, err)
	}
	// Resolve the file itself so a symlink inside the allowed root cannot point
	// outside it. The file must exist before it can become evidence.
	candidate, err = filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("resolve artifact source %q: %w", sourcePath, err)
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("artifact source %q escapes source directory", sourcePath)
	}
	return candidate, nil
}

func (s *FileArtifactStore) ListByTask(_ context.Context, taskID string) ([]ArtifactRef, error) {
	if s == nil {
		return nil, fmt.Errorf("artifact store is nil")
	}
	entries, err := os.ReadDir(filepath.Join(s.root, "meta"))
	if err != nil {
		return nil, err
	}
	refs := make([]ArtifactRef, 0)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		b, readErr := os.ReadFile(filepath.Join(s.root, "meta", entry.Name()))
		if readErr != nil {
			return nil, readErr
		}
		var ref ArtifactRef
		if err := json.Unmarshal(b, &ref); err != nil {
			return nil, err
		}
		if ref.TaskID == taskID {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs, nil
}

type EvidenceRequirement struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Required  bool     `json:"required"`
	Validator string   `json:"validator"`
	Inputs    []string `json:"inputs,omitempty"`
	Expected  any      `json:"expected,omitempty"`
}

type EvidenceResult struct {
	RequirementID string           `json:"requirement_id"`
	Status        string           `json:"status"`
	Validator     string           `json:"validator,omitempty"`
	ArtifactRefs  []ArtifactRef    `json:"artifact_refs,omitempty"`
	Binding       *EvidenceBinding `json:"binding,omitempty"`
	Assertions    []string         `json:"assertions,omitempty"`
	CheckedAt     time.Time        `json:"checked_at"`
}

// EvidenceBinding seals the exact execution provenance behind one task
// requirement. Reports may project this binding, but may not reconstruct it
// by joining a run ID to an arbitrary receipt.
type EvidenceBinding struct {
	RunID            string   `json:"run_id"`
	TaskID           string   `json:"task_id"`
	Attempt          int      `json:"attempt"`
	ModelExecutionID string   `json:"model_execution_id"`
	ProducerID       string   `json:"producer_id"`
	TranscriptRef    string   `json:"transcript_ref"`
	ArtifactIDs      []string `json:"artifact_ids"`
}

type EvidenceManifest struct {
	SchemaVersion      int              `json:"schema_version"`
	RunID              string           `json:"run_id"`
	TaskID             string           `json:"task_id,omitempty"`
	Attempt            int              `json:"attempt,omitempty"`
	PolicyDecisionRefs []string         `json:"policy_decision_refs,omitempty"`
	ArtifactRefs       []ArtifactRef    `json:"artifact_refs,omitempty"`
	EvidenceResults    []EvidenceResult `json:"evidence_results,omitempty"`
	PreviousHash       string           `json:"previous_hash,omitempty"`
	ManifestHash       string           `json:"manifest_hash"`
	Status             string           `json:"status"`
}

func (m *EvidenceManifest) Seal() error {
	if m == nil || m.RunID == "" {
		return fmt.Errorf("evidence manifest requires run id")
	}
	m.SchemaVersion = 1
	if m.Status == "" {
		m.Status = "accepted"
	}
	copyManifest := *m
	copyManifest.ManifestHash = ""
	b, err := json.Marshal(copyManifest)
	if err != nil {
		return err
	}
	h := sha256.Sum256(append([]byte(m.PreviousHash), b...))
	m.ManifestHash = hex.EncodeToString(h[:])
	return nil
}

func (m EvidenceManifest) Verify(ctx context.Context, store ArtifactStore) error {
	if m.ManifestHash == "" {
		return fmt.Errorf("evidence manifest is unsealed")
	}
	copyManifest := m
	copyManifest.ManifestHash = ""
	b, err := json.Marshal(copyManifest)
	if err != nil {
		return err
	}
	h := sha256.Sum256(append([]byte(m.PreviousHash), b...))
	if hex.EncodeToString(h[:]) != m.ManifestHash {
		return fmt.Errorf("evidence manifest hash mismatch")
	}
	for _, ref := range m.ArtifactRefs {
		if err := store.Verify(ctx, ref); err != nil {
			return err
		}
	}
	for _, result := range m.EvidenceResults {
		if strings.HasPrefix(result.RequirementID, "task:") && strings.EqualFold(result.Status, "passed") {
			if err := verifyEvidenceBinding(m, result); err != nil {
				return err
			}
		}
		if strings.EqualFold(result.Status, "error") || (strings.EqualFold(result.Status, "failed") && strings.EqualFold(m.Status, "accepted")) {
			return fmt.Errorf("evidence requirement %q failed", result.RequirementID)
		}
	}
	return nil
}

func verifyEvidenceBinding(manifest EvidenceManifest, result EvidenceResult) error {
	if result.Binding == nil {
		return fmt.Errorf("evidence requirement %q has no sealed execution binding", result.RequirementID)
	}
	b := result.Binding
	wantTask := strings.TrimPrefix(result.RequirementID, "task:")
	if b.RunID != manifest.RunID || b.TaskID == "" || b.TaskID != wantTask || b.Attempt <= 0 || b.ModelExecutionID == "" || b.ProducerID == "" || b.TranscriptRef == "" {
		return fmt.Errorf("evidence requirement %q has conflicting execution binding", result.RequirementID)
	}
	if strings.HasPrefix(b.TranscriptRef, "sha256-") && !slices.Contains(b.ArtifactIDs, b.TranscriptRef) {
		return fmt.Errorf("evidence requirement %q transcript is outside artifact membership", result.RequirementID)
	}
	wantIDs := make([]string, 0, len(result.ArtifactRefs))
	if len(result.ArtifactRefs) == 0 {
		return fmt.Errorf("evidence requirement %q has no transcript/artifact membership", result.RequirementID)
	}
	seen := make(map[string]bool, len(result.ArtifactRefs))
	for _, ref := range result.ArtifactRefs {
		if ref.ID == "" || seen[ref.ID] {
			return fmt.Errorf("evidence requirement %q has invalid artifact membership", result.RequirementID)
		}
		seen[ref.ID] = true
		wantIDs = append(wantIDs, ref.ID)
		found := false
		for _, manifestRef := range manifest.ArtifactRefs {
			if manifestRef.ID == ref.ID && manifestRef.RunID == b.RunID && manifestRef.TaskID == b.TaskID && manifestRef.Attempt == b.Attempt && (manifestRef.Agent == "" || manifestRef.Agent == b.ProducerID) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("evidence requirement %q references artifact outside manifest", result.RequirementID)
		}
	}
	gotIDs := append([]string(nil), b.ArtifactIDs...)
	sort.Strings(wantIDs)
	sort.Strings(gotIDs)
	if !slices.Equal(wantIDs, gotIDs) {
		return fmt.Errorf("evidence requirement %q has incomplete artifact membership", result.RequirementID)
	}
	return nil
}

// VerifiedTaskBinding returns only a binding that passed the manifest's
// structural checks. It is the sole report-facing task identity accessor.
func (m EvidenceManifest) VerifiedTaskBinding(taskID string) (*EvidenceBinding, bool) {
	if taskID == "" {
		return nil, false
	}
	for _, result := range m.EvidenceResults {
		if result.RequirementID != "task:"+taskID || !strings.EqualFold(result.Status, "passed") || result.Binding == nil {
			continue
		}
		if verifyEvidenceBinding(m, result) != nil {
			return nil, false
		}
		binding := *result.Binding
		binding.ArtifactIDs = append([]string(nil), result.Binding.ArtifactIDs...)
		return &binding, true
	}
	return nil, false
}
