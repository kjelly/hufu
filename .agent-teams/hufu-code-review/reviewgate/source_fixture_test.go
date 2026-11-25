package main

// Raw checkpoint fixtures exist only in native tests. Every gate assertion
// crosses the real runtime source projection before invoking domain checks.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/actionstate"
	"github.com/kjelly/hufu/internal/team"
)

type task struct {
	ID           string
	Status       string
	ContractID   string   `json:"contract_id"`
	EvidenceFrom []string `json:"evidence_from"`
	SnapshotID   string   `json:"run_input_snapshot_id"`
	Binding      *struct {
		Bindings     map[string]string   `json:"bindings"`
		TouchedPaths []string            `json:"touched_paths"`
		Inputs       []sourceArtifactRef `json:"inputs"`
	} `json:"workset_binding"`
	Receipt *struct {
		RunID              string `json:"run_id"`
		TaskID             string `json:"task_id"`
		Attempt            int    `json:"attempt"`
		ProducerID         string `json:"producer_id"`
		ModelExecutionID   string `json:"model_execution_id"`
		TranscriptRef      string `json:"transcript_ref"`
		ExitCode           *int   `json:"exit_code,omitempty"`
		ActionID           string `json:"action_invocation_id"`
		RuntimeOutputsHash string `json:"runtime_outputs_hash"`
	} `json:"execution_receipt"`
	VerifyResult *struct {
		ExitCode    *int   `json:"exit_code"`
		Fingerprint string `json:"fingerprint"`
		TimedOut    bool   `json:"timed_out"`
	}
	Result *struct {
		RuntimeOutputs     map[string]json.RawMessage `json:"runtime_outputs"`
		RuntimeOutputsHash string                     `json:"runtime_outputs_hash"`
		Status             string                     `json:"status"`
		Summary            string                     `json:"summary,omitempty"`
		Findings           []finding                  `json:"findings"`
		Invariants         *struct {
			Assessments []invariantAssessment `json:"assessments"`
		} `json:"invariant_verification"`
		FilesRead []struct {
			Path string `json:"path"`
		} `json:"files_read"`
		Payload *struct {
			Value json.RawMessage `json:"value"`
			Hash  string          `json:"sha256"`
		} `json:"structured_payload"`
	} `json:"typed_result"`
}

type session struct {
	RuntimeWorkspace  string            `json:"runtime_workspace"`
	SnapshotID        string            `json:"active_run_input_snapshot_id"`
	Tasks             []task            `json:"tasks"`
	RecoveredTaskRuns map[string]string `json:"-"`
}

func projectedFixture(current session, runID string) (sourceSession, error) {
	data, err := json.Marshal(current)
	if err != nil {
		return sourceSession{}, err
	}
	data, err = actionstate.ProjectSources(data, runID, current.RecoveredTaskRuns)
	if err != nil {
		return sourceSession{}, err
	}
	var view sourceSession
	err = json.Unmarshal(data, &view)
	return view, err
}
func projectedTask(item task) sourceTask {
	// Domain-only helpers do not require an accepted result; preserve all fields.
	data, _ := json.Marshal(item)
	var view sourceTask
	_ = json.Unmarshal(data, &view)
	return view
}
func projectedTasks(tasks map[string]task) map[string]sourceTask {
	view := make(map[string]sourceTask)
	for id, item := range tasks {
		view[id] = projectedTask(item)
	}
	return view
}
func fixtureVerify(current session, runID string) (string, string, error) {
	view, err := projectedFixture(current, runID)
	if err != nil {
		return "", "", err
	}
	return verify(view)
}
func fixtureCollectReviewHandoffs(current session, runID string) (handoffInventory, error) {
	view, err := projectedFixture(current, runID)
	if err != nil {
		return handoffInventory{}, err
	}
	return collectReviewHandoffs(view)
}
func fixtureVerifyHandoffInventory(current session, runID string) error {
	view, err := projectedFixture(current, runID)
	if err != nil {
		return err
	}
	return verifyHandoffInventory(view)
}
func fixtureCollectSupplementRequests(current session, runID string) (supplementRequests, error) {
	view, err := projectedFixture(current, runID)
	if err != nil {
		return supplementRequests{}, err
	}
	return collectSupplementRequests(view)
}
func fixtureCollectSupplementOutcomes(current session, runID string) ([]supplementOutcome, error) {
	view, err := projectedFixture(current, runID)
	if err != nil {
		return nil, err
	}
	return collectSupplementOutcomes(view)
}
func fixtureVerifyReview(item task, r record) error { return verifyReview(projectedTask(item), r) }
func fixtureVerifyInvariants(item task, r record) error {
	return verifyInvariants(projectedTask(item), r)
}
func fixtureVerifyCritique(item task, r record, tasks map[string]task, values map[string]payload, records map[string]record, critiqued map[string]bool) (string, bool, error) {
	return verifyCritique(projectedTask(item), r, projectedTasks(tasks), values, records, critiqued)
}
func fixtureVerifyGaps(report payload, records map[string]record, tasks map[string]task) error {
	return verifyGaps(report, records, projectedTasks(tasks))
}

func checkpointRoot(workspace, runID, actionID string) (string, string, error) {
	root := filepath.Clean(workspace)
	for range 5 {
		root = filepath.Dir(root)
	}
	runtimeRoot := filepath.Join(root, "runtime")
	if filepath.Join(runtimeRoot, "runs", runID, "actions", actionID) != workspace {
		return "", "", fmt.Errorf("invalid fixture workspace")
	}
	return root, runtimeRoot, nil
}

func sealFixtureOutputs(t *testing.T, item *task) {
	t.Helper()
	data, err := json.Marshal(item.Result.RuntimeOutputs)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		t.Fatal(err)
	}
	_, digest, err := team.CanonicalizeRuntimeOutputs(values)
	if err != nil {
		t.Fatal(err)
	}
	item.Receipt.ActionID = "action-" + item.ID
	item.Receipt.RuntimeOutputsHash = digest
	item.Result.RuntimeOutputsHash = digest
}
