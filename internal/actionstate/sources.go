package actionstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// sourceCheckpoint is runtime-owned storage data. Adapters consume the versioned
// projection below instead of parsing receipts, recovery grants or store paths.
type sourceCheckpoint struct {
	RuntimeWorkspace string             `json:"runtime_workspace"`
	InputID          string             `json:"active_run_input_snapshot_id"`
	Tasks            []checkpointSource `json:"tasks"`
}

type checkpointSource struct {
	ID           string         `json:"ID"`
	Status       string         `json:"Status"`
	ContractID   string         `json:"contract_id"`
	InputID      string         `json:"run_input_snapshot_id"`
	EvidenceFrom []string       `json:"evidence_from"`
	Binding      jsontext.Value `json:"workset_binding"`
	Result       jsontext.Value `json:"typed_result"`
	Receipt      *struct {
		RunID            string `json:"run_id"`
		TaskID           string `json:"task_id"`
		Attempt          int    `json:"attempt"`
		ProducerID       string `json:"producer_id"`
		ModelExecutionID string `json:"model_execution_id"`
		TranscriptRef    string `json:"transcript_ref"`
		ActionID         string `json:"action_invocation_id"`
		OutputsHash      string `json:"runtime_outputs_hash"`
	} `json:"execution_receipt"`
	Verification *struct {
		ExitCode    *int   `json:"exit_code"`
		Fingerprint string `json:"fingerprint"`
		TimedOut    bool   `json:"timed_out"`
	} `json:"VerifyResult"`
}

type sourceProjection struct {
	ID              string         `json:"ID"`
	Status          string         `json:"Status"`
	ContractID      string         `json:"contract_id"`
	EvidenceFrom    []string       `json:"evidence_from"`
	Binding         jsontext.Value `json:"workset_binding,omitempty"`
	Result          jsontext.Value `json:"typed_result,omitempty"`
	VerifiedResult  bool           `json:"verified_result"`
	VerifiedOutputs bool           `json:"verified_outputs"`
	ResultError     string         `json:"result_error,omitempty"`
	OutputsError    string         `json:"outputs_error,omitempty"`
	Workspace       string         `json:"workspace,omitempty"`
}

type sourceResult struct {
	Status      string         `json:"status"`
	Outputs     jsontext.Value `json:"runtime_outputs"`
	OutputsHash string         `json:"runtime_outputs_hash"`
	Payload     *struct {
		Value jsontext.Value `json:"value"`
		Hash  string         `json:"sha256"`
	} `json:"structured_payload"`
}

// ProjectSources produces the v1 read-only source view from a checkpoint and
// runtime-admitted origins. Invalid and pending tasks remain visible with false
// verification flags, so consumers cannot mistake missing evidence for no work.
// Callers must obtain origins from the runtime, never from checkpoint contents.
func ProjectSources(checkpoint []byte, runID string, origins map[string]string) ([]byte, error) {
	return projectSources(checkpoint, runID, origins, "")
}

func projectSources(checkpoint []byte, runID string, origins map[string]string, root string) ([]byte, error) {
	var current sourceCheckpoint
	if err := json.Unmarshal(checkpoint, &current); err != nil {
		return nil, err
	}
	if runID == "" {
		return nil, errors.New("missing source run identity")
	}
	view := struct {
		Version int                `json:"version"`
		InputID string             `json:"input_id"`
		Tasks   []sourceProjection `json:"tasks"`
	}{Version: 1, InputID: current.InputID, Tasks: []sourceProjection{}}
	seen := make(map[string]bool)
	for _, item := range current.Tasks {
		if item.ID == "" || seen[item.ID] {
			return nil, errors.New("duplicate or empty source task identity")
		}
		seen[item.ID] = true
		projection := sourceProjection{ID: item.ID, Status: item.Status, ContractID: item.ContractID,
			EvidenceFrom: item.EvidenceFrom, Binding: item.Binding, Result: item.Result}
		var result sourceResult
		resultErr := json.Unmarshal(item.Result, &result)
		if resultErr == nil {
			resultErr = verifySourceIdentity(item, current.InputID, runID, origins)
		}
		modelErr, actionErr := resultErr, resultErr
		if resultErr == nil {
			modelErr = verifyModelSource(item, result)
			actionErr = verifyActionSource(item, result, current.InputID)
		}
		projection.VerifiedResult = modelErr == nil
		projection.VerifiedOutputs = actionErr == nil
		if projection.VerifiedOutputs && root != "" {
			projection.Workspace = filepath.Join(root, "runtime", "runs", item.Receipt.RunID, "actions", item.Receipt.ActionID)
		}
		if modelErr != nil {
			projection.ResultError = modelErr.Error()
		}
		if actionErr != nil {
			projection.OutputsError = actionErr.Error()
		}
		view.Tasks = append(view.Tasks, projection)
	}
	return json.Marshal(view)
}

func verifySourceIdentity(item checkpointSource, inputID, runID string, origins map[string]string) error {
	receipt := item.Receipt
	if item.Status != "done" || receipt == nil || receipt.RunID == "" || receipt.TaskID != item.ID || receipt.Attempt <= 0 {
		return errors.New("source lacks a current accepted receipt")
	}
	if receipt.RunID != runID && (origins[item.ID] != receipt.RunID || inputID == "" || item.InputID != inputID) {
		return errors.New("source lacks a current accepted origin")
	}
	// Current-run legacy model handoffs may omit an input snapshot. Recovered
	// sources and all native outputs require an exact nonempty snapshot below.
	if item.InputID != "" && item.InputID != inputID {
		return errors.New("source lacks a current accepted input snapshot")
	}
	verifier := item.Verification
	if verifier == nil || verifier.ExitCode == nil || *verifier.ExitCode != 0 || verifier.TimedOut || verifier.Fingerprint == "" {
		return errors.New("source lacks a current accepted verification")
	}
	return nil
}

func verifyModelSource(item checkpointSource, result sourceResult) error {
	receipt := item.Receipt
	if (result.Status != "success" && result.Status != "completed_with_gaps") || result.Payload == nil || receipt.ProducerID == "" || receipt.ModelExecutionID == "" || receipt.TranscriptRef == "" {
		return errors.New("source lacks a current accepted model result")
	}
	canonical, err := canonicalSourceJSON(result.Payload.Value, false)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != result.Payload.Hash {
		return errors.New("source payload hash mismatch")
	}
	return nil
}

func verifyActionSource(item checkpointSource, result sourceResult, inputID string) error {
	if result.Status != "success" || inputID == "" || item.InputID != inputID || !sourcePathComponent(item.Receipt.RunID) || !sourcePathComponent(item.Receipt.ActionID) || len(result.Outputs) == 0 || string(result.Outputs) == "null" {
		return errors.New("source lacks verified native outputs")
	}
	canonical, err := canonicalSourceJSON(result.Outputs, true)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(canonical)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	if result.OutputsHash != digest || item.Receipt.OutputsHash != digest {
		return errors.New("source runtime outputs differ from its receipt")
	}
	return nil
}

// Canonical source hashes must preserve the existing encoding/json storage
// representation (including number spelling and HTML escaping), without a
// second redaction pass. JSON v2 checks duplicate names before legacy encoding.
func canonicalSourceJSON(raw []byte, indent bool) ([]byte, error) {
	var checked jsontext.Value
	if err := json.Unmarshal(raw, &checked); err != nil {
		return nil, err
	}
	var value any
	decoder := jsonv1.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if indent {
		return jsonv1.MarshalIndent(value, "", "  ")
	}
	return jsonv1.Marshal(value)
}

// ActionSources reads and verifies the current action's source view. Paths,
// occurrence identity and recovery grants come only from the runtime-owned
// invocation environment. No raw receipt or verification record is exposed.
func ActionSources(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	workspace, runID, actionID := os.Getenv("HUFU_WORKSPACE"), os.Getenv("HUFU_RUN_ID"), os.Getenv("HUFU_ACTION_INVOCATION_ID")
	root, runtimeRoot, err := actionCheckpointRoot(workspace, runID, actionID)
	if err != nil {
		return nil, err
	}
	data, err := sessionCheckpoint(ctx, root)
	if err != nil {
		return nil, err
	}
	var current sourceCheckpoint
	if err := json.Unmarshal(data, &current); err != nil {
		return nil, err
	}
	if current.RuntimeWorkspace != "" && current.RuntimeWorkspace != runtimeRoot {
		return nil, errors.New("runtime workspace does not match checkpoint")
	}
	matched := false
	for _, item := range current.Tasks {
		if item.ID == os.Getenv("HUFU_TASK_ID") && item.Status == "in_progress" && current.InputID != "" && item.InputID == current.InputID {
			matched = true
		}
	}
	if !matched {
		return nil, errors.New("current action occurrence is absent from checkpoint")
	}
	encoded := os.Getenv("HUFU_RECOVERED_TASK_RUNS")
	origins := map[string]string{}
	if len(encoded) > 64<<10 {
		return nil, errors.New("runtime recovery origins exceed 64 KiB")
	}
	if encoded != "" {
		if err := json.Unmarshal([]byte(encoded), &origins); err != nil {
			return nil, fmt.Errorf("invalid runtime recovery origins: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return projectSources(data, runID, origins, root)
}

func sourcePathComponent(value string) bool {
	return value != "" && value != "." && value != ".." && filepath.Base(value) == value
}

func actionCheckpointRoot(workspace, runID, actionID string) (string, string, error) {
	if !filepath.IsAbs(workspace) || !sourcePathComponent(runID) || !sourcePathComponent(actionID) {
		return "", "", errors.New("missing runtime action identity")
	}
	root := filepath.Clean(workspace)
	for range 5 {
		root = filepath.Dir(root)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if filepath.Join(runtimeRoot, "runs", runID, "actions", actionID) != workspace {
		return "", "", errors.New("unsupported action workspace layout")
	}
	return root, runtimeRoot, nil
}
