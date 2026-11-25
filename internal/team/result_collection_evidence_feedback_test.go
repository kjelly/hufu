package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

func TestResultCollectionAuthenticatesProducerAndResolvedEvidenceSiblings(t *testing.T) {
	const content = "accepted original source"
	artifactID := "sha256-" + sha256HexBytes([]byte(content))
	schema := strings.Replace(collectionReferenceSchema, `"x-hufu-evidence-references":`, `"x-hufu-evidence-keys":{"runtime_owned_files_read":true,"pointers":["/references/*/paths/*"]},"x-hufu-evidence-references":`, 1)
	compiled := compileFixtureJSONSchema(t, "overlapping-evidence", schema)
	producerSchema := compileFixtureJSONSchema(t, "accepted-source", `{"type":"object","properties":{"verified":{"type":"boolean"}}}`)
	for _, normalized := range []bool{false, true} {
		for _, tc := range []struct {
			name, producer, extra string
			corrupt               bool
			want                  RepairFailureReason
		}{
			{"unknown choice", "producer", "", false, RepairFailureInvalidSchema},
			{"foreign producer", "foreign", "", false, RepairFailureSource},
			{"foreign literal path", "producer", `,"foreign.go"`, false, RepairFailureSource},
			{"corrupt observed artifact", "producer", "", true, RepairFailureSource},
		} {
			for _, route := range []string{"direct", "live", "reopened"} {
				t.Run(fmt.Sprintf("normalizer=%t/%s/%s", normalized, tc.name, route), func(t *testing.T) {
					testEvidenceSiblings(t, compiled, producerSchema, content, artifactID, route, normalized, tc.producer, tc.extra, tc.corrupt, tc.want)
				})
			}
		}
	}
}

func testEvidenceSiblings(t *testing.T, compiled, producerSchema *CompiledResultContract, content, artifactID, route string, normalized bool, claimedProducer, extra string, corrupt bool, want RepairFailureReason) {
	t.Helper()
	var draft, corrected string
	var normalizations atomic.Int32
	var nextResponse atomic.Pointer[string]
	endpoint, calls := collectionEvidenceFeedbackServer(t, func(int32) string { return *nextResponse.Load() })
	ref := compiled.ref(true)
	configure := func(s *TodoSpec) {
		s.ResultContract, s.EvidenceFrom = &ref, []string{"producer"}
	}
	c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, route != "direct", configure, func(c *Coordinator) {
		c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled, producerSchema.ID: producerSchema}
		c.session.Agents["worker"].Tools = "view"
		c.initEventStore()
		appendEvidenceSiblingProducer(t, c, producerSchema, artifactID)
		if normalized {
			bindTestNormalizer(t, c, &TodoItem{ResultContract: &ref}, &mockNormalizerProvider{fn: func(_ context.Context, raw []byte) ([]byte, error) {
				normalizations.Add(1)
				if string(raw) == "{}" {
					return []byte(draft), nil
				}
				return raw, nil
			}})
		}
	})
	artifact := observeCollectionArtifact(t, c, item, ctx, &receipt, content)
	var key string
	for _, event := range mustReadEvents(t, c.eventStore) {
		if event.Type == string(EventArtifactEvidenceObserved) {
			var observation decisionArtifactObservation
			if err := json.Unmarshal(event.Payload, &observation); err != nil {
				t.Fatal(err)
			}
			key = artifactEvidenceKey(event, observation, artifact)
		}
	}
	if key == "" || artifact.ID != artifactID {
		t.Fatal("fixture did not observe the accepted producer's source")
	}
	draft = fmt.Sprintf(`{"references":[{"producer":%q,"paths":["ev_000000000000",%q%s]}]}`, claimedProducer, key, extra)
	corrected = fmt.Sprintf(`{"status":"success","summary":"accepted original claims","structured_payload":{"references":[{"producer":"producer","paths":[%q]}]}}`, key)
	nextResponse.Store(&corrected)
	submitted := draft
	if normalized {
		submitted = "{}"
	}
	wire := `{"status":"success","summary":"original claims","structured_payload":` + submitted + `}`
	corruptArtifact := func() {
		if corrupt {
			if err := os.WriteFile(normalizedArtifactData(c, artifact.ID), []byte("corrupt"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if route == "direct" {
		corruptArtifact()
		ctx = context.WithValue(ctx, artifactAccessScopeKey, receipt.ArtifactScope)
		ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: receipt.RunID, TaskID: item.ID, AgentName: item.Agent, Attempt: 1, ModelExecutionID: receipt.ModelExecutionID})
		response, err := (&submitResultTool{coordinator: c, todoID: item.ID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: wire})
		if err != nil || !response.IsError || submissionFailureMetadata(response.Metadata) != want ||
			(want == RepairFailureInvalidSchema && !strings.Contains(response.Content, "evidence_reference_unknown")) {
			t.Fatalf("direct classification = %+v, %v", response, err)
		}
		return
	}
	cp, err := c.beginResultCollection(ctx, item.ID, receipt, "original work")
	if err == nil {
		cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
	}
	if err == nil {
		cp, err = c.recordCollectionResponse(ctx, cp, []byte(wire))
	}
	if err != nil {
		t.Fatal(err)
	}
	if route == "live" {
		corruptArtifact()
		err = c.collectWorkResult(ctx, c.session.Agents[item.Agent], cp)
		if want == RepairFailureSource && localResultRejectionReason(err) != want {
			t.Fatalf("live failure classification = %v", err)
		}
	} else {
		if normalized {
			cp, err = c.prepareCollectionClaims(ctx, cp)
			if err != nil {
				t.Fatal(err)
			}
		}
		corruptArtifact()
		c, err = resumeRejectedEvidenceCollection(t, c, item, cp, ctx, want)
	}
	result := c.GetTaskResult(item.ID)
	if want == RepairFailureSource {
		if err == nil || calls.Load() != 0 || result != nil || normalizations.Load() > 1 {
			t.Fatalf("source failure received a correction: turns=%d normalizations=%d err=%v", calls.Load(), normalizations.Load(), err)
		}
	} else if err != nil || calls.Load() != 1 || (normalized && normalizations.Load() != 2) || result == nil ||
		result.StructuredPayload == nil || !strings.Contains(string(result.StructuredPayload.Value), artifact.ID) {
		t.Fatalf("correction lost the accepted artifact: turns=%d normalizations=%d err=%v", calls.Load(), normalizations.Load(), err)
	}
}

func appendEvidenceSiblingProducer(t *testing.T, c *Coordinator, compiled *CompiledResultContract, artifactID string) {
	t.Helper()
	ref := compiled.ref(true)
	payload, err := validateStructuredResultPayload(compiled, ref, []byte(`{"verified":true}`))
	if err != nil {
		t.Fatal(err)
	}
	source := &TodoItem{ID: "producer", Agent: "worker", Status: TaskDone, ResultContract: &ref}
	source.TypedResult = &TaskResult{TaskID: source.ID, Agent: source.Agent, Source: "submitted", Status: TaskResultStatusSuccess,
		Summary: "verified producer", StructuredPayload: payload, FilesRead: []FileRef{{Path: artifactID}}}
	c.taskTracker.TodoList().AddReserved([]*TodoItem{source})
	body, err := json.Marshal(source, collectionDurationFormat)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []EventType{EventTaskCreated, EventTaskCompleted} {
		if _, err := c.EventJournal().Append(t.Context(), RunEvent{Type: string(kind), BranchID: "main", RunID: c.executionRunID,
			TaskID: source.ID, Actor: source.Agent, Payload: body}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResultCollectionAuthenticatesEvidenceBeforeRepairingItsRepresentation(t *testing.T) {
	const producerID = "accepted-producer"
	const clean = `{"status":"success","summary":"report","structured_payload":{"references":[]}}`
	const observed = `{"status":"success","summary":"report","files_read":[{"path":"source.go"}],"structured_payload":{"references":[{"producer":"` + producerID + `","paths":["source.go"]}]}}`
	schema := strings.Replace(collectionReferenceSchema, `"properties":{"references"`,
		`"properties":{"audit_complete":{"type":"boolean","const":true},"evidence_rows":{"type":"array","items":{"type":"object","additionalProperties":false,"required":["id","digest"],"properties":{"id":{"type":"string"},"digest":{"type":"string"}}}},"references"`, 1)
	schema = strings.Replace(schema, `"x-hufu-evidence-references":`,
		`"x-hufu-evidence-inputs":{"runtime_owned":true,"runtime_complete":true,"items_pointer":"/evidence_rows","task_id_pointer":"/id","hash_pointer":"/digest","complete_pointer":"/audit_complete"},"x-hufu-evidence-references":`, 1)
	compiled := compileFixtureJSONSchema(t, "bound-evidence-collection", schema)
	producer := compileFixtureJSONSchema(t, "producer", `{"type":"object","properties":{"from":{"type":"string"}}}`)
	producerRef := producer.ref(true)
	for _, tc := range []struct {
		name, payload, corrected string
		tampered                 bool
		want                     RepairFailureReason
	}{
		{"runtime-owned rows", `{"evidence_rows":[],"references":[]}`, clean, false, RepairFailureInvalidSchema},
		{"runtime-owned completion", `{"audit_complete":true,"references":[]}`, clean, false, RepairFailureInvalidSchema},
		{"runtime-owned rows with corrupt source", `{"evidence_rows":[],"references":[]}`, clean, true, RepairFailureSource},
		{"runtime-owned rows with foreign producer", `{"evidence_rows":[],"references":[{"producer":"foreign","paths":["source.go"]}]}`, clean, false, RepairFailureSource},
		{"observed scalar path", `{"references":[{"producer":"` + producerID + `","paths":"source.go"}]}`, observed, false, RepairFailureInvalidSchema},
		{"unobserved scalar path", `{"references":[{"producer":"` + producerID + `","paths":"unknown.go"}]}`, observed, false, RepairFailureSource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := `{"status":"success","summary":"report","structured_payload":` + tc.payload + `}`
			endpoint, calls := collectionEvidenceFeedbackServer(t, func(turn int32) string {
				if turn == 1 {
					return wire
				}
				return tc.corrected
			})
			configure := func(s *TodoSpec) {
				s.ResultContract = new(compiled.ref(true))
				s.EvidenceFrom = []string{producerID}
			}
			install := func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled, producer.ID: producer}
				payload, err := validateStructuredResultPayload(producer, producerRef, []byte(`{"from":"verified"}`))
				if err != nil {
					t.Fatal(err)
				}
				if tc.tampered {
					payload.SHA256 = strings.Repeat("0", 64)
				}
				source := &TodoItem{ID: producerID, Status: TaskDone, ResultContract: &producerRef, TypedResult: &TaskResult{
					TaskID: producerID, Status: TaskResultStatusSuccess, Summary: "accepted source", StructuredPayload: payload,
					FilesRead: []FileRef{{Path: "source.go"}},
				}}
				c.taskTracker.TodoList().AddReserved([]*TodoItem{source})
				if err := os.WriteFile(filepath.Join(c.projectDir, "source.go"), []byte("package fixture\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, true, configure, install)
			err := c.collectReturnedWork(ctx, c.session.Agents[item.Agent], receipt, "report", "original work")
			if tc.want == RepairFailureSource {
				failure, ok := errors.AsType[*resultCollectionFailure](err)
				if !ok || failure.Reason != RepairFailureSource || calls.Load() > 1 || c.GetTaskResult(item.ID) != nil {
					t.Fatalf("invalid source received a correction: calls=%d err=%v", calls.Load(), err)
				}
			} else {
				result := c.GetTaskResult(item.ID)
				if err != nil || calls.Load() != 2 || result == nil || result.StructuredPayload == nil ||
					!strings.Contains(string(result.StructuredPayload.Value), producerID) {
					t.Fatalf("correction did not bind authenticated evidence: calls=%d err=%v", calls.Load(), err)
				}
			}
			direct, task, ctx, _ := resultCollectionTaskFixture(t, endpoint, false, configure, install)
			response, err := (&submitResultTool{coordinator: direct, todoID: task.ID}).Run(ctx, fantasy.ToolCall{Name: submitResultToolName, Input: wire})
			if err != nil || !response.IsError || submissionFailureMetadata(response.Metadata) != tc.want {
				t.Fatalf("direct classification = %+v, %v", response, err)
			}
		})
	}
}

func TestResultCollectionChecksEveryReadablePayloadWithEvidenceKeys(t *testing.T) {
	schema := strings.Replace(collectionReferenceSchema, `"x-hufu-evidence-references":`, `"x-hufu-evidence-keys":{"pointers":["/references/*/paths/*"]},"x-hufu-evidence-references":`, 1)
	compiled := compileFixtureJSONSchema(t, "keyed-collection", schema)
	const corrected = `{"status":"success","summary":"report","structured_payload":{"references":[]}}`
	for _, tc := range []struct {
		name, members string
		want          RepairFailureReason
		wrapped       bool
	}{
		{"verified payload", `"structured_payload":{"references":[]}`, RepairFailureInvalidSchema, false},
		{"foreign producer", `"structured_payload":{"references":[{"producer":"foreign","paths":["unobserved.go"]}]}`, RepairFailureSource, false},
		{"foreign producer after malformed payload", `"structured_payload":{"remarks":"a","remarks":"b","references":[]},"structured_payload":{"references":[{"producer":"foreign","paths":["unobserved.go"]}]}`, RepairFailureSource, false},
		{"clean source beside inner duplicate", `"structured_payload":{"remarks":"a","remarks":"b","references":[]}`, RepairFailureInvalidSchema, false},
		{"foreign source beside inner duplicate", `"structured_payload":{"remarks":"a","remarks":"b","references":[{"producer":"foreign","paths":["unobserved.go"]}]}`, RepairFailureSource, false},
		{"foreign duplicate reference member", `"structured_payload":{"references":[{"producer":"foreign","paths":["unobserved.go"]}],"references":[]}`, RepairFailureSource, false},
		{"clean nested duplicates in wrappers", `"result":{"status":"success","structured_payload":{"remarks":"a","remarks":"b","references":[]}},"result":` + corrected, RepairFailureInvalidSchema, true},
		{"foreign source after malformed wrapper", `"result":{"status":"success","structured_payload":{"remarks":"a","remarks":"b","references":[]}},"result":{"status":"success","structured_payload":{"references":[{"producer":"foreign","paths":["unobserved.go"]}]}}`, RepairFailureSource, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := `{"status":"success","summary":"draft","summary":"report",` + tc.members + `}`
			var issue *agent.StructuredResponseIssue
			if tc.wrapped {
				wire = `{` + tc.members + `}`
				issue = &agent.StructuredResponseIssue{Code: "invalid_json", Pointer: "/result", Expected: "object", Actual: "invalid_json"}
			}
			configure := func(s *TodoSpec) { s.ResultContract = new(compiled.ref(true)) }
			install := func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			}
			t.Run("direct", func(t *testing.T) {
				if tc.wrapped {
					t.Skip("the response wrapper belongs to collector transport")
				}
				c := newTestCoordinator(t)
				install(c)
				spec := TodoSpec{Agent: "worker", Desc: "submit evidence"}
				configure(&spec)
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{spec})[0]
				c.workerAgentOverride, _ = normalizationStreamedSubmission(c, item.ID, wire, nil)
				repair := &rejectedResultStreamModel{toolName: submitResultToolName, finish: fantasy.FinishReasonToolCalls, input: func(int) string { return corrected }}
				c.repairAgentOverride = fantasy.NewAgent(repair, fantasy.WithTools(&submitResultTool{coordinator: c, todoID: item.ID}), fantasy.WithMaxRetries(0))
				_, err := c.executeTask(withTestProtocolRepairInvocationContext(t.Context()), TaskDef{
					Agent: "worker", Goal: "submit evidence", Recovery: RecoveryManual, SideEffect: SideEffectExternalWrite,
					Execution: ExecutionContract{RequiresResult: true}, ResultContract: item.ResultContract,
				}, item.ID)
				if tc.want == RepairFailureSource {
					receipt := todoByID(t, c, item.ID).ExecutionReceipt
					if err == nil || repair.calls != 0 || receipt == nil || receipt.WorkSubmissionFailure != tc.want {
						t.Fatalf("foreign source received a correction: receipt=%+v calls=%d err=%v", receipt, repair.calls, err)
					}
				} else if err != nil || repair.calls != 1 || c.GetTaskResult(item.ID) == nil {
					t.Fatalf("duplicate summary did not get one correction: calls=%d err=%v", repair.calls, err)
				}
			})
			for _, route := range []string{"live", "reopened"} {
				t.Run(route, func(t *testing.T) {
					endpoint, calls := collectionEvidenceFeedbackServer(t, func(int32) string { return corrected })
					c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, true, configure, install)
					cp, err := c.beginResultCollection(ctx, item.ID, receipt, "report")
					if err == nil {
						cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
					}
					if err == nil {
						cp, err = c.recordCollectionResponseUsage(ctx, cp, []byte(wire), ExecutionUsage{}, issue)
					}
					if err != nil {
						t.Fatal(err)
					}
					if route == "live" {
						err = c.collectWorkResult(ctx, c.session.Agents[item.Agent], cp)
					} else {
						c, err = resumeRejectedEvidenceCollection(t, c, item, cp, ctx, tc.want)
					}
					if tc.want == RepairFailureSource {
						if err == nil || calls.Load() != 0 || c.GetTaskResult(item.ID) != nil {
							t.Fatalf("foreign source received a correction or result: calls=%d err=%v", calls.Load(), err)
						}
					} else if err != nil || calls.Load() != 1 || c.GetTaskResult(item.ID) == nil {
						t.Fatalf("duplicate summary did not get one correction: calls=%d err=%v", calls.Load(), err)
					}
				})
			}
		})
	}
}

func resumeRejectedEvidenceCollection(t *testing.T, c *Coordinator, item *TodoItem, original *ResultCollectionCheckpoint, ctx context.Context, reason RepairFailureReason) (*Coordinator, error) {
	t.Helper()
	stopped, err := c.blockResultCollection(ctx, original, RepairFailureInvalidSchema)
	if err != nil {
		t.Fatal(err)
	}
	c.recordCollectionStopReceipt(item.ID, &resultCollectionFailure{Reason: RepairFailureInvalidSchema, Checkpoint: stopped, Cause: errors.New("historical format rejection")})
	c.PersistFailureWithClassAndStatusAndOutput(item.Agent, item.Goal, item.ID, "historical format rejection", NeedsHuman, FailureProtocol, TaskBlocked, "report")
	c = reopenRecoveryCoordinator(t, c)
	before := mustReadEvents(t, c.eventStore)
	a, err := c.AssessResultCollectionRecovery(t.Context(), item.ID)
	want, extraTurn := RecoveryRepairClaims, 1
	switch reason {
	case "":
		want, extraTurn = RecoveryCommitLocal, 0
	case RepairFailureSource:
		want = RecoveryBlocked
	}
	if err != nil || a.Disposition != want || a.AdditionalTurns != 0 || a.RemainingTurns != 1 {
		t.Fatalf("cold classification = %+v, err=%v", a, err)
	}
	if !sameCanonicalEvidence(before, mustReadEvents(t, c.eventStore)) || a.Checkpoint.Revision != stopped.Revision {
		t.Fatal("recovery preview modified durable work or claims")
	}
	err = c.authorizeOperatorResultRepair(WithResultOnlyRepair(t.Context(), item.ID))
	if want == RecoveryBlocked {
		return c, err
	}
	if err != nil {
		t.Fatal(err)
	}
	worker := scriptWorker(failTurn(errors.New("original work must not be repeated")))
	c.workerAgentOverride = worker
	_, err = c.executeTask(WithSessionResume(t.Context()), taskDefFromTodoItem(c.todoItemByID(item.ID)), item.ID)
	cp := currentResultCollection(c.todoItemByID(item.ID))
	if worker.Calls() != 0 || cp == nil || cp.Work != original.Work || cp.TurnsUsed != original.TurnsUsed+extraTurn {
		t.Fatalf("cold repair repeated the original work or used another turn: %+v", cp)
	}
	return c, err
}

func TestResultCollectionAuthenticatesEvidenceWhenParentRejectsCondition(t *testing.T) {
	for _, schema := range []struct {
		name, stateField, statePointer, validValue, invalidValue string
	}{
		{"propertyNames", `"if":{"properties":{"state":{"const":"unknown"}}},"then":{"propertyNames":{"not":{"const":"state"}}},"properties":{"state":{"type":"string"},"producer":`, "/state", `%q`, `%q`},
		{"array index", `"properties":{"state":{"anyOf":[{"type":"object","properties":{"0":{"type":"string"}}},{"type":"array","prefixItems":[{"enum":["active","inactive"]}]}]},"producer":`, "/state/0", `[%q]`, `[%q]`},
		{"invalid container", `"properties":{"state":{"type":"object","properties":{"0":{"type":"string"}}},"producer":`, "/state/0", `{"0":%q}`, `[%q]`},
	} {
		t.Run(schema.name, func(t *testing.T) {
			document := strings.Replace(collectionReferenceSchema, `"properties":{"producer":`, schema.stateField, 1)
			condition := fmt.Sprintf(`"references_pointer":"/paths","when":{"pointer":%q,"value":"active"}`, schema.statePointer)
			document = strings.Replace(document, `"references_pointer":"/paths"`, condition, 1)
			assertCollectionAuthenticatesInvalidCondition(t, compileFixtureJSONSchema(t, "conditional-collection", document), schema.validValue, schema.invalidValue)
		})
	}
}

func assertCollectionAuthenticatesInvalidCondition(t *testing.T, compiled *CompiledResultContract, validValue, invalidValue string) {
	t.Helper()
	producer := compileFixtureJSONSchema(t, "conditional-source", `{"type":"object","properties":{"verified":{"type":"boolean"}}}`)
	const corrected = `{"status":"success","summary":"corrected","structured_payload":{"references":[]}}`
	for _, tc := range []struct {
		name, state, producerID string
		reason                  RepairFailureReason
		turns                   int32
	}{
		{"valid inactive", "inactive", "foreign", "", 0},
		{"invalid clean", "unknown", "producer", RepairFailureInvalidSchema, 1},
		{"invalid foreign", "unknown", "foreign", RepairFailureSource, 0},
	} {
		for _, route := range []string{"live", "reopened"} {
			t.Run(tc.name+"/"+route, func(t *testing.T) {
				state := invalidValue
				if tc.reason == "" {
					state = validValue
				}
				wire := fmt.Sprintf(`{"status":"success","summary":"report","structured_payload":{"references":[{"state":%s,"producer":%q,"paths":["source.go"]}]}}`, fmt.Sprintf(state, tc.state), tc.producerID)
				endpoint, calls := collectionEvidenceFeedbackServer(t, func(int32) string { return corrected })
				c, item, ctx, receipt := resultCollectionTaskFixture(t, endpoint, true, func(s *TodoSpec) {
					s.ResultContract = new(compiled.ref(true))
					s.EvidenceFrom = []string{"producer"}
				}, func(c *Coordinator) {
					c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled, producer.ID: producer}
					c.initEventStore()
					appendEvidenceSiblingProducer(t, c, producer, "source.go")
				})
				cp, err := c.beginResultCollection(ctx, item.ID, receipt, "report")
				if err == nil {
					cp, err = c.reserveResultCollection(ctx, cp, "original-collector", []byte(`{"tools":[]}`))
				}
				if err == nil {
					cp, err = c.recordCollectionResponse(ctx, cp, []byte(wire))
				}
				if err != nil {
					t.Fatal(err)
				}
				if route == "live" {
					err = c.collectWorkResult(ctx, c.session.Agents[item.Agent], cp)
				} else {
					c, err = resumeRejectedEvidenceCollection(t, c, item, cp, ctx, tc.reason)
				}
				result := c.GetTaskResult(item.ID)
				if tc.reason == RepairFailureSource {
					if err == nil || result != nil || route == "live" && localResultRejectionReason(err) != tc.reason {
						t.Fatalf("foreign producer was not blocked: %v", err)
					}
				} else if err != nil || result == nil {
					t.Fatalf("source-clean result did not settle: %v", err)
				}
				if calls.Load() != tc.turns {
					t.Fatalf("correction calls=%d, want %d", calls.Load(), tc.turns)
				}
			})
		}
	}
}

func collectionEvidenceFeedbackServer(t *testing.T, answer func(int32) string) (string, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			_, _ = fmt.Fprint(w, `{"model":"test","parameters":"num_ctx 131072","model_info":{"general.architecture":"qwen2","qwen2.context_length":131072},"capabilities":["completion","tools"]}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"return\",\"type\":\"function\",\"function\":{\"name\":\"return_result\",\"arguments\":%q}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", `{"result":`+answer(calls.Add(1))+`}`)
	}))
	t.Cleanup(server.Close)
	return server.URL + "/v1", &calls
}
