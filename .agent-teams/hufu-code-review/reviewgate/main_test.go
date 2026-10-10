package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/golangruntime"
	"github.com/kjelly/hufu/internal/team"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == golangruntime.ChildArg {
		os.Exit(golangruntime.RunChild(os.Args[2], os.Args[3], os.Stdin, os.Stdout, os.Stderr))
	}
	os.Exit(m.Run())
}

func raw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureTask(t *testing.T, id, contract, status string, value payload, r record, evidenceFrom []string) task {
	t.Helper()
	data, err := canonicalJSON(raw(t, value))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	// Only native actions carry materialized run-input snapshot metadata.
	var snapshotID string
	if contract == "produce-workset" || contract == "verify-targeted-go-tests" || contract == "inventory-review-handoffs" {
		snapshotID = "scope-1"
	}
	var item task
	if err := json.Unmarshal(raw(t, map[string]any{
		"ID": id, "Status": "done", "contract_id": contract,
		"evidence_from": evidenceFrom, "run_input_snapshot_id": snapshotID,
		"execution_receipt": map[string]any{"run_id": "run-1", "task_id": id,
			"attempt": 1, "producer_id": "reviewer", "model_execution_id": "execution-" + id,
			"transcript_ref": "transcript-" + id},
		"VerifyResult": map[string]any{"exit_code": 0, "fingerprint": "vfp-1"},
		"typed_result": map[string]any{"status": status, "findings": r.Findings,
			"files_read":         []map[string]string{{"path": "snapshot-a"}, {"path": "snapshot-b"}},
			"structured_payload": map[string]any{"value": value, "sha256": hex.EncodeToString(sum[:])}},
		"workset_binding": map[string]any{"bindings": map[string]string{"review_revision": r.Revision, "key": r.Key, "lens": r.Lens}},
	}), &item); err != nil {
		t.Fatal(err)
	}
	return item
}

func entry(item task, r record, t *testing.T) inputReview {
	return inputReview{TaskID: item.ID, Hash: item.Result.Payload.Hash, Record: raw(t, r)}
}

func gateFixture(t *testing.T) session {
	t.Helper()
	first := record{Kind: "review", Revision: "frozen-head", Key: "unit-0", Lens: "security-tool", Findings: []finding{
		{Summary: "Output limit off by one", Severity: "warning", Evidence: []string{"snapshot-a"}, RequiresCritique: true},
		{Summary: "Network classification", Severity: "info", Evidence: []string{"snapshot-a"}},
	}, Gaps: []json.RawMessage{raw(t, map[string]string{"description": "Registry not in this snapshot", "missing_evidence": "AllTools"})}}
	second := record{Kind: "review", Revision: "frozen-head", Key: "unit-1", Lens: "general", Findings: []finding{
		{Summary: "Prompt-only URL protection", Severity: "info", Evidence: []string{"snapshot-b"}, RequiresCritique: true},
	}, Gaps: []json.RawMessage{}}
	a := fixtureTask(t, "3", "review-primary-workset", "completed_with_gaps", payload{Record: raw(t, first)}, first, nil)
	b := fixtureTask(t, "4", "review-primary-workset", "success", payload{Record: raw(t, second)}, second, nil)
	c1 := record{Kind: "critique", InputReview: []inputReview{entry(a, first, t)}, Decisions: []decision{
		{FindingIndex: 0, Verdict: "confirmed", Evidence: []string{"snapshot-a"}},
		{FindingIndex: 1, Verdict: "rejected", Evidence: []string{"snapshot-a"}},
	}}
	c2 := record{Kind: "critique", InputReview: []inputReview{entry(b, second, t)}, Decisions: []decision{{FindingIndex: 0, Verdict: "unverified"}}}
	c := fixtureTask(t, "5", "critic-review", "success", payload{Record: raw(t, c1)}, c1, []string{"3"})
	d := fixtureTask(t, "6", "critic-review", "completed_with_gaps", payload{Record: raw(t, c2)}, c2, []string{"4"})
	scope := raw(t, map[string]any{"requested": map[string]any{"kind": "last_n", "count": 10, "history": "first_parent", "head": "HEAD"}, "resolved": map[string]any{"base": "base", "head": "frozen-head", "selected_commit_count": 10}, "satisfied": true})
	tests := raw(t, map[string]any{"passed": true, "reviewed_revision": "frozen-head", "command": []string{"go", "test"}, "artifact_id": "tests-artifact"})
	report := payload{Scope: scope, Tests: tests, Coverage: "completed_with_gaps", InputReview: []inputReview{entry(a, first, t), entry(b, second, t), entry(c, c1, t), entry(d, c2, t)}, GapResolutions: []gapResolution{
		{TaskID: "3", Index: 0, Status: "resolved_cross_workset", ResolvingTaskID: "4", Evidence: []string{"snapshot-b"}},
	}}
	e := fixtureTask(t, "7", "synthesize-review", "completed_with_gaps", report, record{}, []string{"3", "4", "5", "6"})
	prepare := fixtureTask(t, "1", "produce-workset", "success", payload{}, record{}, nil)
	prepare.Result.RuntimeOutputs = map[string]json.RawMessage{"scope": scope}
	verifier := fixtureTask(t, "2", "verify-targeted-go-tests", "success", payload{}, record{}, nil)
	verifier.Result.RuntimeOutputs = map[string]json.RawMessage{"targeted_go_tests": tests}
	fixture := session{SnapshotID: "scope-1", Tasks: []task{a, b, c, d, e, prepare, verifier}}
	setFixtureInventory(t, &fixture)
	return fixture
}

func setFixtureInventory(t *testing.T, fixture *session) {
	t.Helper()
	inventory, err := collectReviewHandoffs(*fixture, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	item := fixtureTask(t, "9", "inventory-review-handoffs", "success", payload{}, record{}, nil)
	item.SnapshotID = fixture.SnapshotID
	item.Result.RuntimeOutputs = map[string]json.RawMessage{"review_handoff_inventory": raw(t, inventory)}
	for i := range fixture.Tasks {
		if fixture.Tasks[i].ContractID == item.ContractID {
			fixture.Tasks[i] = item
			return
		}
	}
	fixture.Tasks = append(fixture.Tasks, item)
}

func replacePayload(t *testing.T, item *task, edit func(*payload)) {
	t.Helper()
	var p payload
	if err := json.Unmarshal(item.Result.Payload.Value, &p); err != nil {
		t.Fatal(err)
	}
	edit(&p)
	canonical, err := canonicalJSON(raw(t, p))
	if err != nil {
		t.Fatal(err)
	}
	item.Result.Payload.Value = canonical
	sum := sha256.Sum256(item.Result.Payload.Value)
	item.Result.Payload.Hash = hex.EncodeToString(sum[:])
}

func TestReviewGateCoverageAndRejectedHandoffs(t *testing.T) {
	tests := []struct {
		name string
		edit func(*session)
		want string
	}{
		{"valid_with_resolved_local_gap", func(*session) {}, ""},
		{"critic_missing_unit_1_evidence", func(s *session) { s.Tasks[3].EvidenceFrom = []string{"3"} }, "invalid input binding"},
		{"critic_combines_sources", func(s *session) { s.Tasks[2].EvidenceFrom = []string{"3", "4"} }, "exactly one source"},
		{"critic_skips_finding", func(s *session) {
			replacePayload(t, &s.Tasks[2], func(p *payload) {
				var r record
				_ = json.Unmarshal(p.Record, &r)
				r.Decisions = r.Decisions[:1]
				p.Record = raw(t, r)
			})
		}, "incomplete or duplicate finding coverage"},
		{"critic_substitutes_index", func(s *session) {
			replacePayload(t, &s.Tasks[2], func(p *payload) {
				var r record
				_ = json.Unmarshal(p.Record, &r)
				r.Decisions[1].FindingIndex = 0
				p.Record = raw(t, r)
			})
		}, "substituted or repeated"},
		{"report_skips_review", func(s *session) {
			replacePayload(t, &s.Tasks[4], func(p *payload) { p.InputReview = p.InputReview[1:] })
		}, "omitted review or critic"},
		{"report_rewrites_original_finding", func(s *session) {
			replacePayload(t, &s.Tasks[4], func(p *payload) {
				var r record
				_ = json.Unmarshal(p.InputReview[0].Record, &r)
				r.Findings[0].Summary = "safe"
				p.InputReview[0].Record = raw(t, r)
			})
		}, "substituted source record"},
		{"report_hides_original_gap", func(s *session) { replacePayload(t, &s.Tasks[4], func(p *payload) { p.GapResolutions = nil }) }, "omitted original gaps"},
		{"report_claims_complete", func(s *session) { replacePayload(t, &s.Tasks[4], func(p *payload) { p.Coverage = "complete" }) }, "overstated coverage"},
		{"report_invents_test_execution", func(s *session) {
			replacePayload(t, &s.Tasks[4], func(p *payload) {
				p.Tests = raw(t, map[string]any{"passed": true, "command": []string{"go", "test", "./..."}})
			})
		}, "substituted verify-targeted-go-tests"},
		{"report_changes_requested_scope", func(s *session) {
			replacePayload(t, &s.Tasks[4], func(p *payload) { p.Scope = raw(t, map[string]any{"requested": map[string]any{"count": 1}}) })
		}, "substituted produce-workset"},
		{"gap_resolution_without_evidence", func(s *session) {
			replacePayload(t, &s.Tasks[4], func(p *payload) { p.GapResolutions[0].Evidence = []string{"unread"} })
		}, "lacks observed cross-workset evidence"},
		{"stale_revision", func(s *session) { s.Tasks[0].Binding.Bindings["review_revision"] = "other-head" }, "changed its workset identity"},
		{"stale_run", func(s *session) { s.Tasks[0].Receipt.RunID = "old-run" }, "current accepted"},
		{"stale_worker_snapshot", func(s *session) { s.Tasks[0].SnapshotID = "old-snapshot" }, "current accepted"},
		{"missing_prepare_snapshot", func(s *session) { s.Tasks[5].SnapshotID = "" }, "PREPARE evidence"},
		{"missing_inventory", func(s *session) { s.Tasks = s.Tasks[:7] }, "inventory missing"},
		{"stale_inventory_snapshot", func(s *session) { s.Tasks[7].SnapshotID = "old-snapshot" }, "review handoff inventory"},
		{"wrong_receipt_task", func(s *session) { s.Tasks[0].Receipt.TaskID = "other-task" }, "current accepted"},
		{"missing_receipt_producer", func(s *session) { s.Tasks[0].Receipt.ProducerID = "" }, "current accepted"},
		{"missing_receipt_transcript", func(s *session) { s.Tasks[0].Receipt.TranscriptRef = "" }, "current accepted"},
		{"missing_verification_exit", func(s *session) { s.Tasks[0].VerifyResult.ExitCode = nil }, "current accepted"},
		{"failed_verification", func(s *session) { *s.Tasks[0].VerifyResult.ExitCode = 1 }, "current accepted"},
		{"timed_out_verification", func(s *session) { s.Tasks[0].VerifyResult.TimedOut = true }, "current accepted"},
		{"tampered_payload", func(s *session) { s.Tasks[0].Result.Payload.Hash = strings.Repeat("0", 64) }, "hash mismatch"},
		{"blocked_critic", func(s *session) { s.Tasks[3].Status = "error"; s.Tasks[3].Result.Status = "blocked" }, "current accepted"},
		{"gap_hidden_in_success_status", func(s *session) { s.Tasks[0].Result.Status = "success" }, "concealed its coverage status"},
		{"unknown_hidden_in_success_status", func(s *session) { s.Tasks[3].Result.Status = "success" }, "concealed unverified verdicts"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := gateFixture(t)
			test.edit(&s)
			_, _, err := verify(s, "run-1")
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %s", err, test.want)
			}
		})
	}
}

func TestReviewGateAcceptsCompleteReviewWithoutFindings(t *testing.T) {
	fixture := gateFixture(t)
	r := record{Kind: "review", Revision: "frozen-head", Key: "unit-0", Lens: "noop"}
	source := fixtureTask(t, "3", "review-primary-workset", "success", payload{Record: raw(t, r)}, r, nil)
	var original payload
	if err := json.Unmarshal(fixture.Tasks[4].Result.Payload.Value, &original); err != nil {
		t.Fatal(err)
	}
	report := payload{Scope: original.Scope, Tests: original.Tests, Coverage: "complete", InputReview: []inputReview{entry(source, r, t)}}
	final := fixtureTask(t, "4", "synthesize-review", "success", report, record{}, []string{"3"})
	fixture.Tasks = append([]task{source, final}, fixture.Tasks[5:]...)
	setFixtureInventory(t, &fixture)
	if id, _, err := verify(fixture, "run-1"); err != nil || id != "4" {
		t.Fatalf("id=%s error=%v", id, err)
	}
}

func TestReviewGateReadsNativeVerificationCheckpointFields(t *testing.T) {
	for _, test := range []struct {
		name   string
		result map[string]any
		valid  bool
	}{
		{"verified", map[string]any{"exit_code": 0, "fingerprint": "vfp-1"}, true},
		{"failed", map[string]any{"exit_code": 1, "fingerprint": "vfp-1"}, false},
		{"timed_out", map[string]any{"exit_code": 0, "timed_out": true, "fingerprint": "vfp-1"}, false},
		{"missing_exit", map[string]any{"fingerprint": "vfp-1"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := gateFixture(t)
			item := &fixture.Tasks[0]
			item.VerifyResult = nil
			if err := json.Unmarshal(raw(t, map[string]any{"VerifyResult": test.result}), item); err != nil {
				t.Fatal(err)
			}
			_, _, err := verify(fixture, "run-1")
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}

func TestReviewGateInvariantHandoff(t *testing.T) {
	var item task
	if err := json.Unmarshal(raw(t, map[string]any{"ID": "3", "typed_result": map[string]any{"invariant_verification": map[string]any{"assessments": []invariantAssessment{{ID: "lease-order", Status: "unknown", Summary: "Missing caller", MissingEvidence: []string{"setAttemptReceipt"}}}}}}), &item); err != nil {
		t.Fatal(err)
	}
	r := record{Invariants: []invariantAssessment{{ID: "lease-order", Status: "unknown", Summary: "Missing caller", MissingEvidence: []string{"setAttemptReceipt"}}}, Gaps: []json.RawMessage{raw(t, map[string]string{"missing_evidence": "setAttemptReceipt"})}}
	if err := verifyInvariants(item, r); err != nil {
		t.Fatal(err)
	}
	r.Gaps = nil
	if err := verifyInvariants(item, r); err == nil || !strings.Contains(err.Error(), "concealed an unknown invariant") {
		t.Fatalf("error=%v", err)
	}
	r.Invariants = nil
	if err := verifyInvariants(item, r); err == nil || !strings.Contains(err.Error(), "omitted invariant assessments") {
		t.Fatalf("error=%v", err)
	}
}

func TestReviewGateRejectsUnassignedRequiredCritique(t *testing.T) {
	s := gateFixture(t)
	s.Tasks = append(s.Tasks[:3], s.Tasks[4])
	if _, _, err := verify(s, "run-1"); err == nil || !strings.Contains(err.Error(), "unassigned required critique") {
		t.Fatalf("error=%v", err)
	}
}

func actionFixture(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	s := gateFixture(t)
	s.Tasks = append(s.Tasks, task{ID: "8", ContractID: "verify-review-report", Status: "in_progress", SnapshotID: s.SnapshotID})
	if err := os.WriteFile(filepath.Join(root, "session.json"), raw(t, s), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "runtime", "runs", "run-1", "actions", "action-1")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HUFU_WORKSPACE="+workspace, "HUFU_RUN_ID=run-1", "HUFU_ACTION_INVOCATION_ID=action-1", "HUFU_TASK_ID=8")
	return workspace, env
}

func TestEmbeddedReviewGateAndTeamContracts(t *testing.T) {
	teamDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := team.LoadTeam(teamDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := loaded.ProviderRegistry.Get("verify-review-report")
	if !ok {
		t.Fatal("report audit provider is not registered")
	}
	workspace, _ := actionFixture(t)
	ctx := team.WithActionEnvironment(t.Context(), team.ActionEnvironment{Workspace: workspace, RunID: "run-1", ActionInvocationID: "action-1", TaskID: "8"})
	result, err := provider.Execute(ctx, team.Action{Capability: "verify-review-report", Type: "verify_review_report", Payload: `{"scope":{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}}`})
	if err != nil {
		t.Fatalf("configured embedded gate: %v", err)
	}
	if !bytes.Contains(raw(t, result), []byte(`"passed":true`)) {
		t.Fatalf("output=%s", raw(t, result))
	}
	root, _, err := checkpointRoot(workspace, "run-1", "action-1")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	var current session
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatal(err)
	}
	replacePayload(t, &current.Tasks[4], func(p *payload) { p.InputReview = p.InputReview[1:] })
	if err := os.WriteFile(filepath.Join(root, "session.json"), raw(t, current), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Execute(ctx, team.Action{Capability: "verify-review-report", Type: "verify_review_report", Payload: `{"scope":{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}}`}); err == nil {
		t.Fatal("configured embedded gate accepted an incomplete report")
	}
}

func TestReviewGateActionIdentityFailsClosed(t *testing.T) {
	workspace, _ := actionFixture(t)
	for _, key := range []string{"HUFU_WORKSPACE", "HUFU_RUN_ID", "HUFU_ACTION_INVOCATION_ID", "HUFU_TASK_ID"} {
		t.Setenv(key, "")
	}
	t.Setenv("HUFU_WORKSPACE", workspace)
	t.Setenv("HUFU_RUN_ID", "wrong-run")
	t.Setenv("HUFU_ACTION_INVOCATION_ID", "action-1")
	t.Setenv("HUFU_TASK_ID", "8")
	if err := Run(t.Context(), bytes.NewReader(raw(t, team.Action{Type: "verify_review_report", Payload: `{"scope":{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}}`})), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "unsupported action workspace layout") {
		t.Fatalf("error=%v", err)
	}
}
