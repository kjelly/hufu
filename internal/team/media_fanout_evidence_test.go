package team

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/execution"
)

func TestNativeMediaFanoutCompletionRequiresEveryAcceptedLeaf(t *testing.T) {
	c := newTestCoordinator(t)
	primary, next := execution.ExecutionTarget{Backend: "fixture", Model: "vision"}, execution.ExecutionTarget{Backend: "fixture", Model: "next"}
	base := TodoItem{ID: "task", Agent: "worker", Retries: 1, MediaInputDigest: "ordered-image-digest",
		ExecutionTarget: primary, ExecutionTopology: []execution.ExecutionTarget{primary, next}}
	for index, target := range base.ExecutionTopology {
		id, err := nativeMediaFanoutExecutionID(&base, "run", index)
		if err != nil {
			t.Fatal(err)
		}
		base.ExecutionReceipts = append(base.ExecutionReceipts, ExecutionReceipt{
			RunID: "run", TaskID: base.ID, Attempt: 1, OccurrenceAttempt: 2, ExecutionTarget: target,
			ModelExecutionID: id, MediaInputDigest: base.MediaInputDigest,
			ExitCode: new(0), FinishedAt: time.Unix(1, 0), HandoffState: ResultHandoffSubmitted,
			AcceptedResult: &TaskResult{TaskID: base.ID, Attempt: 1, Agent: base.Agent, Source: "submitted",
				Status: TaskResultStatusSuccess, Summary: "original observations", Details: "complete semantic result",
				Facts: map[string]any{"observations": []any{"first", "second"}}},
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(*TodoItem)
		valid  bool
	}{
		{"all accepted", func(*TodoItem) {}, true},
		{"missing leaf", func(item *TodoItem) { item.ExecutionReceipts = item.ExecutionReceipts[:1] }, false},
		{"failed receipt", func(item *TodoItem) { item.ExecutionReceipts[1].ExitCode = new(1) }, false},
		{"unfinished receipt", func(item *TodoItem) { item.ExecutionReceipts[1].FinishedAt = time.Time{} }, false},
		{"nonterminal handoff only", func(item *TodoItem) {
			item.ExecutionReceipts[1].SubmittedResult = item.ExecutionReceipts[1].AcceptedResult
			item.ExecutionReceipts[1].AcceptedResult = nil
		}, false},
		{"blocked", func(item *TodoItem) { item.ExecutionReceipts[1].AcceptedResult.Status = TaskResultStatusBlocked }, false},
		{"stale occurrence", func(item *TodoItem) { item.ExecutionReceipts[1].OccurrenceAttempt-- }, false},
		{"wrong target", func(item *TodoItem) { item.ExecutionReceipts[1].ExecutionTarget = primary }, false},
		{"wrong images", func(item *TodoItem) { item.ExecutionReceipts[1].MediaInputDigest = "other" }, false},
		{"wrong task", func(item *TodoItem) { item.ExecutionReceipts[1].AcceptedResult.TaskID = "other" }, false},
		{"wrong attempt", func(item *TodoItem) { item.ExecutionReceipts[1].AcceptedResult.Attempt++ }, false},
		{"wrong agent", func(item *TodoItem) { item.ExecutionReceipts[1].AcceptedResult.Agent = "other" }, false},
		{"promoted prose", func(item *TodoItem) { item.ExecutionReceipts[1].AcceptedResult.Source = "free_text" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := base
			item.ExecutionReceipts = make([]ExecutionReceipt, len(base.ExecutionReceipts))
			for index := range base.ExecutionReceipts {
				item.ExecutionReceipts[index] = cloneExecutionReceipt(&base.ExecutionReceipts[index])
			}
			test.mutate(&item)
			if got := c.nativeMediaAcceptedWork(&item); got != test.valid {
				t.Fatalf("accepted work = %v, want %v", got, test.valid)
			}
		})
	}
	// Mutating a clone, a rejected handoff, or a later attempt cannot replace
	// the original accepted semantic payload during persistence and replay.
	if base.ExecutionReceipts[1].AcceptedResult.Status != TaskResultStatusSuccess {
		t.Fatal("receipt clone aliases the accepted result")
	}
	payload, err := json.Marshal(map[string]any{"id": base.ID, "status": TaskDone,
		"execution_receipts": base.ExecutionReceipts})
	if err != nil {
		t.Fatal(err)
	}
	replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCompleted), TaskID: base.ID, Payload: payload}})
	if len(replayed) != 1 {
		t.Fatal("canonical replay lost its task")
	}
	want, err := json.Marshal(base.ExecutionReceipts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(replayed[0].ExecutionReceipts)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("canonical replay changed accepted leaf evidence")
	}
}

func TestNativeMediaAcceptedLeafPayloadRequiresOriginalContractAndHash(t *testing.T) {
	ref := ResultContractRef{ID: "observations.schema.json", SchemaSHA256: "frozen-schema", RequireStructured: true}
	value := []byte(`{"observation":"original finding"}`)
	digest := sha256.Sum256(value)
	payload := ResultPayload{Contract: ref, Value: value, SHA256: hex.EncodeToString(digest[:])}
	base := ExecutionReceipt{ResultContractID: ref.ID, ResultContractSHA256: ref.SchemaSHA256,
		ResultPayloadSHA256: payload.SHA256, ResultValidation: ResultValidationValid,
		AcceptedResult: &TaskResult{StructuredPayload: &payload}}
	for _, test := range []struct {
		name   string
		mutate func(*TodoItem, *ExecutionReceipt)
		valid  bool
	}{
		{"original", func(*TodoItem, *ExecutionReceipt) {}, true},
		{"missing required payload", func(_ *TodoItem, r *ExecutionReceipt) { r.AcceptedResult.StructuredPayload = nil }, false},
		{"unbound payload", func(item *TodoItem, _ *ExecutionReceipt) { item.ResultContract = nil }, false},
		{"changed value", func(_ *TodoItem, r *ExecutionReceipt) { r.AcceptedResult.StructuredPayload.Value = []byte(`{}`) }, false},
		{"changed result hash", func(_ *TodoItem, r *ExecutionReceipt) { r.AcceptedResult.StructuredPayload.SHA256 = "other" }, false},
		{"changed receipt hash", func(_ *TodoItem, r *ExecutionReceipt) { r.ResultPayloadSHA256 = "other" }, false},
		{"different schema", func(item *TodoItem, _ *ExecutionReceipt) { item.ResultContract.SchemaSHA256 = "other" }, false},
		{"not validated", func(_ *TodoItem, r *ExecutionReceipt) { r.ResultValidation = "" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := TodoItem{ResultContract: ref.clone()}
			receipt := cloneExecutionReceipt(&base)
			test.mutate(&item, &receipt)
			if got := nativeMediaAcceptedPayload(&item, receipt); got != test.valid {
				t.Fatalf("accepted payload = %v, want %v", got, test.valid)
			}
		})
	}
}
