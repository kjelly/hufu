package team

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/tools"
)

func TestResultCollectionMixedFanoutResumesExternalCompletion(t *testing.T) {
	for _, scenario := range []string{"present", "present_secondary", "leaf_document_write", "leaf_artifact_write", "leaf_after_verified_sibling",
		"leaf_transcript_data_write", "leaf_transcript_metadata_write", "leaf_transcript_timestamp_conflict", "missing", "corrupt", "missing_session", "missing_local", "corrupt_attachment",
		"missing_attachment", "missing_external_file", "corrupt_external_file", "missing_external_read", "corrupt_external_read",
		"missing_external_transcript", "corrupt_external_transcript", "corrupt_external_transcript_timestamp"} {
		t.Run(scenario, func(t *testing.T) {
			external := execution.ExecutionTarget{Backend: "codex", Model: "gpt-5-codex"}
			local := execution.ExecutionTarget{Backend: "ollama", Model: "test"}
			topology := []execution.ExecutionTarget{external, local}
			externalIndex := 0
			if scenario == "present_secondary" {
				topology[0], topology[1] = topology[1], topology[0]
				externalIndex = 1
			}
			externalWriteFault := strings.HasPrefix(scenario, "leaf_")
			verifiedSibling := scenario == "leaf_after_verified_sibling"
			transcriptWriteFault := scenario == "leaf_transcript_data_write" || scenario == "leaf_transcript_metadata_write"
			pendingTimestampConflict := scenario == "leaf_transcript_timestamp_conflict"
			publishedTimestampConflict := scenario == "corrupt_external_transcript_timestamp"
			present := scenario == "present" || scenario == "present_secondary" || externalWriteFault && !pendingTimestampConflict
			attachmentLost := strings.HasSuffix(scenario, "_attachment")
			externalEvidenceLost := strings.Contains(scenario, "_external_")
			var source ArtifactRef
			const sourceTask = "external-evidence-source"
			const proposedPath = "external-delivery.txt"
			const proposedBody = "file present at original external completion"
			c, item, _, receipt := resultCollectionIdentityFixture(t, "http://127.0.0.1:1/v1", true, "", func(spec *TodoSpec) {
				spec.Model, spec.SideEffect = topology[0].String(), SideEffectNone
				spec.ExecutionTopology, spec.ExecutionTarget = topology, topology[0]
				spec.EvidenceFrom = []string{sourceTask}
				if externalWriteFault {
					spec.Verify = "printf 'verified\\n' >> fanout-verification.log && test -s fanout-verification.log"
				}
			}, func(c *Coordinator, spec *TodoSpec, ids []string) {
				if transcriptWriteFault {
					script := filepath.Join(c.projectDir, "fault-transcript.sh")
					command := transcriptFanoutFaultCommand(c, ids[0], spec.Agent, external, externalIndex, scenario)
					if err := os.WriteFile(script, []byte(command+"\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					spec.Verify += " && sh " + quoteFanoutFixtureArg(script)
				}
			}, func(c *Coordinator) {
				def := c.session.Agents["worker"]
				def.Generation.Model, def.ExtraModels = topology[0].String(), []string{topology[1].String()}
				def.MaxRetries = 0
				if externalWriteFault {
					def.MaxRetries = 2
				}
				source = acceptedExternalFanoutSource(t, c, sourceTask)
				if err := os.WriteFile(filepath.Join(c.projectDir, proposedPath), []byte(proposedBody), 0o600); err != nil {
					t.Fatal(err)
				}
				proposal := fmt.Sprintf(`{"status":"success","summary":"exact external result","proposed_files":[{"path":%q,"role":"delivery"}],"files_read":[%q]}`, proposedPath, source.ID)
				fixture := newCodexE2ECoordinatorBase(t, newCodexWorkspace(t), []fakeCodexStep{
					codexInitializeStep(t),
					fakeCodexThreadStartStep("thread-mixed", c.projectDir, external.Model, "read-only", t),
					fakeCodexTurnCompletedStep(t, "turn-mixed", proposal),
				}, def)
				c.session.Config.SubagentProviders = fixture.session.Config.SubagentProviders
			})
			callLog := filepath.Join(filepath.Dir(os.Getenv(fakeCodexServerScriptEnvVar)), "raw_calls.log")
			def := cloneAgentDef(c.session.Agents[item.Agent])
			def.Generation.Model = external.String()
			def.ExtraModels = nil
			task := taskDefFromTodoItem(item)
			localIndex := 1 - externalIndex
			var localReceipt *ExecutionReceipt
			if verifiedSibling {
				interruptFanoutCollectionBeforeReceived(t, c, item, receipt, localIndex)
				c = reopenRecoveryCoordinator(t, c)
				item = todoByID(t, c, item.ID)
				localReceipt = finishVerifiedFanoutSibling(t, c, item, task, localIndex, local)
				checkFanoutVerifications(t, c, 1)
			}
			restore := faultExternalFanoutPublication(t, c, item, scenario, proposedPath, proposedBody)
			output, err := c.executeSingleAgentWithModel(t.Context(), item.Agent, def, task, item.ID, fanoutSlotName(externalIndex), externalIndex, nil)
			if externalWriteFault {
				if localResultRejectionReason(err) != RepairFailureCommit {
					t.Fatalf("external attachment or receipt publication did not fail: %v", err)
				}
				if restore != nil {
					restore()
				}
				want := 1
				if verifiedSibling {
					want++
				}
				checkFanoutVerifications(t, c, want)
			} else if err != nil || !strings.Contains(output, "exact external result") {
				t.Fatalf("external execution could not complete: output=%q err=%v", output, err)
			}
			if calls := readRawCalls(t, callLog); len(calls) != 3 {
				t.Fatalf("expected one external execution, got %d RPC calls", len(calls))
			}
			item = todoByID(t, c, item.ID)
			sealed, _, err := c.readFanoutLeafCompletion(t.Context(), item, externalIndex)
			if err != nil || sealed == nil || sealed.Output != output || sealed.Receipt.AcceptedResult == nil ||
				sealed.Receipt.AcceptedResult.Summary != "exact external result" || (sealed.pendingBody != nil) != externalWriteFault {
				t.Fatalf("external slot completion=%+v, err=%v", sealed, err)
			}
			var originalProviderTranscript []byte
			if transcriptWriteFault {
				originalProviderTranscript = restoreFanoutTranscriptWrite(t, c, &sealed.Receipt, scenario)
			}
			if pendingTimestampConflict {
				damageFanoutTranscriptTimestamp(t, c, sealed.Receipt.ProviderTranscriptRef)
			}
			result := sealed.Receipt.AcceptedResult
			if len(result.Artifacts) != 1 || result.Artifacts[0].ID == "" || result.Artifacts[0].Path != proposedPath ||
				result.Artifacts[0].SHA256 != sha256HexBytes([]byte(proposedBody)) || len(result.FilesRead) != 1 ||
				result.FilesRead[0] != (FileRef{Path: source.ID, Purpose: "artifact"}) {
				t.Fatalf("external accepted evidence = %+v", result)
			}
			if err := os.Remove(filepath.Join(c.projectDir, proposedPath)); err != nil {
				t.Fatal(err)
			}
			boundSession, err := c.readFanoutBackendSession(item, externalIndex)
			if err != nil || boundSession == nil || boundSession.Binding.Backend != external.Backend ||
				boundSession.Binding.SessionID != "thread-mixed" ||
				(item.BackendBinding != nil && item.BackendBinding.SessionID != "") {
				t.Fatalf("external session was not independently bound to its slot: binding=%+v task=%+v err=%v", boundSession, item.BackendBinding, err)
			}
			var original *ResultCollectionCheckpoint
			if (present || attachmentLost || externalEvidenceLost) && !verifiedSibling {
				interruptFanoutCollectionBeforeReceived(t, c, item, receipt, localIndex)
			} else if !present && scenario != "missing_local" {
				original = beginUncollectedFanoutLeaf(t, c, item, receipt, localIndex, scenario == "corrupt")
			}
			name, _, err := c.fanoutLeafDocument(item, externalIndex)
			if err != nil {
				t.Fatal(err)
			}
			docs := workspaceDocs(c.artifactStoreRootPath())
			switch scenario {
			case "missing":
				if err := docs.deleteDocument(name); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := docs.putDocument(name, []byte(`{"output":"replacement"}`)); err != nil {
					t.Fatal(err)
				}
			case "missing_session":
				sessionDocument, _, err := c.fanoutBackendSessionDocument(item, externalIndex)
				if err != nil {
					t.Fatal(err)
				}
				if err := docs.deleteDocument(sessionDocument); err != nil {
					t.Fatal(err)
				}
			}

			c = reopenRecoveryCoordinator(t, c)
			c.executionRunID = "cold-mixed-fanout"
			forbidden := scriptWorker(failTurn(errors.New("sealed worker or legacy repair dispatched")))
			c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
			ctx := WithSessionResume(t.Context())
			if present || attachmentLost || externalEvidenceLost {
				if verifiedSibling {
					rejectFanoutProviderBoundary(t, c, ctx)
				}
				execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_mixed_done BEFORE INSERT ON events WHEN NEW.type='task_completed' BEGIN SELECT RAISE(ABORT, 'fixture aggregate write fault'); END`)
				count, err := c.ResumeInterruptedTasks(ctx)
				if count != 1 || localResultRejectionReason(err) != RepairFailureCommit {
					t.Fatalf("expected recoverable aggregate append fault, got count=%d err=%v", count, err)
				}
				execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_mixed_done")
				c = reopenRecoveryCoordinator(t, c)
				c.executionRunID = "cold-mixed-aggregate"
				c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
				if attachmentLost {
					damageAcceptedFanoutAttachment(t, c, item.ID, localIndex, scenario)
				}
				if publishedTimestampConflict {
					damageFanoutTranscriptTimestamp(t, c, sealed.Receipt.ProviderTranscriptRef)
				} else if externalEvidenceLost {
					id := result.Artifacts[0].ID
					if strings.HasSuffix(scenario, "_read") {
						id = source.ID
					}
					if strings.HasSuffix(scenario, "_transcript") {
						id = sealed.Receipt.ProviderTranscriptRef
					}
					damageFanoutCAS(t, normalizedArtifactData(c, id), strings.HasPrefix(scenario, "missing"))
				}
			}
			boundaryCalls := 0
			c.providerBoundaryStarted = false
			resumeCtx, cancel := context.WithCancelCause(ctx)
			defer cancel(nil)
			c.setInvocationWatchdog(c.newInvocationWatchdog(resumeCtx, cancel))
			defer c.setInvocationWatchdog(nil)
			c.providerBoundaryStart = func(context.Context, string) error {
				boundaryCalls++
				return errors.New("fanout recovery crossed provider boundary")
			}
			count, err := c.ResumeInterruptedTasks(resumeCtx)
			item = todoByID(t, c, item.ID)
			if count != 1 || len(readRawCalls(t, callLog)) != 3 || forbidden.Calls() != 0 || boundaryCalls != 0 {
				t.Fatalf("recovery count=%d, external RPCs=%d, work/repair calls=%d, provider starts=%d, err=%v", count, len(readRawCalls(t, callLog)), forbidden.Calls(), boundaryCalls, err)
			}
			if present {
				if err != nil || item.Status != TaskDone || !strings.Contains(item.Output, output) || !strings.Contains(item.Output, "exact local result") {
					t.Fatalf("new parent completion lost a leaf: status=%s output=%q err=%v", item.Status, item.Output, err)
				}
				if externalWriteFault {
					checkFanoutVerifications(t, c, 2)
				}
				if transcriptWriteFault {
					ref, published := readCodexTranscriptArtifact(t, c.session.Workspace, sealed.Receipt.ProviderTranscriptRef)
					if string(published) != string(originalProviderTranscript) || !ref.CreatedAt.Equal(sealed.Receipt.FinishedAt) {
						t.Fatal("published transcript differs from the exact provider return")
					}
				}
				c = reopenRecoveryCoordinator(t, c)
				item = todoByID(t, c, item.ID)
				var foundLocal, foundExternal bool
				for _, saved := range item.ExecutionReceipts {
					foundLocal = foundLocal || localReceipt != nil && sameCanonicalEvidence(&saved, localReceipt)
					foundExternal = foundExternal || sameCanonicalEvidence(&saved, &sealed.Receipt)
				}
				if !foundExternal || (verifiedSibling && !foundLocal) {
					t.Fatalf("terminal replay lost a receipt: local=%v external=%v", foundLocal, foundExternal)
				}
				return
			}
			if localResultRejectionReason(err) != RepairFailureSource || item.Status != TaskBlocked {
				t.Fatalf("unproven slot = %s, err=%v", item.Status, err)
			}
			if (pendingTimestampConflict || publishedTimestampConflict) && !strings.Contains(err.Error(), "transcript timestamp differs") {
				t.Fatalf("changed transcript timestamp was not rejected: %v", err)
			}
			if original != nil && !sameCanonicalEvidence(original, currentFanoutCollection(item, localIndex)) {
				t.Fatalf("unverified sibling consumed a collector turn: original=%+v recovered=%+v", original, currentFanoutCollection(item, localIndex))
			}
		})
	}
}

func damageFanoutTranscriptTimestamp(t *testing.T, c *Coordinator, id string) {
	t.Helper()
	path := filepath.Join(runtimestore.ArtifactRoot(c.artifactStoreRootPath()), "meta", id+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]jsontext.Value
	if err := json.Unmarshal(body, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["created_at"] = jsontext.Value(`"2001-01-01T00:00:00Z"`)
	body, err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func rejectFanoutProviderBoundary(t *testing.T, c *Coordinator, ctx context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancelCause(ctx)
	c.providerBoundaryStarted = false
	c.setInvocationWatchdog(c.newInvocationWatchdog(ctx, cancel))
	t.Cleanup(func() {
		c.setInvocationWatchdog(nil)
		cancel(nil)
	})
	c.providerBoundaryStart = func(context.Context, string) error {
		t.Error("offline fanout recovery started the provider boundary")
		return errors.New("fixture provider boundary unavailable")
	}
}

func transcriptFanoutFaultCommand(c *Coordinator, taskID, agent string, target execution.ExecutionTarget, index int, scenario string) string {
	run := c.contextRunID()
	model := modelFanoutExecutionID(run, taskID, agent, target.String(), fanoutSlotName(index), 0)
	stage := fanoutStagePath(c.artifactStoreRootPath(), codexFanoutTranscriptKey(model, 1))
	dest, suffix := transcriptFanoutFaultDestination(c, scenario)
	return fmt.Sprintf("if test -f %s; then hash=$({ printf '%%s\\000%%s\\000%%s\\0001\\000' %s %s %s; cat %s; } | sha256sum) && id=provider-transcript-${hash%%%% *}%s && ln -s \"$id\" %s/\"$id\"; fi",
		quoteFanoutFixtureArg(stage), quoteFanoutFixtureArg(target.Backend), quoteFanoutFixtureArg(run), quoteFanoutFixtureArg(taskID),
		quoteFanoutFixtureArg(stage), suffix, quoteFanoutFixtureArg(dest))
}

func quoteFanoutFixtureArg(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}

func transcriptFanoutFaultDestination(c *Coordinator, scenario string) (string, string) {
	root := runtimestore.ArtifactRoot(c.artifactStoreRootPath())
	if scenario == "leaf_transcript_metadata_write" {
		return filepath.Join(root, "meta"), ".json"
	}
	return filepath.Join(root, "data"), ""
}

func restoreFanoutTranscriptWrite(t *testing.T, c *Coordinator, receipt *ExecutionReceipt, scenario string) []byte {
	t.Helper()
	dest, suffix := transcriptFanoutFaultDestination(c, scenario)
	if err := os.Remove(filepath.Join(dest, receipt.ProviderTranscriptRef+suffix)); err != nil {
		t.Fatalf("expected actual filesystem failure at the provider transcript destination: %v", err)
	}
	content, err := readCodexFanoutTranscript(t.Context(), c.artifactStoreRootPath(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func finishVerifiedFanoutSibling(t *testing.T, c *Coordinator, item *TodoItem, task TaskDef, index int, target execution.ExecutionTarget) *ExecutionReceipt {
	t.Helper()
	def := cloneAgentDef(c.session.Agents[item.Agent])
	def.Generation.Model, def.ExtraModels = target.String(), nil
	recovery, err := c.prepareFanoutCollectionLeaf(t.Context(), item, index, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.executeSingleAgentWithModel(WithSessionResume(t.Context()), item.Agent, def, task, item.ID, fanoutSlotName(index), index, &recovery); err != nil {
		t.Fatal(err)
	}
	completed, _, err := c.readFanoutLeafCompletion(t.Context(), todoByID(t, c, item.ID), index)
	if err != nil || completed.Receipt.VerifyResult == nil || completed.Receipt.VerifyResult.ExitCode != 0 {
		t.Fatalf("verified sibling completion=%+v, err=%v", completed, err)
	}
	return &completed.Receipt
}

func TestResultCollectionFanoutChecksReturnedAttachmentsBeforeProvider(t *testing.T) {
	for _, state := range []ResultCollectionState{ResultCollectionInFlight, ResultCollectionReceived, ResultCollectionPrepared} {
		t.Run(string(state), func(t *testing.T) {
			c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
				spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
			})
			_, _, first, err := beginFanoutWorkForRecovery(t, c, item, receipt, 0)
			if err != nil {
				t.Fatal(err)
			}
			leaf, ctx, affected, err := beginFanoutWorkForRecovery(t, c, item, receipt, 1)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(leaf.session.Workspace, "delivery.txt")
			if err := os.WriteFile(path, []byte("original attachment"), 0o600); err != nil {
				t.Fatal(err)
			}
			affected = recordFanoutAttachmentResponse(t, c, leaf, ctx, affected, state)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			assertFanoutPreflightRejects(t, c, item, "delivery.txt", first, affected)
		})
	}
}

func TestResultCollectionFanoutChecksIndependentSourcesBeforeProvider(t *testing.T) {
	const schema = `{
		"type":"object","additionalProperties":false,"required":["completion"],
		"properties":{"completion":{"type":"object","required":["status","summary"],"properties":{"status":{"type":"string"},"summary":{"type":"string"}}}},
		"x-hufu-runtime-inputs":[{"target":"/completion","source_task":"missing-action","output":"report"}],
		"x-hufu-runtime-completion":"/completion"
	}`
	for _, scenario := range []struct {
		name, body, detail string
		state              ResultCollectionState
		native             bool
		wrapped            bool
	}{
		{"returned native completion", `{"structured_payload":{}}`, "runtime input /completion", ResultCollectionInFlight, true, false},
		{"received native completion", `{"structured_payload":{}}`, "runtime input /completion", ResultCollectionReceived, true, false},
		{"mixed artifact declarations", `{"status":"success","summary":"returned result","artifacts":[{"path":"delivery.txt"},42]}`, "delivery.txt", ResultCollectionReceived, false, false},
		{"fabricated receipt", `{"status":"success","summary":"returned result","receipt_ids":["fabricated"]}`, "receipt", ResultCollectionReceived, false, false},
		{"fabricated receipt beside malformed entry", `{"status":"success","summary":"returned result","receipt_ids":[42,"fabricated"]}`, "receipt", ResultCollectionInFlight, false, false},
		{"unauthorized payload producer", `{"status":"success","summary":"returned result","structured_payload":{"references":[{"producer":"outside","paths":["source.go"]}]}}`, "must be a different accepted evidence_from task", ResultCollectionPrepared, false, false},
		{"unauthorized producer in repeated payload", `{"result":{"status":"success","summary":"returned result","structured_payload":{"references":[]},"structured_payload":{"references":[{"producer":"outside","paths":["source.go"]}]}},"extra":true}`, "must be a different accepted evidence_from task", ResultCollectionReceived, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			compiled := compileFixtureJSONSchema(t, "fanout-completion", schema)
			references := scenario.state == ResultCollectionPrepared || scenario.wrapped
			if references {
				compiled = compileFixtureJSONSchema(t, "fanout-references", collectionReferenceSchema)
			}
			c, item, _, receipt := resultCollectionTaskFixture(t, "http://127.0.0.1:1/v1", true, func(spec *TodoSpec) {
				spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
				if scenario.native || references {
					ref := compiled.ref(true)
					spec.ResultContract = &ref
				}
			}, func(c *Coordinator) {
				c.session.ResultContracts = map[string]*CompiledResultContract{compiled.ID: compiled}
			})
			_, _, first, err := beginFanoutWorkForRecovery(t, c, item, receipt, 0)
			if err != nil {
				t.Fatal(err)
			}
			leaf, ctx, affected, err := beginFanoutWorkForRecovery(t, c, item, receipt, 1)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.wrapped {
				affected, err = leaf.reserveResultCollection(ctx, affected, "local-attachment", []byte(`{"tools":[]}`))
				if err != nil {
					t.Fatal(err)
				}
				issue := &agent.StructuredResponseIssue{Code: "additional_property", Expected: "object", Actual: "object"}
				affected, err = leaf.recordCollectionResponseUsage(ctx, affected, []byte(scenario.body), ExecutionUsage{}, issue)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := leaf.prepareCollectionClaims(ctx, affected); localResultRejectionReason(err) != RepairFailureSource || !strings.Contains(err.Error(), scenario.detail) {
					t.Fatalf("live collection missed the unauthorized producer in repeated payloads: %v", err)
				}
			} else {
				affected = recordFanoutResponse(t, c, leaf, ctx, affected, scenario.state, scenario.body)
			}
			assertFanoutPreflightRejects(t, c, item, scenario.detail, first, affected)
		})
	}
}

func recordFanoutAttachmentResponse(t *testing.T, c, leaf *Coordinator, ctx context.Context, cp *ResultCollectionCheckpoint, state ResultCollectionState) *ResultCollectionCheckpoint {
	t.Helper()
	return recordFanoutResponse(t, c, leaf, ctx, cp, state, `{"status":"success","summary":"returned result","artifacts":[{"path":"delivery.txt"}]}`)
}

func recordFanoutResponse(t *testing.T, c, leaf *Coordinator, ctx context.Context, cp *ResultCollectionCheckpoint, state ResultCollectionState, body string) *ResultCollectionCheckpoint {
	t.Helper()
	cp, err := leaf.reserveResultCollection(ctx, cp, "local-attachment", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if state == ResultCollectionInFlight {
		execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_returned_attachment BEFORE INSERT ON events WHEN NEW.type='task_result_collection_checkpointed' AND json_extract(NEW.payload, '$.execution_receipt.result_collection.state')='received' BEGIN SELECT RAISE(ABORT, 'fixture response publication fault'); END`)
	}
	next, err := leaf.recordCollectionResponse(ctx, cp, []byte(body))
	if state == ResultCollectionInFlight {
		execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_returned_attachment")
		if localResultRejectionReason(err) != RepairFailureCommit {
			t.Fatalf("expected a response publication failure, got %v", err)
		}
		return cp
	}
	if err != nil {
		t.Fatal(err)
	}
	if state == ResultCollectionPrepared {
		next, err = leaf.prepareCollectionClaims(ctx, next)
		if err != nil {
			t.Fatal(err)
		}
	}
	return next
}

func TestResultCollectionFanoutRechecksRequiredReadOnCommittedLeaf(t *testing.T) {
	for _, missing := range []bool{true, false} {
		t.Run(map[bool]string{true: "missing", false: "corrupt"}[missing], func(t *testing.T) {
			c, item, _, receipt, _ := requiredReadTaskFixture(t, "required source", true, func(spec *TodoSpec) {
				spec.ExecutionTopology = append(spec.ExecutionTopology, spec.ExecutionTarget)
			})
			for index := range item.ExecutionTopology {
				leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, index)
				if err != nil {
					t.Fatal(err)
				}
				ctx = context.WithValue(ctx, artifactAccessScopeKey, receipt.ArtifactScope)
				ctx = withInvocationMetadata(ctx, InvocationMetadata{RunID: cp.SourceRunID, TaskID: cp.TaskID, AgentName: cp.Agent, Attempt: cp.Attempt, ModelExecutionID: cp.ModelExecutionID})
				ctx = tools.SetToolsAllowed(ctx, []string{"view"})
				view := tools.NewViewTool(tools.WithArtifactOpener(leaf.openArtifactRef))
				deliverRequiredRead(t, leaf, ctx, view, "read", fmt.Sprintf(`{"artifact_ref":%q}`, item.WorksetBinding.Inputs[0].ID))
				prepared := recordFanoutResponse(t, c, leaf, ctx, cp, ResultCollectionPrepared, `{"status":"success","summary":"read source"}`)
				// New dispatch rejects this topology. Seed historical accepted
				// slots through the original collection writer to exercise replay.
				if err := leaf.commitPreparedCollection(ctx, prepared); err != nil {
					t.Fatal(err)
				}
			}
			c = reopenRecoveryCoordinator(t, c)
			item = todoByID(t, c, item.ID)
			slots, _, err := c.fanoutSettlementSlots(t.Context(), item)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.sealFanoutSettlement(t.Context(), item, slots, "read source"); err != nil {
				t.Fatal(err)
			}
			execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_required_read_done BEFORE INSERT ON events WHEN NEW.type='task_completed' BEGIN SELECT RAISE(ABORT, 'fixture aggregate write fault'); END`)
			if _, err := c.commitFanoutOutput(t.Context(), item, "read source"); localResultRejectionReason(err) != RepairFailureCommit {
				t.Fatalf("expected first aggregate write fault: %v", err)
			}
			execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_required_read_done")
			c = reopenRecoveryCoordinator(t, c)
			item = todoByID(t, c, item.ID)
			first, second := currentFanoutCollection(item, 0), currentFanoutCollection(item, 1)
			if first == nil || second == nil || first.State != ResultCollectionCommitted || second.State != ResultCollectionCommitted {
				t.Fatalf("expected committed siblings: %+v %+v", first, second)
			}
			damageFanoutCAS(t, normalizedArtifactData(c, item.WorksetBinding.Inputs[0].ID), missing)
			assertFanoutPreflightRejects(t, c, item, "artifact_read_source_invalid", first, second)
		})
	}
}

func assertFanoutPreflightRejects(t *testing.T, c *Coordinator, item *TodoItem, detail string, checkpoints ...*ResultCollectionCheckpoint) {
	t.Helper()
	assertFanoutPreflightFailure(t, c, item, RepairFailureSource, detail, checkpoints...)
}

func assertFanoutPreflightFailure(t *testing.T, c *Coordinator, item *TodoItem, reason RepairFailureReason, detail string, checkpoints ...*ResultCollectionCheckpoint) {
	t.Helper()
	c = reopenRecoveryCoordinator(t, c)
	c.executionRunID = "cold-fanout-preflight"
	forbidden := scriptWorker(failTurn(errors.New("unverified sibling was dispatched")))
	c.workerAgentOverride, c.repairAgentOverride = forbidden, forbidden
	ctx, cancel := context.WithCancelCause(WithSessionResume(t.Context()))
	defer cancel(nil)
	c.setInvocationWatchdog(c.newInvocationWatchdog(ctx, cancel))
	defer c.setInvocationWatchdog(nil)
	c.providerBoundaryStarted = false
	var boundaries int
	c.providerBoundaryStart = func(context.Context, string) error {
		boundaries++
		return errors.New("unverified sibling crossed the provider boundary")
	}
	count, err := c.ResumeInterruptedTasks(ctx)
	after := todoByID(t, c, item.ID)
	if count != 1 || localResultRejectionReason(err) != reason || after.Status != TaskBlocked || boundaries != 0 ||
		forbidden.Calls() != 0 || !strings.Contains(err.Error(), detail) {
		t.Fatalf("count=%d status=%s boundaries=%d worker or repair=%d err=%v", count, after.Status, boundaries, forbidden.Calls(), err)
	}
	if reason == RepairFailureAction && (after.ExecutionReceipt.RepairProvenance == nil || after.ExecutionReceipt.RepairProvenance.FailureReason != reason) {
		t.Fatalf("preflight lost action failure provenance: %+v", after.ExecutionReceipt.RepairProvenance)
	}
	for index, cp := range checkpoints {
		if actual := currentFanoutCollection(after, index); !sameCanonicalEvidence(actual, cp) {
			t.Fatalf("failed preflight changed slot %d: original=%+v actual=%+v", index, cp, actual)
		}
	}
}

func faultExternalFanoutPublication(t *testing.T, c *Coordinator, item *TodoItem, scenario, path, content string) func() {
	t.Helper()
	switch scenario {
	case "leaf_document_write", "leaf_after_verified_sibling", "leaf_transcript_timestamp_conflict":
		execJournal(t, c.session.Workspace, `CREATE TRIGGER refuse_leaf_document BEFORE INSERT ON runtime_documents WHEN NEW.name LIKE 'result-collection-fanout-leaf:%' BEGIN SELECT RAISE(ABORT, 'fixture leaf document write fault'); END`)
		return func() { execJournal(t, c.session.Workspace, "DROP TRIGGER refuse_leaf_document") }
	case "leaf_artifact_write":
		ref := ArtifactRef{Path: path, Role: "delivery", SHA256: sha256HexBytes([]byte(content)), Bytes: int64(len(content)),
			RunID: c.contextRunID(), TaskID: item.ID, Attempt: 1, Agent: item.Agent, Provider: "codex"}
		digest, err := collectionDigest(ref)
		if err != nil {
			t.Fatal(err)
		}
		path := normalizedArtifactData(c, "external-result-"+digest)
		if err := os.Symlink(filepath.Base(path), path); err != nil {
			t.Fatal(err)
		}
		return func() {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
	default:
		return nil
	}
}

func checkFanoutVerifications(t *testing.T, c *Coordinator, want int) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(c.projectDir, "fanout-verification.log"))
	if err != nil || string(body) != strings.Repeat("verified\n", want) {
		t.Fatalf("objective verification expected %d executions, got %q: %v", want, body, err)
	}
}

func TestExternalFanoutArtifactCaptureAndReplayEnforceSizeLimits(t *testing.T) {
	for _, scenario := range []struct {
		name string
		refs []ArtifactRef
	}{
		{"individual", []ArtifactRef{{Path: "large.txt", Bytes: fanoutExternalArtifactMaxBytes + 1}}},
		{"aggregate", []ArtifactRef{
			{Path: "one.txt", Bytes: fanoutExternalArtifactMaxBytes},
			{Path: "two.txt", Bytes: fanoutExternalArtifactMaxBytes},
			{Path: "three.txt", Bytes: fanoutExternalArtifactMaxBytes},
			{Path: "four.txt", Bytes: fanoutExternalArtifactMaxBytes},
			{Path: "five.txt", Bytes: 1},
		}},
		{"count", make([]ArtifactRef, fanoutExternalArtifactsMaxCount+1)},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			missingWorld := filepath.Join(t.TempDir(), "unavailable")
			captureErr := stageExternalResultArtifacts(t.Context(), AttemptRequest{}, missingWorld, scenario.refs)
			replayErr := validateFanoutExternalArtifactSizes(scenario.refs)
			if !errors.Is(captureErr, ErrReadLimit) || !errors.Is(replayErr, ErrReadLimit) || localResultRejectionReason(captureErr) != RepairFailureSource {
				t.Fatalf("oversized evidence was not rejected before reading: capture=%v replay=%v", captureErr, replayErr)
			}
		})
	}
}

func acceptedExternalFanoutSource(t *testing.T, c *Coordinator, id string) ArtifactRef {
	t.Helper()
	c.initEventStore()
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		t.Fatal(err)
	}
	runID := c.contextRunID()
	artifact, err := store.Put(t.Context(), PutArtifactRequest{
		Path: "external-input.txt", Content: []byte("authorized external evidence"), RunID: runID, TaskID: id, Attempt: 1, Agent: "worker",
	})
	if err != nil {
		t.Fatal(err)
	}
	source := &TodoItem{ID: id, Agent: "worker", Status: TaskDone, TypedResult: &TaskResult{
		TaskID: id, Attempt: 1, Agent: "worker", Status: TaskResultStatusSuccess, Summary: "accepted source", Artifacts: []ArtifactRef{artifact.ArtifactRef},
	}}
	c.taskTracker.TodoList().AddReserved([]*TodoItem{source})
	for _, eventType := range []EventType{EventTaskCreated, EventTaskCompleted} {
		projected := cloneTodoItem(source)
		if eventType == EventTaskCreated {
			projected.Status = TaskPending
		}
		body, err := json.Marshal(projected, collectionDurationFormat)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = c.EventJournal().Append(t.Context(), RunEvent{Type: string(eventType), RunID: runID, BranchID: c.activeBranchID(), TaskID: id, Actor: source.Agent, Payload: body}); err != nil {
			t.Fatal(err)
		}
	}
	return artifact.ArtifactRef
}

func interruptFanoutCollectionBeforeReceived(t *testing.T, c *Coordinator, item *TodoItem, receipt ExecutionReceipt, index int) {
	t.Helper()
	workspace := c.session.Workspace
	leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, index)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaf.session.Workspace, "delivery.txt"), []byte("accepted fanout artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	cp, err = leaf.reserveResultCollection(ctx, cp, "local-collector", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	execJournal(t, workspace, `CREATE TRIGGER refuse_mixed_received BEFORE INSERT ON events WHEN NEW.type='task_result_collection_checkpointed' AND json_extract(NEW.payload, '$.execution_receipt.result_collection.state')='received' BEGIN SELECT RAISE(ABORT, 'fixture response publication fault'); END`)
	_, err = leaf.recordCollectionResponse(ctx, cp, []byte(`{"status":"success","summary":"exact local result","artifacts":[{"path":"delivery.txt"}]}`))
	execJournal(t, workspace, "DROP TRIGGER refuse_mixed_received")
	if localResultRejectionReason(err) != RepairFailureCommit {
		t.Fatalf("response append did not exercise durable handoff: %v", err)
	}
}

func beginUncollectedFanoutLeaf(t *testing.T, c *Coordinator, item *TodoItem, receipt ExecutionReceipt, index int, rejected bool) *ResultCollectionCheckpoint {
	t.Helper()
	leaf, ctx, cp, err := beginFanoutWorkForRecovery(t, c, item, receipt, index)
	if err != nil {
		t.Fatal(err)
	}
	if !rejected {
		return cp
	}
	cp, err = leaf.reserveResultCollection(ctx, cp, "local-rejected", []byte(`{"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = leaf.recordCollectionResponse(ctx, cp, []byte(`{"status":`))
	if err != nil {
		t.Fatal(err)
	}
	cp, err = leaf.recordCollectionFormatFailure(ctx, cp, errors.New("unfinished JSON"))
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func damageAcceptedFanoutAttachment(t *testing.T, c *Coordinator, taskID string, index int, scenario string) {
	t.Helper()
	item := todoByID(t, c, taskID)
	result, err := c.acceptedCollectionResult(t.Context(), item, currentFanoutCollection(item, index))
	if err != nil || len(result.Artifacts) != 1 {
		t.Fatalf("accepted attachment was not preserved before terminal append: result=%+v err=%v", result, err)
	}
	path := filepath.Join(c.artifactStoreRootPath(), logsDir, "artifacts", "data", result.Artifacts[0].ID)
	damageFanoutCAS(t, path, scenario == "missing_attachment")
}

func damageFanoutCAS(t *testing.T, path string, missing bool) {
	t.Helper()
	var err error
	if missing {
		err = os.Remove(path)
	} else {
		err = os.WriteFile(path, []byte("corrupted accepted artifact"), 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
}
