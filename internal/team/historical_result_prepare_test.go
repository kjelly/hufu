package team

import (
	"context"
	jsonv1 "encoding/json"
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func historicalGrantPreparationFixture(t *testing.T) (*Coordinator, *[]HistoricalTaskResult, *scriptedWorker) {
	t.Helper()
	worker := scriptWorker(scriptedTurn{text: "must not dispatch"})
	c := newTestCoordinator(t, withWorker(worker))
	def := c.session.Agents["worker"]
	def.Name = "Review Worker"
	c.session.Agents = map[string]*agent.AgentDef{"review worker": def, "review-worker": def,
		"observer":     {Name: "Observer", Role: "worker", Generation: agent.GenerationParams{Model: "test"}},
		"coordinator":  {Name: "Coordinator", Role: "coordinator", Generation: agent.GenerationParams{Model: "test"}},
		"orchestrator": {Name: "Orchestrator", Role: "orchestrator", Generation: agent.GenerationParams{Model: "test"}}}
	if err := c.session.SetWorkspaceScope(WorkspaceScope{Managed: true, ProjectID: "project", ContextScopeID: "project",
		ControlRoot: c.session.Workspace, SubjectRoot: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	c.projectDir = c.session.Scope.SubjectRoot
	manager, err := agent.NewProviderManager("http://127.0.0.1:1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	c.providerManager = manager
	c.maxConcurrent = 2
	c.executionPolicy, err = newExecutionPolicyState(c)
	if err != nil {
		t.Fatal(err)
	}
	rows := []HistoricalTaskResult{historicalGrantSourceFixture(t, c.session.Scope.ControlRoot, "1")}
	c.SetHistoricalTaskReader(historicalTaskReaderFunc(func(_ context.Context, scope HistoricalTaskScope) ([]HistoricalTaskResult, error) {
		if scope.Workspace != c.session.Scope.ControlRoot || scope.Team != "test" || scope.Branch != "main" {
			t.Fatalf("preflight broadened source scope: %+v", scope)
		}
		return rows, nil
	}))
	return c, &rows, worker
}

func historicalGrantSourceFixture(t *testing.T, workspace, id string) HistoricalTaskResult {
	t.Helper()
	source := RunEvent{ID: "source-" + id, SessionID: "source-session", RunID: "source-run", TaskID: id, Type: string(EventTaskCompleted)}
	raw := []byte(fmt.Sprintf(`{"task_id":%q,"agent":"Researcher","attempt":1,"status":"success","summary":"accepted analysis","details":"complete deliverable","source":"submitted","future_detail":{"count":9007199254740993},"artifacts":[{"path":"/private/opaque-artifact","sha256":"%s"}],"verification":[{"command":"check","exit_code":0,"duration":1500000000,"stdout":"all diagnostics"}]}`, id, strings.Repeat("a", 64)))
	projected, hash, err := ProjectHistoricalTaskResult(raw)
	if err != nil {
		t.Fatal(err)
	}
	return HistoricalTaskResult{ResultID: HistoricalResultID(source), Team: "test", Workspace: workspace, SessionID: source.SessionID,
		SourceBranchID: "main", RunID: source.RunID, OriginRunID: "effect-run", TaskID: id, Agent: "Researcher", Attempt: 2,
		Status: string(TaskDone), CurrentStatus: string(TaskDone), Validity: "accepted", Summary: "accepted analysis",
		Output: "RAW_OUTPUT_MUST_NOT_BE_IMPORTED", SourceEventID: source.ID, SourceHash: strings.Repeat("b", 64),
		PayloadSHA256: hash, TypedResultJSON: projected}
}

func TestHistoricalResultPreparationResolvesCanonicalRecipients(t *testing.T) {
	c, rows, worker := historicalGrantPreparationFixture(t)
	profile := c.ExecutionProfile()
	profile.DisableHistoricalMemory = true
	c.SetExecutionProfile(profile)
	*rows = append(*rows, historicalGrantSourceFixture(t, c.session.Workspace, "2"))
	first, second := (*rows)[0].ResultID, (*rows)[1].ResultID
	bindings := []string{
		"REVIEW-WORKER=" + second, "Review Worker=" + first, "worker=" + first,
	}
	for range maxHistoricalResultGrants * 2 {
		bindings = append(bindings, "review-worker="+first)
	}
	prepared, err := c.PrepareHistoricalResultGrants(t.Context(), bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.grants) != 2 || prepared.grants[0].Recipient != "review worker" || prepared.grants[0].ResultID > prepared.grants[1].ResultID || worker.Calls() != 0 {
		t.Fatalf("grants were not deduplicated/sorted by canonical recipient: %+v", prepared.grants)
	}
	for _, presentation := range prepared.presentations {
		for _, required := range []string{"Historical reference data only", "complete deliverable", "future_detail", "9007199254740993", "1500000000", "all diagnostics", "/private/opaque-artifact"} {
			if !strings.Contains(presentation, required) {
				t.Fatalf("required complete historical content missing: %s", required)
			}
		}
		if strings.Contains(presentation, "RAW_OUTPUT_MUST_NOT_BE_IMPORTED") {
			t.Fatal("raw output was imported")
		}
	}
	if len(c.taskTracker.TodoList().Items()) != 0 || c.EventStore() != nil {
		t.Fatal("read-only preparation created tasks or a writable event store")
	}
	envelope, err := prepared.envelope("consumer-session", "consumer-cycle")
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Grants[0].SourceAttempt != 2 || envelope.Grants[0].SourceDispatchAttempt != 1 {
		t.Fatal("collection occurrence replaced actual dispatch attempt")
	}
	rebuilt, err := c.rebuildHistoricalResultGrants(t.Context(), envelope)
	if err != nil || !slices.Equal(prepared.presentations, rebuilt.presentations) {
		t.Fatalf("frozen presentation did not rebuild exactly: %v", err)
	}
	envelope.Grants[0].Recipient = "changed"
	if prepared.grants[0].Recipient != "review worker" {
		t.Fatal("caller mutated the immutable preflight through an envelope")
	}
}

func TestHistoricalResultPreparationRejectsUnavailableOrInvalidSources(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*HistoricalTaskResult)
	}{
		{"other_team", func(r *HistoricalTaskResult) { r.Team = "other" }},
		{"other_workspace", func(r *HistoricalTaskResult) { r.Workspace += "-other" }},
		{"invalidated", func(r *HistoricalTaskResult) { r.Validity = "invalidated" }},
		{"not_accepted", func(r *HistoricalTaskResult) { r.Validity = "legacy_unverified" }},
		{"current_incomplete", func(r *HistoricalTaskResult) { r.CurrentStatus = string(TaskError) }},
		{"failed_source", func(r *HistoricalTaskResult) { r.Status = string(TaskError) }},
		{"missing_typed", func(r *HistoricalTaskResult) { r.TypedResultJSON = nil }},
		{"missing_payload_hash", func(r *HistoricalTaskResult) { r.PayloadSHA256 = "" }},
		{"missing_event_hash", func(r *HistoricalTaskResult) { r.SourceHash = "" }},
		{"mismatched_event", func(r *HistoricalTaskResult) { r.SourceEventID = "other-event" }},
		{"typed_identity", func(r *HistoricalTaskResult) { r.TaskID = "another-task" }},
		{"typed_not_completed", func(r *HistoricalTaskResult) {
			r.TypedResultJSON = []byte(strings.Replace(string(r.TypedResultJSON), `"status":"success"`, `"status":"partial"`, 1))
		}},
		{"missing_source", func(r *HistoricalTaskResult) { r.ResultID = "res-" + strings.Repeat("f", 64) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, rows, worker := historicalGrantPreparationFixture(t)
			id := (*rows)[0].ResultID
			test.change(&(*rows)[0])
			if prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{"worker=" + id}); err == nil || prepared != nil || worker.Calls() != 0 {
				t.Fatalf("invalid source returned partial preparation or dispatched: %+v, %v", prepared, err)
			}
		})
	}
}

func TestHistoricalResultPreparationRejectsUnsupportedScopeAndBindings(t *testing.T) {
	for _, name := range []string{"unmanaged", "disabled", "independent_root", "missing_policy", "policy_drift", "missing_reader", "duplicate_source", "coordinator", "orchestrator", "unknown", "wildcard", "prefix", "empty", "too_many", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			c, rows, worker := historicalGrantPreparationFixture(t)
			bindings := []string{"worker=" + (*rows)[0].ResultID}
			ctx := t.Context()
			switch name {
			case "unmanaged":
				c.session.Scope.Managed = false
			case "disabled":
				profile := c.ExecutionProfile()
				profile.DisableHistoricalTaskReuse = true
				c.SetExecutionProfile(profile)
			case "independent_root":
				c.session.Workspace = t.TempDir()
			case "missing_policy":
				c.executionPolicy = nil
			case "policy_drift":
				c.maxConcurrent++
			case "missing_reader":
				c.SetHistoricalTaskReader(nil)
			case "duplicate_source":
				*rows = append(*rows, (*rows)[0])
			case "coordinator", "orchestrator", "unknown":
				bindings[0] = name + "=" + (*rows)[0].ResultID
			case "wildcard":
				bindings[0] = "*=" + (*rows)[0].ResultID
			case "prefix":
				bindings[0] = "worker=" + (*rows)[0].ResultID[:20]
			case "empty":
				bindings = nil
			case "too_many":
				for n := range maxHistoricalResultGrants {
					bindings = append(bindings, "worker=res-"+fmt.Sprintf("%064x", n))
				}
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if prepared, err := c.PrepareHistoricalResultGrants(ctx, bindings); err == nil || prepared != nil || worker.Calls() != 0 {
				t.Fatalf("unsupported preflight succeeded or dispatched: %+v, %v", prepared, err)
			}
		})
	}
}

func TestHistoricalResultRecipientRolesCannotUseCaseAliases(t *testing.T) {
	c, rows, _ := historicalGrantPreparationFixture(t)
	for _, role := range []string{"COORDINATOR", "Orchestrator"} {
		c.session.Agents["control-alias"] = &agent.AgentDef{Name: "Controller", Role: role}
		if _, err := c.historicalResultGrantRequests([]string{"control-alias=" + (*rows)[0].ResultID}); err == nil {
			t.Fatalf("role case bypassed explicit recipient authorization: %s", role)
		}
	}
}

func TestHistoricalResultPreparationRejectsFrozenDrift(t *testing.T) {
	for _, changed := range []string{"body", "source_hash", "payload_hash", "policy", "invalidated"} {
		t.Run(changed, func(t *testing.T) {
			c, rows, _ := historicalGrantPreparationFixture(t)
			prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{"worker=" + (*rows)[0].ResultID})
			if err != nil {
				t.Fatal(err)
			}
			envelope, err := prepared.envelope("session", "cycle")
			if err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "body":
				(*rows)[0].TypedResultJSON = []byte(strings.Replace(string((*rows)[0].TypedResultJSON), "complete deliverable", "replaced deliverable", 1))
			case "source_hash":
				(*rows)[0].SourceHash = strings.Repeat("c", 64)
			case "payload_hash":
				(*rows)[0].PayloadSHA256 = strings.Repeat("d", 64)
			case "policy":
				envelope.PolicyFingerprint = strings.Repeat("e", 64)
			case "invalidated":
				(*rows)[0].Validity = "invalidated"
			}
			if rebuilt, err := c.rebuildHistoricalResultGrants(t.Context(), envelope); err == nil || rebuilt != nil {
				t.Fatal("rebuild replaced frozen content with current data")
			}
		})
	}
}

func TestHistoricalResultPreparationEnforcesCompleteByteLimit(t *testing.T) {
	c, rows, _ := historicalGrantPreparationFixture(t)
	row := &(*rows)[0]
	var document map[string]jsonv1.RawMessage
	if err := json.Unmarshal(row.TypedResultJSON, &document); err != nil {
		t.Fatal(err)
	}
	text, _ := json.Marshal(strings.Repeat("界", maxHistoricalResultBytes/3))
	document["details"] = text
	row.TypedResultJSON, _ = json.Marshal(document)
	if prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{"worker=" + row.ResultID}); err == nil || prepared != nil {
		t.Fatal("oversized UTF-8 presentation was silently truncated")
	}
}

func TestHistoricalResultPreparationCountsAllRequiredPresentations(t *testing.T) {
	c, rows, _ := historicalGrantPreparationFixture(t)
	*rows = append(*rows, historicalGrantSourceFixture(t, c.session.Workspace, "2"))
	for i := range *rows {
		row := &(*rows)[i]
		row.TypedResultJSON = []byte(strings.Replace(string(row.TypedResultJSON), "complete deliverable", strings.Repeat("x", maxHistoricalResultBytes/2), 1))
	}
	first := "worker=" + (*rows)[0].ResultID
	if _, err := c.PrepareHistoricalResultGrants(t.Context(), []string{first, first}); err != nil {
		t.Fatalf("duplicate binding consumed the byte budget twice: %v", err)
	}
	if prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{first, "worker=" + (*rows)[1].ResultID}); err == nil || prepared != nil {
		t.Fatal("aggregate size overflow returned only part of the required sources")
	}
}

func TestHistoricalResultPreparationReturnsNoPartialGrantSet(t *testing.T) {
	c, rows, worker := historicalGrantPreparationFixture(t)
	*rows = append(*rows, historicalGrantSourceFixture(t, c.session.Workspace, "2"))
	slices.SortFunc(*rows, func(a, b HistoricalTaskResult) int { return strings.Compare(a.ResultID, b.ResultID) })
	(*rows)[1].Validity = "invalidated"
	if prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{"worker=" + (*rows)[0].ResultID, "worker=" + (*rows)[1].ResultID}); err == nil || prepared != nil || worker.Calls() != 0 {
		t.Fatal("invalid required source left a partial grant set or dispatched a model")
	}
}

func TestHistoricalResultPreparationValidatesReceiptBoundPayloads(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(fmt.Sprint(valid), func(t *testing.T) {
			c, rows, _ := historicalGrantPreparationFixture(t)
			row := &(*rows)[0]
			outputs, digest, err := CanonicalizeRuntimeOutputs(map[string]any{"receipt": map[string]any{"count": jsonv1.Number("9007199254740993")}})
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]jsonv1.RawMessage
			if err := json.Unmarshal(row.TypedResultJSON, &document); err != nil {
				t.Fatal(err)
			}
			document["source"] = []byte(`"runtime"`)
			document["runtime_outputs"], _ = jsonv1.Marshal(outputs)
			document["runtime_outputs_hash"], _ = json.Marshal(digest)
			payload := []byte(`{"diagnostic":"complete canonical diagnostic","receipt_id":"receipt-accepted"}`)
			payloadHash := historicalResultDigest(payload)
			if !valid {
				payloadHash = strings.Repeat("f", 64)
			}
			document["structured_payload"], _ = json.Marshal(map[string]any{"value": jsonv1.RawMessage(payload), "sha256": payloadHash})
			row.TypedResultJSON, _ = json.Marshal(document)
			prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{"worker=" + row.ResultID})
			if valid {
				if err != nil || !strings.Contains(prepared.presentations[0], "9007199254740993") || !strings.Contains(prepared.presentations[0], "complete canonical diagnostic") {
					t.Fatalf("complete receipt-bound payload was changed or omitted: %v", err)
				}
			} else if err == nil || prepared != nil {
				t.Fatal("source with a broken structured receipt was delivered")
			}
		})
	}
}

func TestHistoricalResultPreparationPreservesRawStructuredPayloads(t *testing.T) {
	c, rows, _ := historicalGrantPreparationFixture(t)
	row := &(*rows)[0]
	var document map[string]jsonv1.RawMessage
	if err := json.Unmarshal(row.TypedResultJSON, &document); err != nil {
		t.Fatal(err)
	}
	payload, err := jsonv1.Marshal(map[string]any{"html": "<finding>&</finding>", "count": jsonv1.Number("1.0")})
	if err != nil {
		t.Fatal(err)
	}
	document["structured_payload"], err = jsonv1.Marshal(map[string]any{"value": jsonv1.RawMessage(payload), "sha256": historicalResultDigest(payload)})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := jsonv1.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	row.TypedResultJSON, row.PayloadSHA256, err = ProjectHistoricalTaskResult(canonical)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := c.PrepareHistoricalResultGrants(t.Context(), []string{"worker=" + row.ResultID})
	if err != nil {
		t.Fatalf("canonical escaped payload was invalidated during projection: %v", err)
	}
	presentation := prepared.presentations[0]
	start, end := strings.IndexByte(presentation, '{'), strings.LastIndexByte(presentation, '}')
	if start < 0 || end < start {
		t.Fatal("presentation has no complete JSON document")
	}
	var decoded struct {
		TypedResult historicalResultHeader `json:"typed_result"`
	}
	if err := json.Unmarshal([]byte(presentation[start:end+1]), &decoded); err != nil {
		t.Fatal(err)
	}
	if err := validateHistoricalResultHashes(decoded.TypedResult); err != nil {
		t.Fatalf("presentation encoding invalidated the canonical payload: %v", err)
	}
	if string(decoded.TypedResult.StructuredPayload.Value) != string(payload) {
		t.Fatal("complete typed presentation rewrote canonical raw JSON")
	}
}
