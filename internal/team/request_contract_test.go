package team

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func TestProjectDecisionReplaysRequestContractBinding(t *testing.T) {
	j := &memoryJournal{}
	if err := appendDecisionEvent(context.Background(), j, agent.EventRequestContractCommitted, decisionEvent{
		DecisionID: "dec-1", TaskID: "todo-1", ContractRef: "sha256:contract", ContractRevision: 3,
	}); err != nil {
		t.Fatal(err)
	}
	state, err := projectDecision(context.Background(), j, "dec-1")
	if err != nil {
		t.Fatal(err)
	}
	if state.TaskID != "todo-1" || state.ContractRef != "sha256:contract" || state.ContractRevision != 3 {
		t.Fatalf("replayed contract binding = %#v", state)
	}
}

func TestReduceToSessionDataReplaysRequestContractProjection(t *testing.T) {
	payload, err := json.Marshal(decisionEvent{DecisionID: "d1", TaskID: "t1", ContractRef: "sha256:contract", ContractRevision: 2, ContractArtifact: ArtifactRef{SHA256: "sha256:artifact"}})
	if err != nil {
		t.Fatal(err)
	}
	session := ReduceToSessionData([]RunEvent{{Type: agent.EventRequestContractCommitted, Payload: payload}})
	if len(session.RequestContractProjections) != 1 || session.RequestContractProjections[0].ContractRevision != 2 {
		t.Fatalf("contract projection was not replayed: %#v", session.RequestContractProjections)
	}
}

func TestBuildRequestContractHashesMetadataFreeRedactedMaterial(t *testing.T) {
	cfg := agent.RequestContractConfig{
		Objective:       "ship safely",
		SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "tests pass"}},
	}
	first, data, err := BuildRequestContract("token=secret", "Ship?", cfg, 1, time.Unix(10, 0))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := BuildRequestContract("token=secret", "Ship?", cfg, 9, time.Unix(99, 0))
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.MaterialHash != second.MaterialHash {
		t.Fatalf("metadata changed material identity: %q vs %q", first.ID, second.ID)
	}
	if strings.Contains(string(data), "secret") {
		t.Fatalf("contract artifact leaked secret: %s", data)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestContractConfigRejectsMalformedCriteria(t *testing.T) {
	for name, cfg := range map[string]agent.RequestContractConfig{
		"missing objective":   {Enabled: true, SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "ok"}}},
		"blank criterion":     {Enabled: true, Objective: "objective", SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "", Statement: "ok"}}},
		"duplicate criterion": {Enabled: true, Objective: "objective", SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "a"}, {ID: "S1", Statement: "b"}}},
	} {
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: malformed contract config was accepted", name)
		}
	}
}

func TestDecisionEnginePersistsSealedPacketBeforeEvent(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.RequestContractConfig{Objective: "ship safely", SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "tests pass"}}}
	envelope, data, err := BuildRequestContract("ship", "ship", cfg, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	contractArtifact, err := PersistRequestContract(context.Background(), store, envelope, data)
	if err != nil {
		t.Fatal(err)
	}
	contract := envelope.RequestContract()
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	engine := newTestEngineWithStages(journal, runner, nil, DecisionServices{Store: store})
	req := engineRequest(enginePolicy(1))
	req.Contract = &contract
	req.RequireRequestContract = true
	req.RequestContractRef = contractArtifact.ID
	req.RequestContractRevision = envelope.Revision
	req.RequestContractArtifact = contractArtifact
	record, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if record.EvidenceArtifactRef == nil || record.EvidenceArtifactRef.ID == "" {
		t.Fatalf("record evidence artifact ref = %#v", record.EvidenceArtifactRef)
	}
	reader, err := store.Open(context.Background(), record.EvidenceArtifactRef.ID)
	if err != nil {
		t.Fatalf("open persisted packet: %v", err)
	}
	defer func() { _ = reader.Close() }()
	var packet DecisionEvidencePacket
	if err := json.NewDecoder(reader).Decode(&packet); err != nil {
		t.Fatal(err)
	}
	if !packet.Sealed || packet.Hash != record.EvidenceHash {
		t.Fatalf("persisted packet = %#v, record = %#v", packet, record)
	}
	for _, event := range journal.events {
		if event.Type != agent.EventDecisionEvidenceSealed {
			continue
		}
		var payload decisionEvent
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.EvidenceArtifact.ID != record.EvidenceArtifactRef.ID {
			t.Fatalf("event evidence artifact = %#v, record = %#v", payload.EvidenceArtifact, record.EvidenceArtifactRef)
		}
		return
	}
	t.Fatal("sealed evidence event was not emitted")
}

func TestDecisionEngineRejectsUnresolvableBaseRateBeforeJudge(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	cfg := agent.RequestContractConfig{Objective: "ship safely", SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "tests pass"}}}
	envelope, data, err := BuildRequestContract("ship", "ship", cfg, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	contractArtifact, err := PersistRequestContract(context.Background(), store, envelope, data)
	if err != nil {
		t.Fatal(err)
	}
	contract := envelope.RequestContract()
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		t.Fatal("judge was called with an unresolved base rate")
		return DecisionOpinion{}, nil
	})
	engine := newTestEngineWithStages(journal, runner, nil, DecisionServices{Store: store})
	req := engineRequest(enginePolicy(1))
	req.Contract = &contract
	req.RequireRequestContract = true
	req.RequestContractRef = contractArtifact.ID
	req.RequestContractRevision = envelope.Revision
	req.RequestContractArtifact = contractArtifact
	req.BaseRates = []BaseRateEvidence{{ReferenceClass: "deployments", Metric: "success", SampleSize: 10, Distribution: DistributionSummary{Mean: .8, Median: .8, P10: .5, P90: .95}, Source: ArtifactRef{SHA256: "missing"}}}
	_, err = engine.Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionOutsideViewMissing) {
		t.Fatalf("Run = %v, want %s", err, ReasonDecisionOutsideViewMissing)
	}
	if len(runner.dispatched()) != 0 {
		t.Fatalf("judge calls = %v, want none", runner.dispatched())
	}
}

type referenceEvidenceTestRunner struct {
	calls int
	draft ReferenceEvidenceDraft
}

func (r *referenceEvidenceTestRunner) RunReferenceEvidence(context.Context, ReferenceEvidenceRequest) (ReferenceEvidenceDraft, error) {
	r.calls++
	return r.draft, nil
}

func TestReferenceEvidenceIsBoundedAndRunsBeforeJudge(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	envelope, data, err := BuildRequestContract("ship", "ship", agent.RequestContractConfig{Objective: "ship", SuccessCriteria: []agent.RequestSuccessCriterion{{ID: "S1", Statement: "works"}}}, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	contractArtifact, err := PersistRequestContract(context.Background(), store, envelope, data)
	if err != nil {
		t.Fatal(err)
	}
	contract := envelope.RequestContract()
	refRunner := &referenceEvidenceTestRunner{draft: ReferenceEvidenceDraft{SchemaVersion: ReferenceEvidenceSchemaVersion, Entries: []ReferenceBaseRateDraft{{ReferenceClass: "deployments", Metric: "success", SampleSize: 10, Distribution: DistributionSummary{Mean: .8, Median: .8, P10: .5, P90: .95}, Source: ReferenceSourceDeclaration{Name: "test source"}}}}}
	engine := newTestEngineWithStages(&memoryJournal{}, newRecordingRunner(func(string, int) (DecisionOpinion, error) { return scoredOpinion(8, 4, "migrate", .8), nil }), nil, DecisionServices{Store: store, ReferenceEvidence: refRunner})
	policy := enginePolicy(1)
	policy.OutsideView.Required = true
	policy.OutsideView.ReferenceEvidence = true
	req := engineRequest(policy)
	req.Contract = &contract
	req.RequireRequestContract = true
	req.RequestContractRef = contractArtifact.ID
	req.RequestContractRevision = envelope.Revision
	req.RequestContractArtifact = contractArtifact
	if _, err := engine.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if refRunner.calls != 1 {
		t.Fatalf("reference calls = %d, want 1", refRunner.calls)
	}
}

func TestDecisionEvidencePersistenceIsContentAddressedAndMaterialSensitive(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	req := engineRequest(enginePolicy(1))
	packet := DecisionEvidencePacket{
		ID: "decision-evidence", Question: req.Question, Options: req.Options,
		Criteria: req.Policy.Criteria, Facts: req.Facts, CreatedAt: time.Unix(10, 0).UTC(),
	}
	sealed, err := packet.Seal()
	if err != nil {
		t.Fatal(err)
	}
	first, err := persistDecisionEvidence(context.Background(), store, req, sealed)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := persistDecisionEvidence(context.Background(), store, req, sealed)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != reused.ID || !strings.HasPrefix(first.ID, "sha256-") {
		t.Fatalf("same packet did not reuse content address: first=%#v reused=%#v", first, reused)
	}
	changed := sealed
	changed.Options = append([]DecisionOption(nil), sealed.Options...)
	changed.Options[0].Title = changed.Options[0].Title + " changed"
	changed, err = changed.Seal()
	if err != nil {
		t.Fatal(err)
	}
	second, err := persistDecisionEvidence(context.Background(), store, req, changed)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatalf("material packet change reused artifact ID %q", first.ID)
	}
}
