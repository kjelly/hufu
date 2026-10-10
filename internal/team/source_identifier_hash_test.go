package team

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/utils"
)

func TestAcceptedPayloadHashSurvivesLaterSourceIdentifierDiscovery(t *testing.T) {
	compiled, ref := compiledReviewContract(t, true)
	payload, err := validateStructuredResultPayload(compiled, ref, []byte(`{"verdict":"approve","findings":[{"summary":"DOCUMENT_API_KEY handling is fail-closed; it cannot be verified within this workset"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	result := &TaskResult{TaskID: "1", Agent: "worker", Status: TaskResultStatusSuccess, Summary: "review complete", Source: "submitted", StructuredPayload: payload}
	item := &TodoItem{ID: "1", Agent: "worker", Status: TaskDone, TypedResult: result}
	keyBefore := taskTransitionEventKey(item)
	workspace := t.TempDir()
	session := NewSession()
	session.Tasks = []*TodoItem{item}
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	// Later source/model text resembles an assignment but its value names a
	// public environment variable. It must not become a process-wide secret.
	utils.RedactSecrets("api_key: DOCUMENT_API_KEY")
	utils.RedactSecrets("api_key: `DOCUMENT_API_KEY`")
	utils.RedactSecrets("Missing credential: fail-closed before any network call")
	if keyAfter := taskTransitionEventKey(item); keyAfter != keyBefore {
		t.Fatalf("unchanged task acquired a different event idempotency key: %q != %q", keyAfter, keyBefore)
	}
	assertPayload := func(got *ResultPayload) {
		t.Helper()
		if got == nil || got.SHA256 != payload.SHA256 {
			t.Fatalf("accepted payload changed after source discovery: %#v", got)
		}
		value, err := decodeResultContractJSON(got.Value)
		if err != nil {
			t.Fatal(err)
		}
		canonical, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if string(canonical) != string(payload.Value) {
			t.Fatal("accepted payload content changed after source discovery")
		}
		sum := sha256.Sum256(canonical)
		if hex.EncodeToString(sum[:]) != got.SHA256 {
			t.Fatal("persisted payload no longer matches its hash")
		}
	}
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	loaded := LoadSession(workspace)
	if loaded == nil || len(loaded.Tasks) != 1 {
		t.Fatalf("reload: session=%#v", loaded)
	}
	assertPayload(loaded.Tasks[0].TypedResult.StructuredPayload)

	store, err := NewEventStore(workspace, "run-source-discovery", "session-source-discovery")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	encoded, err := json.Marshal(map[string]any{"typed_result": result})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.AppendPersistedContext(t.Context(), RunEvent{Type: "payload_hash_probe", Actor: "worker", Payload: encoded})
	if err != nil {
		t.Fatal(err)
	}
	var event struct {
		Result TaskResult `json:"typed_result"`
	}
	if err := json.Unmarshal(persisted.Payload, &event); err != nil {
		t.Fatal(err)
	}
	assertPayload(event.Result.StructuredPayload)
	if err := store.VerifyHashChain(); err != nil {
		t.Fatal(err)
	}

	journal, err := openTaskJournal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = journal.Close() }()
	c := &Coordinator{taskTracker: NewTaskTracker(), journal: journal, executionRunID: "run-source-discovery"}
	tracked := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "review"}})[0]
	tracked.Status, tracked.TypedResult = TaskDone, result
	c.recordTerminalTypedTaskResult(tracked.ID)
	data, err := os.ReadFile(taskJournalPath(workspace))
	if err != nil {
		t.Fatal(err)
	}
	var record journalRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.TypedResult == nil {
		t.Fatal("journal omitted the typed result")
	}
	assertPayload(record.TypedResult.StructuredPayload)
	if strings.Contains(string(data), "[REDACTED]") {
		t.Fatal("source identifier was redacted in the journal")
	}
}
