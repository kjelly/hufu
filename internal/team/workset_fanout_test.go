package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

// writeWorksetManifest stores raw manifest content at manifest.json in the
// workspace and returns its path.
func writeWorksetManifest(t *testing.T, workspace string, content string) string {
	t.Helper()
	path := filepath.Join(workspace, "manifest.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStructuredFanOutUsesValidatedManifestBindings(t *testing.T) {
	c := newTestCoordinator(t)
	c.executionRunID = "run-1"
	manifestPath := writeWorksetManifest(t, c.session.Workspace, `{"schema_version":1,"items":[{"key":"alpha","bindings":{"name":"alpha","input":"a.txt"},"touched_paths":["internal/team/z.go","internal/team/a.go","internal/team/z.go"]},{"key":"beta","bindings":{"name":"beta","input":"b.txt"},"touched_paths":["cmd/hufu/main.go"]}]}`)
	parent := TaskDef{ID: "prepare", Agent: "worker", InvariantVerification: InvariantVerificationReport, FanOut: &FanOutSpec{Source: "manifest.json", GoalTemplate: "process {name} from {input}"}}
	tasks, err := c.expandFanOutTasks([]TaskDef{parent})
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Goal != "process alpha from a.txt" || tasks[1].WorksetBinding == nil {
		t.Fatalf("structured fan-out = %#v", tasks)
	}
	if tasks[0].WorksetBinding.ItemKey != "alpha" || tasks[1].WorksetBinding.ItemKey != "beta" || tasks[0].WorksetBinding.SourceArtifactID == "" {
		t.Fatalf("missing immutable workset binding: %#v", tasks)
	}
	if got := tasks[0].WorksetBinding.TouchedPaths; len(got) != 2 || got[0] != "internal/team/a.go" || got[1] != "internal/team/z.go" {
		t.Fatalf("touched paths were not normalized and frozen: %#v", got)
	}
	if tasks[0].InvariantVerification != InvariantVerificationReport || tasks[1].InvariantVerification != InvariantVerificationReport {
		t.Fatalf("fan-out children lost invariant verification mode: %#v", tasks)
	}
	if tasks[0].WorksetBinding.WorksetID != tasks[1].WorksetBinding.WorksetID {
		t.Fatal("children do not share one workset identity")
	}
	// The committed workset is frozen at expansion time; rewriting the
	// source afterwards must not change the receipt.
	originalSource := tasks[0].WorksetBinding.SourceSHA256
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"items":[{"key":"changed","bindings":{"name":"changed","input":"c.txt"}}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	receipts, err := buildWorksetReceipts(tasks, []string{"child-1", "child-2"}, "run-1")
	if err != nil || receipts[tasks[0].WorksetBinding.WorksetID].SourceSHA256 != originalSource {
		t.Fatalf("committed workset changed after source mutation: receipts=%#v err=%v", receipts, err)
	}
}

func TestTaskGoalContractCarriesArtifactFanOutBeforeExpansion(t *testing.T) {
	session := &TeamSession{
		Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{BindTaskGoalContracts: true}},
		ContractTasks: []TaskDef{{
			ID: "review-workset", Agent: "reviewer",
			FanOut: &FanOutSpec{SourceArtifact: FactRef{TaskID: "producer", Artifact: "manifest"}, GoalTemplate: "review {key}"},
		}},
	}
	bound, _, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "reviewer", Goal: "review workset", ContractID: "review-workset"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(bound) != 1 || bound[0].FanOut == nil || bound[0].FanOut.SourceArtifact.Artifact != "manifest" {
		t.Fatalf("static contract did not carry fan-out: %#v", bound)
	}
}

func TestTaskGoalInvariantContractPropagatesThroughFanOutAndSubmission(t *testing.T) {
	workspace := t.TempDir()
	writeWorksetManifest(t, workspace, `{"schema_version":1,"items":[{"key":"one","bindings":{"key":"one"}}]}`)
	session := &TeamSession{
		Workspace: workspace,
		Config:    agent.TeamConfig{Delegation: agent.DelegationPolicy{BindTaskGoalContracts: true}},
		ContractTasks: []TaskDef{{
			ID: "review-workset", Agent: "reviewer",
			InvariantVerification: InvariantVerificationReport,
			Execution:             ExecutionContract{RequiresResult: true, ToolSequence: []string{"submit_result"}},
			FanOut:                &FanOutSpec{Source: "manifest.json", GoalTemplate: "review {key}"},
		}},
	}
	bound, effective, err := CompileTaskGoalContracts(session, []TaskDef{{Agent: "reviewer", Goal: "review workset", ContractID: "review-workset"}})
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{session: session, executionRunID: "run-1"}
	expanded, err := c.expandFanOutTasks(bound)
	if err != nil {
		t.Fatal(err)
	}
	if len(expanded) != 1 || len(effective) != 1 || effective[0].InvariantVerification != InvariantVerificationReport {
		t.Fatalf("compiled fan-out invariant projection = expanded=%#v effective=%#v", expanded, effective)
	}
	item := todoItemFromSpec(TodoSpec{
		PlanTaskID: expanded[0].ID, Agent: expanded[0].Agent, Desc: expanded[0].Goal,
		InvariantVerification: expanded[0].InvariantVerification,
		ContractID:            expanded[0].ContractID, ContractHash: expanded[0].ContractHash, ContractRevision: expanded[0].ContractRevision,
		Execution: expanded[0].Execution, WorksetBinding: expanded[0].WorksetBinding,
	}, "task-1")
	submission := taskResultSubmissionContractForTask(taskDefFromTodoItem(item))
	if item.InvariantVerification != InvariantVerificationReport || submission.InvariantVerification != InvariantVerificationReport {
		t.Fatalf("durable submission contract lost invariant policy: item=%#v submission=%#v", item, submission)
	}
	if _, err := decodeSubmitResultInput([]byte(`{"status":"success","summary":"checked","invariant_assessments":[{"invariant_id":"safe","status":"preserved","summary":"preserved"}]}`), submission); err != nil {
		t.Fatalf("report-mode submission was rejected: %v", err)
	}
}

func TestStructuredFanOutRejectsInvalidManifestAtomically(t *testing.T) {
	for name, content := range map[string]string{
		"zero-byte source": "",
		"duplicate key":    `{"schema_version":1,"items":[{"key":"same","bindings":{"x":"1"}},{"key":"same","bindings":{"x":"2"}}]}`,
		"empty key":        `{"schema_version":1,"items":[{"key":"","bindings":{"x":"1"}}]}`,
		"bad schema":       `{"schema_version":99,"items":[{"key":"one","bindings":{"x":"1"}}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestCoordinator(t)
			writeWorksetManifest(t, c.session.Workspace, content)
			if expanded, err := c.expandFanOutTasks([]TaskDef{
				{Agent: "other", Goal: "must not commit"},
				{Agent: "worker", FanOut: &FanOutSpec{Source: "manifest.json", GoalTemplate: "{x}"}},
			}); err == nil || expanded != nil {
				t.Fatalf("invalid manifest was partially expanded: expanded=%#v err=%v", expanded, err)
			}
		})
	}
}

func TestStructuredFanOutAcceptsProducerManifestV2(t *testing.T) {
	manifest, err := decodeWorksetManifest([]byte(`{"schema_version":2,"scope":{"satisfied":true},"items":[{"key":"one","bindings":{"x":"1"}}]}`))
	if err != nil {
		t.Fatalf("decode v2 workset manifest: %v", err)
	}
	if manifest.SchemaVersion != 2 || len(manifest.Items) != 1 || manifest.Items[0].Key != "one" {
		t.Fatalf("decoded manifest = %#v", manifest)
	}
}

func TestStructuredFanOutResolvesOpaqueSourceArtifactAndInputIntegrity(t *testing.T) {
	c := newTestCoordinator(t)
	c.executionRunID = "run-1"
	workspace := c.session.Workspace
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	producer := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "producer", Agent: "producer", Desc: "produce manifest"}})[0]
	producer.Status = TaskDone

	// Every item carries its own opaque input so each child must keep the
	// binding of its own position, not the first or last one.
	keys := []string{"alpha", "beta", "gamma"}
	inputs := make([]ArtifactRef, 0, len(keys))
	manifest := WorksetManifest{SchemaVersion: 1}
	for _, key := range keys {
		input, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte("input " + key), Path: "inputs/" + key + ".txt", Kind: "input", RunID: "run-1", TaskID: producer.ID, Attempt: 1, Agent: "producer"})
		if err != nil {
			t.Fatal(err)
		}
		inputs = append(inputs, input.ArtifactRef)
		manifest.Items = append(manifest.Items, WorksetItem{Key: key, Bindings: map[string]string{"name": key}, Inputs: []ArtifactRef{input.ArtifactRef}})
	}
	manifestBytes, _ := json.Marshal(manifest)
	manifestRef, err := store.Put(context.Background(), PutArtifactRequest{Content: manifestBytes, Path: "manifests/workset.json", Kind: "workset_manifest", RunID: "run-1", TaskID: producer.ID, Attempt: 1, Agent: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	c.taskResults = map[string]*TaskResult{producer.ID: {
		TaskID: producer.ID, Attempt: 1, Agent: "producer", Status: TaskResultStatusSuccess,
		Artifacts: append([]ArtifactRef{manifestRef.ArtifactRef}, inputs...),
	}}
	tasks, err := c.expandFanOutTasks([]TaskDef{{ID: "producer", Agent: "worker", FanOut: &FanOutSpec{
		SourceArtifact: FactRef{TaskID: "producer", Artifact: manifestRef.ID}, GoalTemplate: "process {name}",
	}}})
	if err != nil || len(tasks) != len(keys) {
		t.Fatalf("opaque source artifact expansion = %#v err=%v", tasks, err)
	}
	for i, task := range tasks {
		if task.Goal != "process "+keys[i] || task.WorksetBinding == nil || task.WorksetBinding.ItemKey != keys[i] {
			t.Fatalf("child %d = %#v, want key %q", i, task, keys[i])
		}
		if len(task.WorksetBinding.Inputs) != 1 || task.WorksetBinding.Inputs[0].ID != inputs[i].ID {
			t.Fatalf("child %d lost its opaque input binding: %#v", i, task.WorksetBinding)
		}
	}

	// The receipts built from this expansion feed the group gate: one partial
	// child among otherwise verified children fails workset_complete.
	specs := make([]TodoSpec, 0, len(tasks))
	for _, task := range tasks {
		specs = append(specs, TodoSpec{PlanTaskID: task.ID, Agent: task.Agent, Desc: task.Goal, WorksetBinding: cloneWorksetBinding(task.WorksetBinding)})
	}
	children := c.taskTracker.TodoList().AddBatch(specs)
	ids := make([]string, len(children))
	for i, child := range children {
		ids[i] = child.ID
		child.Status = TaskDone
		child.VerifyResult = &VerificationResult{ExitCode: 0}
		status := TaskResultStatusSuccess
		if i == 1 {
			status = TaskResultStatusPartial
		}
		child.TypedResult = &TaskResult{TaskID: child.ID, Status: status, Summary: "processed", Source: "submitted"}
	}
	receipts, err := buildWorksetReceipts(tasks, ids, c.executionRunID)
	if err != nil {
		t.Fatal(err)
	}
	children[0].WorksetReceipt = receipts[tasks[0].WorksetBinding.WorksetID]
	result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{
		Type: VerifyWorksetComplete, WorksetSourceTask: "producer", WorksetRequireTerminal: true, WorksetRequireVerified: true,
	})
	if err == nil || result == nil || result.ExitCode == 0 {
		t.Fatalf("workset_complete accepted a partial child: result=%#v err=%v", result, err)
	}
}

func TestWorksetReceiptBindsEveryChildAndReplays(t *testing.T) {
	source := ArtifactRef{ID: "artifact-1", SHA256: strings.Repeat("a", 64), RunID: "run-1", TaskID: "prepare", Attempt: 1, Agent: "producer"}
	tasks := []TaskDef{
		{WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: "a", SourceArtifactID: source.ID, SourceSHA256: source.SHA256, SourceArtifact: source}},
		{WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: "b", SourceArtifactID: source.ID, SourceSHA256: source.SHA256, SourceArtifact: source}},
	}
	receipts, err := buildWorksetReceipts(tasks, []string{"11", "12"}, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	receipt := receipts["workset-1"]
	if receipt == nil || receipt.ItemCount != 2 || receipt.Children["a"] != "11" || receipt.Children["b"] != "12" {
		t.Fatalf("receipt = %#v", receipt)
	}
	tasks[0].WorksetBinding.TouchedPaths = []string{"internal/team/a.go"}
	raw, _ := json.Marshal(map[string]any{"id": "11", "status": "pending", "workset_binding": tasks[0].WorksetBinding, "workset_receipt": receipt})
	projected := ReduceToTodoList([]RunEvent{{Type: "task_created", TaskID: "11", Payload: raw}})
	if len(projected) != 1 || projected[0].WorksetBinding == nil || projected[0].WorksetReceipt == nil || projected[0].WorksetReceipt.Children["b"] != "12" || len(projected[0].WorksetBinding.TouchedPaths) != 1 || projected[0].WorksetBinding.TouchedPaths[0] != "internal/team/a.go" {
		t.Fatalf("replayed workset projection = %#v", projected)
	}
}

func TestStructuredWorksetRejectsInvalidTouchedPath(t *testing.T) {
	for _, touchedPath := range []string{"*", "../escape.go", `/absolute.go`, `internal\\team\\file.go`} {
		manifestBytes, err := json.Marshal(WorksetManifest{SchemaVersion: WorksetSchemaVersion, Items: []WorksetItem{{Key: "one", TouchedPaths: []string{touchedPath}}}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeWorksetManifest(manifestBytes); err == nil {
			t.Fatalf("invalid touched path %q was accepted", touchedPath)
		}
	}
}

// Legacy workspace-relative TSV fan-out still expands (parity with the
// artifact-backed path is covered by workset_legacy_compare_test.go) but the
// policy lint must flag it as deprecated without blocking the team.
func TestLegacyTSVFanOutEmitsWarningOnlyDeprecation(t *testing.T) {
	session := &TeamSession{ContractTasks: []TaskDef{{
		ID:     "legacy",
		FanOut: &FanOutSpec{Source: "inputs/items.tsv", GoalTemplate: "process {item}"},
	}}}
	var deprecated bool
	for _, finding := range ValidateTeamPolicyContracts(session) {
		if finding.Code == FindingLegacyFanOutDeprecated {
			deprecated = finding.Severity == FindingSeverityWarning
		}
	}
	if !deprecated {
		t.Fatal("legacy TSV fan-out did not emit a warning-only deprecation finding")
	}
}
