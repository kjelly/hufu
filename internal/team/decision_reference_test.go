package team

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func validReferenceDraft() ReferenceEvidenceDraft {
	return ReferenceEvidenceDraft{
		SchemaVersion: ReferenceEvidenceSchemaVersion,
		Entries: []ReferenceBaseRateDraft{{
			ReferenceClass: "deployments",
			Metric:         "success",
			SampleSize:     20,
			Distribution:   DistributionSummary{Mean: .8, Median: .8, P10: .5, P90: .95},
			Source:         ReferenceSourceDeclaration{Name: "operations report", Locator: "/model-supplied/path"},
		}},
	}
}

func TestReferenceEvidenceDraftValidationIsBounded(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ReferenceEvidenceDraft)
		want   string
	}{
		{"schema", func(d *ReferenceEvidenceDraft) { d.SchemaVersion = 2 }, "schema version"},
		{"empty entries", func(d *ReferenceEvidenceDraft) { d.Entries = nil }, "1-8 entries"},
		{"too many entries", func(d *ReferenceEvidenceDraft) {
			d.Entries = make([]ReferenceBaseRateDraft, 9)
			for i := range d.Entries {
				d.Entries[i] = validReferenceDraft().Entries[0]
			}
		}, "1-8 entries"},
		{"sample", func(d *ReferenceEvidenceDraft) { d.Entries[0].SampleSize = 0 }, "real sample"},
		{"non finite", func(d *ReferenceEvidenceDraft) { d.Entries[0].Distribution.Mean = math.NaN() }, "not finite"},
		{"bounded string", func(d *ReferenceEvidenceDraft) {
			d.Entries[0].ReferenceClass = strings.Repeat("x", ReferenceEvidenceMaxStringBytes+1)
		}, "exceeds"},
		{"bounded limitation", func(d *ReferenceEvidenceDraft) {
			d.Entries[0].Limitations = []string{strings.Repeat("x", ReferenceEvidenceMaxLimitationBytes+1)}
		}, "limitations[0]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			draft := validReferenceDraft()
			test.mutate(&draft)
			if err := draft.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate = %v, want %q", err, test.want)
			}
		})
	}
}

func TestReferenceEvidenceStrictDecoderRejectsUnknownAndTrailingJSON(t *testing.T) {
	payload, err := json.Marshal(validReferenceDraft())
	if err != nil {
		t.Fatal(err)
	}
	accepted := []string{
		string(payload),
		"```json\n" + string(payload) + "\n```",
	}
	for _, response := range accepted {
		var draft ReferenceEvidenceDraft
		if err := decodeReferenceEvidence(response, &draft); err != nil {
			t.Fatalf("decodeReferenceEvidence rejected %q: %v", response, err)
		}
	}
	for _, response := range []string{
		`prose {"schema_version":1,"entries":[]}`,
		"```json\n" + string(payload) + "\n```\ncommentary",
		"```json\n" + string(payload) + "\n```\n```json\n" + string(payload) + "\n```",
		string(payload) + " {\"schema_version\":1}",
		`{"schema_version":1,"entries":[],"options":["forbidden"]}`,
		"```json\n" + string(payload) + "\n``` trailing",
	} {
		var draft ReferenceEvidenceDraft
		if err := decodeReferenceEvidence(response, &draft); err == nil {
			t.Fatalf("decodeReferenceEvidence accepted %q", response)
		}
	}
}

func TestReferenceEvidenceProducerRawLimitUsesWholeResponse(t *testing.T) {
	payload, err := json.Marshal(validReferenceDraft())
	if err != nil {
		t.Fatal(err)
	}
	accepted := string(payload) + strings.Repeat(" ", ReferenceEvidenceProducerMaxRawBytes-len(payload))
	var draft ReferenceEvidenceDraft
	if err := decodeReferenceEvidence(accepted, &draft); err != nil {
		t.Fatalf("decodeReferenceEvidence rejected exact producer boundary: %v", err)
	}
	if err := decodeReferenceEvidence(accepted+" ", &draft); err == nil {
		t.Fatal("decodeReferenceEvidence accepted producer response over the limit")
	}
}

func TestReferenceEvidenceRuntimeEnvelopeLimitsUseMatchingWriterAndReader(t *testing.T) {
	entry := ReferenceEvidenceArtifact{SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: "invocation"}
	for len(mustReferenceEvidenceEntryBytes(t, entry)) <= ReferenceEvidenceMaxEntryBytes {
		entry.InvocationID += "x"
	}
	entry.InvocationID = entry.InvocationID[:len(entry.InvocationID)-1]
	entryBytes := mustReferenceEvidenceEntryBytes(t, entry)
	if len(entryBytes) > ReferenceEvidenceMaxEntryBytes {
		t.Fatalf("entry boundary is %d bytes, limit %d", len(entryBytes), ReferenceEvidenceMaxEntryBytes)
	}
	if _, err := decodeReferenceEvidenceEntry(bytes.NewReader(entryBytes)); err != nil {
		t.Fatalf("entry reader rejected writer output at boundary: %v", err)
	}
	entry.InvocationID += "x"
	if _, err := referenceEvidenceEntryBytes(entry); err == nil {
		t.Fatal("entry writer accepted an oversized runtime wrapper")
	}

	result := ReferenceEvidenceResult{SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: "invocation", InputHash: "hash"}
	for len(mustReferenceEvidenceResultBytes(t, result)) <= ReferenceEvidenceMaxResultBytes {
		result.Provenance = append(result.Provenance, EvidenceProvenance{SourceID: strings.Repeat("x", len(result.Provenance)+1)})
	}
	result.Provenance = result.Provenance[:len(result.Provenance)-1]
	resultBytes := mustReferenceEvidenceResultBytes(t, result)
	if len(resultBytes) > ReferenceEvidenceMaxResultBytes {
		t.Fatalf("result boundary is %d bytes, limit %d", len(resultBytes), ReferenceEvidenceMaxResultBytes)
	}
	if _, err := decodeReferenceEvidenceResult(bytes.NewReader(resultBytes)); err != nil {
		t.Fatalf("result reader rejected writer output at boundary: %v", err)
	}
	result.Provenance = append(result.Provenance, EvidenceProvenance{SourceID: strings.Repeat("x", ReferenceEvidenceMaxStringBytes)})
	if _, err := referenceEvidenceResultBytes(result); err == nil {
		t.Fatal("result writer accepted an oversized runtime envelope")
	}
}

func mustReferenceEvidenceEntryBytes(t *testing.T, entry ReferenceEvidenceArtifact) []byte {
	t.Helper()
	data, err := referenceEvidenceEntryBytes(entry)
	if err != nil {
		if len(entry.InvocationID) > 0 {
			return bytes.Repeat([]byte{'x'}, ReferenceEvidenceMaxEntryBytes+1)
		}
		t.Fatal(err)
	}
	return data
}

func mustReferenceEvidenceResultBytes(t *testing.T, result ReferenceEvidenceResult) []byte {
	t.Helper()
	data, err := referenceEvidenceResultBytes(result)
	if err != nil {
		return bytes.Repeat([]byte{'x'}, ReferenceEvidenceMaxResultBytes+1)
	}
	return data
}

func TestReferenceEvidenceRequestContainsOnlyProducerInput(t *testing.T) {
	request := ReferenceEvidenceRequest{
		SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: "reference-1",
		DecisionID: "decision-1", RunID: "run-1", TaskID: "task-1", Question: "Ship?",
		ContractRef: "contract-1", ContractRevision: 2,
	}
	hash, err := request.ComputeInputHash()
	if err != nil {
		t.Fatal(err)
	}
	request.InputHash = hash
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"options", "opinions", "aggregate", "memory", "media_type", "sha256", "path"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("producer request contains forbidden field %q: %s", forbidden, encoded)
		}
	}
}

func TestReferenceEvidencePromptDeclaresTypedProducerContract(t *testing.T) {
	prompt := referenceEvidencePrompt(`{"schema_version":1}`)
	for _, required := range []string{"ReferenceEvidenceDraft", "schema_version", "entries", "Evidence only", "ArtifactRef", "media type"} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("producer prompt missing %q: %s", required, prompt)
		}
	}
	if strings.Contains(prompt, "options") && !strings.Contains(prompt, "do not select options") {
		t.Fatal("producer prompt does not constrain option selection")
	}
}

type referenceDraftRunner struct {
	draft   ReferenceEvidenceDraft
	err     error
	calls   int
	request ReferenceEvidenceRequest
}

func (r *referenceDraftRunner) RunReferenceEvidence(_ context.Context, request ReferenceEvidenceRequest) (ReferenceEvidenceDraft, error) {
	r.calls++
	r.request = request
	return r.draft, r.err
}

func TestReferenceEvidencePublishesRuntimeOwnedCASArtifacts(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	j := &memoryJournal{}
	draft := validReferenceDraft()
	draft.AgentID = "resolved-worker"
	refRunner := &referenceDraftRunner{draft: draft}
	judge := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 4, "migrate", .8), nil
	})
	policy := enginePolicy(1)
	policy.OutsideView.Required = true
	policy.OutsideView.ReferenceEvidence = true
	engine := newTestEngineWithStages(j, judge, nil, DecisionServices{Store: store, ReferenceEvidence: refRunner})

	record, err := engine.Run(context.Background(), engineRequest(policy))
	if err != nil {
		t.Fatal(err)
	}
	if refRunner.calls != 1 || refRunner.request.InputHash == "" {
		t.Fatalf("reference request = %#v, calls = %d", refRunner.request, refRunner.calls)
	}
	if refRunner.request.ContractRef != "" {
		t.Fatalf("unexpected contract ref in request: %#v", refRunner.request)
	}
	if record.EvidenceArtifactRef == nil {
		t.Fatal("decision did not retain its sealed evidence artifact")
	}
	if record.ReferenceEvidenceResultRef == nil {
		t.Fatal("decision did not retain its reference result artifact")
	}

	var completed ReferenceEvidenceResult
	var started, completedAt, sealedAt int
	for index, eventType := range j.typesOf() {
		switch eventType {
		case agent.EventDecisionReferenceStarted:
			started = index
		case agent.EventDecisionReferenceCompleted:
			completedAt = index
			events, _ := j.ReadEvents(context.Background())
			if err := json.Unmarshal(events[index].Payload, &struct {
				ReferenceResult *ReferenceEvidenceResult `json:"reference_result"`
			}{ReferenceResult: &completed}); err != nil {
				t.Fatal(err)
			}
		case agent.EventDecisionEvidenceSealed:
			sealedAt = index
		}
	}
	if started >= completedAt || completedAt >= sealedAt || len(completed.Artifacts) != 1 || completed.ResultArtifactRef == nil {
		t.Fatalf("reference event order/result = started:%d completed:%d sealed:%d result:%#v", started, completedAt, sealedAt, completed)
	}
	ref := completed.Artifacts[0]
	if ref.ID == "" || ref.SHA256 == "" || ref.MediaType != ReferenceEvidenceMediaType || ref.Path == "/model-supplied/path" {
		t.Fatalf("CAS reference is not runtime-owned: %#v", ref)
	}
	if completed.BaseRates[0].Source.ID != ref.ID {
		t.Fatalf("base-rate source = %#v, artifact = %#v", completed.BaseRates[0].Source, ref)
	}
	if err := store.Verify(context.Background(), ref); err != nil {
		t.Fatalf("published reference artifact failed verification: %v", err)
	}
	resultRef := *completed.ResultArtifactRef
	if resultRef.MediaType != ReferenceEvidenceResultMediaType || resultRef.Kind != "reference_evidence_result" || resultRef.Path != "decisions/reference-evidence/"+completed.InvocationID+"/result.json" {
		t.Fatalf("result CAS reference is not fixed/runtime-owned: %#v", resultRef)
	}
	if err := store.Verify(context.Background(), resultRef); err != nil {
		t.Fatalf("published result artifact failed verification: %v", err)
	}
	if record.ReferenceEvidenceResultRef.ID != resultRef.ID {
		t.Fatalf("record result ref = %#v, event result ref = %#v", record.ReferenceEvidenceResultRef, resultRef)
	}
	if completed.ProducerAgentID != "resolved-worker" {
		t.Fatalf("ProducerAgentID = %q, want %q (durable AgentBinding must survive publication)", completed.ProducerAgentID, "resolved-worker")
	}
}

func TestReferenceEvidenceResumeRequiresValidResultEnvelopeAndReusesIt(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "reuse", true: "corrupt"}[corrupt], func(t *testing.T) {
			workspace := t.TempDir()
			store, err := NewFileArtifactStore(workspace, workspace)
			if err != nil {
				t.Fatal(err)
			}
			j := &memoryJournal{}
			request := engineRequest(enginePolicy(1))
			request.DecisionID = "decision-result-resume"
			policy := request.Policy
			policy.OutsideView.Required = true
			policy.OutsideView.ReferenceEvidence = true
			request.Policy = policy
			firstRunner := &referenceDraftRunner{draft: validReferenceDraft()}
			first := newTestEngineWithStages(j, newRecordingRunner(func(string, int) (DecisionOpinion, error) { return scoredOpinion(8, 4, "migrate", .8), nil }), nil, DecisionServices{Store: store, ReferenceEvidence: firstRunner})
			if _, err := first.Run(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			events, err := j.ReadEvents(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			completedIndex := -1
			var result ReferenceEvidenceResult
			for i, event := range events {
				if event.Type == agent.EventDecisionReferenceCompleted {
					completedIndex = i
					if err := json.Unmarshal(event.Payload, &struct {
						ReferenceResult *ReferenceEvidenceResult `json:"reference_result"`
					}{ReferenceResult: &result}); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			if completedIndex < 0 || result.ResultArtifactRef == nil {
				t.Fatal("missing completed result envelope")
			}
			j.events = events[:completedIndex+1]
			if corrupt {
				if err := os.WriteFile(filepath.Join(store.root, "data", result.ResultArtifactRef.ID), []byte(`{"schema_version":1,"entries":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			secondRunner := &referenceDraftRunner{draft: validReferenceDraft()}
			second := newTestEngineWithStages(j, newRecordingRunner(func(string, int) (DecisionOpinion, error) { return scoredOpinion(8, 4, "migrate", .8), nil }), nil, DecisionServices{Store: store, ReferenceEvidence: secondRunner})
			_, err = second.Run(context.Background(), request)
			if corrupt {
				if err == nil || !strings.Contains(err.Error(), "reference evidence result") {
					t.Fatalf("corrupt resume error = %v", err)
				}
				if secondRunner.calls != 0 {
					t.Fatalf("corrupt resume producer calls = %d", secondRunner.calls)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if secondRunner.calls != 0 {
				t.Fatalf("resume producer calls = %d, want reuse", secondRunner.calls)
			}
		})
	}
}

func TestReferenceEvidenceStartedOrFailedCannotReplay(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "started", true: "failed"}[failed], func(t *testing.T) {
			j := &memoryJournal{}
			request := engineRequest(enginePolicy(1))
			request.DecisionID = "decision-recovery"
			invocation := ReferenceEvidenceInvocation{
				SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: "reference-recovery",
				InputHash: "input-hash", DecisionID: request.DecisionID, Question: request.Question,
			}
			if err := appendDecisionEvent(context.Background(), j, agent.EventDecisionStarted, decisionEvent{DecisionID: request.DecisionID, Profile: request.Profile}); err != nil {
				t.Fatal(err)
			}
			if err := appendDecisionEvent(context.Background(), j, agent.EventDecisionReferenceStarted, decisionEvent{DecisionID: request.DecisionID, ReferenceInvocation: &invocation}); err != nil {
				t.Fatal(err)
			}
			if failed {
				failure := &ReferenceEvidenceFailure{SchemaVersion: ReferenceEvidenceSchemaVersion, InvocationID: invocation.InvocationID, InputHash: invocation.InputHash, Reason: "failed"}
				if err := appendDecisionEvent(context.Background(), j, agent.EventDecisionReferenceFailed, decisionEvent{DecisionID: request.DecisionID, ReferenceFailure: failure}); err != nil {
					t.Fatal(err)
				}
			}
			runner := &referenceDraftRunner{draft: validReferenceDraft()}
			engine := newTestEngineWithStages(j, newRecordingRunner(func(string, int) (DecisionOpinion, error) { return scoredOpinion(8, 4, "migrate", .8), nil }), nil, DecisionServices{ReferenceEvidence: runner})
			if _, err := engine.Run(context.Background(), request); err == nil || !strings.Contains(err.Error(), ReasonDecisionOutsideViewMissing) {
				t.Fatalf("Run = %v, want fail-closed outside-view error", err)
			}
			if runner.calls != 0 {
				t.Fatalf("reference producer calls = %d, want no replay", runner.calls)
			}
		})
	}
}
