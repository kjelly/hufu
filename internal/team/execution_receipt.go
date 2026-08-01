package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// RepairProvenance records details of protocol repair attempts.
type RepairProvenance struct {
	Attempted       bool        `json:"attempted"`
	Success         bool        `json:"success"`
	Prompt          string      `json:"prompt,omitempty"`
	SubmittedResult *TaskResult `json:"submitted_result,omitempty"`
	Error           string      `json:"error,omitempty"`
}

// ExecutionReceipt represents the execution provenance and metadata for a single task run attempt.
type ExecutionReceipt struct {
	RunID            string            `json:"run_id"`
	TaskID           string            `json:"task_id"`
	Attempt          int               `json:"attempt"`
	StartedAt        time.Time         `json:"started_at"`
	FinishedAt       time.Time         `json:"finished_at,omitempty"`
	ExitCode         *int              `json:"exit_code,omitempty"`
	ProducerID       string            `json:"producer_id,omitempty"`
	TranscriptRef    string            `json:"transcript_ref,omitempty"`
	RepairProvenance *RepairProvenance `json:"repair_provenance,omitempty"`
	// VerifyResult holds the deliverable verification result for this attempt,
	// when one ran. Retained per-attempt so forensics can inspect the verify
	// command, exit code, stdout and stderr of each attempt even after the
	// todo-wide VerifyResult slot is cleared for the next retry (§5, §9).
	VerifyResult *VerificationResult `json:"verify_result,omitempty"`
}

// ArtifactExpectation describes an expected output artifact and its verification criteria.
type ArtifactExpectation struct {
	Name             string `json:"name" yaml:"name"`
	Locator          string `json:"locator" yaml:"locator"`
	MustBeFresh      bool   `json:"must_be_fresh,omitempty" yaml:"must-be-fresh,omitempty"`
	Required         bool   `json:"required,omitempty" yaml:"required,omitempty"`
	VerificationMode string `json:"verification_mode,omitempty" yaml:"verification-mode,omitempty"`
}

// UnmarshalJSON handles both snake_case and kebab-case keys.
func (a *ArtifactExpectation) UnmarshalJSON(data []byte) error {
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	a.Required = true // default to true if unspecified
	if v, ok := m["name"].(string); ok {
		a.Name = v
	}
	if v, ok := m["locator"].(string); ok {
		a.Locator = v
	}
	if v, ok := m["must_be_fresh"].(bool); ok {
		a.MustBeFresh = v
	}
	if v, ok := m["must-be-fresh"].(bool); ok {
		a.MustBeFresh = v
	}
	if v, ok := m["required"].(bool); ok {
		a.Required = v
	}
	if v, ok := m["verification_mode"].(string); ok {
		a.VerificationMode = v
	}
	if v, ok := m["verification-mode"].(string); ok {
		a.VerificationMode = v
	}
	return nil
}

// UnmarshalYAML handles both kebab-case and snake_case keys.
func (a *ArtifactExpectation) UnmarshalYAML(node *yaml.Node) error {
	var m map[string]interface{}
	if err := node.Decode(&m); err != nil {
		return err
	}
	a.Required = true // default to true if unspecified
	if v, ok := m["name"].(string); ok {
		a.Name = v
	}
	if v, ok := m["locator"].(string); ok {
		a.Locator = v
	}
	if v, ok := m["must_be_fresh"].(bool); ok {
		a.MustBeFresh = v
	}
	if v, ok := m["must-be-fresh"].(bool); ok {
		a.MustBeFresh = v
	}
	if v, ok := m["required"].(bool); ok {
		a.Required = v
	}
	if v, ok := m["verification_mode"].(string); ok {
		a.VerificationMode = v
	}
	if v, ok := m["verification-mode"].(string); ok {
		a.VerificationMode = v
	}
	return nil
}

// ArtifactVerifier is the core interface for objective artifact verification adapters.
// It MUST NOT invoke agents, LLMs, or open interactive terminal sessions.
type ArtifactVerifier interface {
	Verify(ctx context.Context, receipt ExecutionReceipt, expectation ArtifactExpectation) VerificationResult
}

// ArtifactProducerMeta represents optional companion metadata written alongside an artifact.
type ArtifactProducerMeta struct {
	RunID      string `json:"run_id,omitempty"`
	TaskID     string `json:"task_id,omitempty"`
	Attempt    *int   `json:"attempt,omitempty"`
	ProducerID string `json:"producer_id,omitempty"`
}

// ArtifactVerifierRegistry dispatches artifact verification based on VerificationMode or Locator.
type ArtifactVerifierRegistry struct {
	mu        sync.RWMutex
	verifiers map[string]ArtifactVerifier
	defaultV  ArtifactVerifier
}

// NewArtifactVerifierRegistry creates a registry initialized with the standard FileArtifactVerifier.
func NewArtifactVerifierRegistry(baseDir string) *ArtifactVerifierRegistry {
	r := &ArtifactVerifierRegistry{
		verifiers: make(map[string]ArtifactVerifier),
	}
	fileV := NewFileArtifactVerifier(baseDir)
	r.SetDefault(fileV)
	r.Register("file", fileV)
	r.Register("exists", fileV)
	return r
}

func (r *ArtifactVerifierRegistry) Register(mode string, v ArtifactVerifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.verifiers[mode] = v
}

func (r *ArtifactVerifierRegistry) SetDefault(v ArtifactVerifier) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.defaultV = v
}

func (r *ArtifactVerifierRegistry) Get(mode string) (ArtifactVerifier, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.verifiers[mode]
	return v, ok
}

func (r *ArtifactVerifierRegistry) Verify(ctx context.Context, receipt ExecutionReceipt, expectation ArtifactExpectation) VerificationResult {
	mode := expectation.VerificationMode
	r.mu.RLock()
	v, ok := r.verifiers[mode]
	defV := r.defaultV
	r.mu.RUnlock()

	if !ok {
		if mode == "" || mode == "file" || mode == "exists" {
			if defV != nil {
				return defV.Verify(ctx, receipt, expectation)
			}
		}
		return VerificationResult{
			Command:  fmt.Sprintf("verifier:%s", mode),
			ExitCode: 1,
			Stderr:   fmt.Sprintf("unregistered verification mode %q for artifact %q", mode, expectation.Name),
		}
	}
	return v.Verify(ctx, receipt, expectation)
}

// FileArtifactVerifier is a file-system adapter for verifying file artifacts.
type FileArtifactVerifier struct {
	baseDir string
}

func NewFileArtifactVerifier(baseDir string) *FileArtifactVerifier {
	return &FileArtifactVerifier{baseDir: baseDir}
}

func (fv *FileArtifactVerifier) Verify(ctx context.Context, receipt ExecutionReceipt, expectation ArtifactExpectation) VerificationResult {
	start := time.Now()
	cmdStr := fmt.Sprintf("file_verifier:%s", expectation.Locator)

	path := expectation.Locator
	if !filepath.IsAbs(path) && fv.baseDir != "" {
		path = filepath.Join(fv.baseDir, path)
	}

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if !expectation.Required {
				return VerificationResult{
					Command:  cmdStr,
					ExitCode: 0,
					Stdout:   fmt.Sprintf("optional artifact file %q does not exist", expectation.Locator),
					Duration: time.Since(start),
				}
			}
			return VerificationResult{
				Command:  cmdStr,
				ExitCode: 1,
				Stderr:   fmt.Sprintf("artifact file %q does not exist", expectation.Locator),
				Duration: time.Since(start),
			}
		}
		return VerificationResult{
			Command:  cmdStr,
			ExitCode: 1,
			Stderr:   fmt.Sprintf("failed to stat artifact file %q: %v", expectation.Locator, err),
			Duration: time.Since(start),
		}
	}

	// Always validate companion producer metadata identity if companion metadata file exists,
	// independently of MustBeFresh.
	metaPath := path + ".json"
	if metaData, err := os.ReadFile(metaPath); err == nil {
		var meta ArtifactProducerMeta
		if jsonErr := json.Unmarshal(metaData, &meta); jsonErr == nil {
			if meta.Attempt != nil && *meta.Attempt != receipt.Attempt {
				return VerificationResult{
					Command:  cmdStr,
					ExitCode: 1,
					Stderr:   fmt.Sprintf("artifact producer attempt %d does not match receipt attempt %d", *meta.Attempt, receipt.Attempt),
					Duration: time.Since(start),
				}
			}
			if meta.RunID != "" && meta.RunID != receipt.RunID {
				return VerificationResult{
					Command:  cmdStr,
					ExitCode: 1,
					Stderr:   fmt.Sprintf("artifact producer run_id %q does not match receipt run_id %q", meta.RunID, receipt.RunID),
					Duration: time.Since(start),
				}
			}
			if meta.TaskID != "" && meta.TaskID != receipt.TaskID {
				return VerificationResult{
					Command:  cmdStr,
					ExitCode: 1,
					Stderr:   fmt.Sprintf("artifact producer task_id %q does not match receipt task_id %q", meta.TaskID, receipt.TaskID),
					Duration: time.Since(start),
				}
			}
			if meta.ProducerID != "" && receipt.ProducerID != "" && meta.ProducerID != receipt.ProducerID {
				return VerificationResult{
					Command:  cmdStr,
					ExitCode: 1,
					Stderr:   fmt.Sprintf("artifact producer_id %q does not match receipt producer_id %q", meta.ProducerID, receipt.ProducerID),
					Duration: time.Since(start),
				}
			}
		}
	}

	// Freshness timestamp verification
	if expectation.MustBeFresh {
		modTime := info.ModTime()
		if !receipt.StartedAt.IsZero() && !modTime.After(receipt.StartedAt) {
			return VerificationResult{
				Command:  cmdStr,
				ExitCode: 1,
				Stderr:   fmt.Sprintf("artifact file %q modtime (%s) is not after receipt start time (%s)", expectation.Locator, modTime.Format(time.RFC3339Nano), receipt.StartedAt.Format(time.RFC3339Nano)),
				Duration: time.Since(start),
			}
		}
	}

	return VerificationResult{
		Command:  cmdStr,
		ExitCode: 0,
		Stdout:   fmt.Sprintf("artifact %q verified successfully", expectation.Locator),
		Duration: time.Since(start),
	}
}
