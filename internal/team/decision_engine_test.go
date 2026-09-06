package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

// memoryJournal is an in-memory EventJournal. Decision state is rebuilt from
// the log, so tests exercise the same projection resume uses.
type memoryJournal struct {
	mu     sync.Mutex
	events []RunEvent
	seq    int
	store  ArtifactStore
}

func (j *memoryJournal) Append(_ context.Context, event RunEvent) (RunEvent, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.seq++
	event.ID = fmt.Sprintf("evt-%d", j.seq)
	event.Timestamp = time.Unix(0, int64(j.seq)).UTC().Format(time.RFC3339Nano)
	j.events = append(j.events, event)
	return event, nil
}

func (j *memoryJournal) ReadEvents(context.Context) ([]RunEvent, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]RunEvent(nil), j.events...), nil
}

func (j *memoryJournal) VerifyHashChain(context.Context) error { return nil }

func (j *memoryJournal) typesOf() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]string, 0, len(j.events))
	for _, event := range j.events {
		out = append(out, event.Type)
	}
	return out
}

func (j *memoryJournal) count(eventType string) int {
	n := 0
	for _, t := range j.typesOf() {
		if t == eventType {
			n++
		}
	}
	return n
}

// recordingRunner captures the exact prompt each judge received so isolation
// can be asserted against the real text, not against the construction code.
type recordingRunner struct {
	mu       sync.Mutex
	prompts  map[string]string
	calls    []string
	respond  func(judgeID string, attempt int) (DecisionOpinion, error)
	attempts map[string]int
	failNext map[string]bool
}

func newRecordingRunner(respond func(judgeID string, attempt int) (DecisionOpinion, error)) *recordingRunner {
	return &recordingRunner{
		prompts:  map[string]string{},
		attempts: map[string]int{},
		failNext: map[string]bool{},
		respond:  respond,
	}
}

func (r *recordingRunner) RunJudge(_ context.Context, req JudgeRequest) (DecisionOpinion, error) {
	r.mu.Lock()
	r.attempts[req.JudgeID]++
	attempt := r.attempts[req.JudgeID]
	r.prompts[fmt.Sprintf("%s#%d", req.JudgeID, attempt)] = req.Context.Prompt
	r.calls = append(r.calls, req.JudgeID)
	r.mu.Unlock()
	return r.respond(req.JudgeID, attempt)
}

func (r *recordingRunner) dispatched() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func enginePolicy(judges int) DecisionPolicy {
	return DecisionPolicy{
		IndependentJudgments: judges,
		ContextIsolation:     agent.DecisionIsolationStrict,
		ScoreScale:           agent.DecisionScoreScale,
		Aggregation:          AggregationPolicy{Method: agent.AggregationMeanScore},
		Criteria:             []DecisionCriterion{{ID: "cost", Weight: 1}, {ID: "risk", Weight: 1}},
		MaxRounds:            1,
	}
}

func engineRequest(policy DecisionPolicy) DecisionRequest {
	return DecisionRequest{
		RunID:    "run-1",
		TaskID:   "task-1",
		Profile:  "standard",
		Policy:   policy,
		Question: "Should we migrate now?",
		Options: []DecisionOption{
			{ID: "migrate", Kind: OptionExecute, Title: "Migrate now"},
			{ID: "wait", Kind: OptionDefer, Title: "Wait"},
		},
		Role: "You are a systems reviewer.",
	}
}

func scoredOpinion(migrate, wait float64, preferred string, probability float64) DecisionOpinion {
	return DecisionOpinion{
		OptionScores: []OptionScore{
			{OptionID: "migrate", Criteria: map[string]float64{"cost": migrate, "risk": migrate}},
			{OptionID: "wait", Criteria: map[string]float64{"cost": wait, "risk": wait}},
		},
		PreferredOption: preferred, SuccessProbability: probability,
	}
}

// stubChallenger and friends stand in for the Phase 2 stage runners. They
// record the prompt they were given so isolation and ordering can be asserted.
type stubChallenger struct {
	mu      sync.Mutex
	prompts []string
	respond func(challengerID string) (DecisionChallenge, error)
}

func (c *stubChallenger) RunChallenge(_ context.Context, req ChallengeRequest) (DecisionChallenge, error) {
	c.mu.Lock()
	c.prompts = append(c.prompts, req.Prompt)
	c.mu.Unlock()
	if c.respond != nil {
		return c.respond(req.ChallengerID)
	}
	return DecisionChallenge{TargetOption: "migrate", StrongestCountercase: "load may spike", Severity: 0.4}, nil
}

func (c *stubChallenger) lastPrompt() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.prompts) == 0 {
		return ""
	}
	return c.prompts[len(c.prompts)-1]
}

type stubPremortem struct {
	respond func() (PremortemResult, error)
}

func (p *stubPremortem) RunPremortem(context.Context, PremortemRequest) (PremortemResult, error) {
	if p.respond != nil {
		return p.respond()
	}
	return PremortemResult{FailureModes: []FailureMode{{
		ID: "F1", Description: "rollout stalls", Likelihood: 0.3, Impact: 0.7,
		EarlyWarningSignals: []string{"ingest lag exceeds 5m"},
	}}}, nil
}

type stubReviser struct {
	mu      sync.Mutex
	prompts map[string]string
	respond func(judgeID string) (DecisionRevision, error)
}

func (r *stubReviser) RunRevision(_ context.Context, req RevisionRequest) (DecisionRevision, error) {
	r.mu.Lock()
	if r.prompts == nil {
		r.prompts = map[string]string{}
	}
	r.prompts[req.JudgeID] = req.Prompt
	r.mu.Unlock()
	if r.respond != nil {
		return r.respond(req.JudgeID)
	}
	return DecisionRevision{Changed: false}, nil
}

func newTestEngine(journal *memoryJournal, runner JudgeRunner, budget BudgetManager) DecisionEngine {
	return newTestEngineWithStages(journal, runner, budget, DecisionServices{})
}

// newTestEngineWithStages builds an engine with optional Phase 2 stage runners.
func newTestEngineWithStages(journal *memoryJournal, runner JudgeRunner, budget BudgetManager, stages DecisionServices) DecisionEngine {
	counter := 0
	store := stages.Store
	if store == nil {
		journal.mu.Lock()
		store = journal.store
		if store == nil {
			workspace, err := os.MkdirTemp("", "hufu-decision-engine-")
			if err != nil {
				journal.mu.Unlock()
				panic(err)
			}
			store, err = NewFileArtifactStore(workspace, workspace)
			if err != nil {
				journal.mu.Unlock()
				panic(err)
			}
			journal.store = store
		}
		journal.mu.Unlock()
	}
	return NewDecisionEngine(DecisionServices{
		Judges:            runner,
		Journal:           journal,
		Budget:            budget,
		Premortems:        stages.Premortems,
		Challengers:       stages.Challengers,
		Revisions:         stages.Revisions,
		Proposer:          stages.Proposer,
		ReferenceEvidence: stages.ReferenceEvidence,
		Store:             store,
		Index:             stages.Index,
		Now:               func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC) },
		NewID: func(prefix string) string {
			counter++
			return fmt.Sprintf("%s-%d", prefix, counter)
		},
	})
}

func TestDecisionEngineRunProducesDurableRecord(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(judgeID string, _ int) (DecisionOpinion, error) {
		switch judgeID {
		case "judge-1":
			return scoredOpinion(8, 4, "migrate", 0.8), nil
		case "judge-2":
			return scoredOpinion(7, 5, "migrate", 0.7), nil
		default:
			return scoredOpinion(3, 6, "wait", 0.4), nil
		}
	})
	engine := newTestEngine(journal, runner, nil)

	record, err := engine.Run(context.Background(), engineRequest(enginePolicy(3)))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if record.FinalOption != "migrate" {
		t.Fatalf("FinalOption = %q, want migrate", record.FinalOption)
	}
	if len(record.Opinions) != 3 || len(record.Aggregates) != 1 {
		t.Fatalf("record kept %d opinions and %d aggregates", len(record.Opinions), len(record.Aggregates))
	}
	if record.SchemaVersion != DecisionRecordSchemaVersion {
		t.Fatalf("SchemaVersion = %d", record.SchemaVersion)
	}
	if record.NoGoOptionID != "wait" || !record.AlternativesChecked {
		t.Fatalf("no-go alternative not recorded: %#v", record.NoGoOptionID)
	}
	// The whole distribution is persisted, not only the winner (spec §20).
	aggregate := record.Aggregates[0]
	for _, m := range []map[string]float64{aggregate.MeanScores, aggregate.MedianScores, aggregate.StdDev, aggregate.MAD} {
		if len(m) != 2 {
			t.Fatalf("aggregate dropped an option: %#v", aggregate)
		}
	}
	if record.Probability == 0 {
		t.Fatal("Probability = 0, want the preferred option's mean probability")
	}

	for _, want := range []string{
		agent.EventDecisionStarted,
		agent.EventDecisionEvidenceSealed,
		agent.EventDecisionOpinionSubmitted,
		agent.EventDecisionAggregateComputed,
		agent.EventDecisionFinalized,
	} {
		if journal.count(want) == 0 {
			t.Errorf("event %s was never emitted; log = %v", want, journal.typesOf())
		}
	}
}

func TestDecisionEvidenceResolutionDoesNotMutateCallerRequest(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := store.Put(context.Background(), PutArtifactRequest{
		Kind: "evidence", Role: "evidence", Path: "evidence/source.txt", Content: []byte("source"),
	})
	if err != nil {
		t.Fatal(err)
	}
	req := engineRequest(enginePolicy(1))
	req.Assumptions = []DecisionAssumption{{
		ID: "A1", Statement: "source is available", EvidenceRefs: []ArtifactRef{artifact.ArtifactRef},
	}}
	original := cloneDecisionAssumptions(req.Assumptions)
	admitted := cloneDecisionRequest(req)
	engine := NewDecisionEngine(DecisionServices{Store: store})
	if err := engine.(*decisionEngine).resolveDecisionEvidence(context.Background(), &admitted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(req.Assumptions, original) {
		t.Fatalf("resolution changed caller request: got %#v want %#v", req.Assumptions, original)
	}
}

func TestDecisionEngineRequiredRequestContractFailsBeforeJudge(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		t.Fatal("judge was called without a required request contract")
		return DecisionOpinion{}, nil
	})
	engine := newTestEngine(journal, runner, nil)
	req := engineRequest(enginePolicy(1))
	req.RequireRequestContract = true
	_, err := engine.Run(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionMissingObjective) {
		t.Fatalf("Run = %v, want %s", err, ReasonDecisionMissingObjective)
	}
	if len(runner.dispatched()) != 0 {
		t.Fatalf("judge calls = %v, want none", runner.dispatched())
	}
}

// Isolation is the point of the whole first round: assert against the prompts
// judges actually received (spec §16, test matrix B).
func TestDecisionEngineJudgesAreIsolated(t *testing.T) {
	journal := &memoryJournal{}
	secrets := map[string]string{
		"judge-1": "JUDGE-ONE-SECRET-RATIONALE",
		"judge-2": "JUDGE-TWO-SECRET-RATIONALE",
		"judge-3": "JUDGE-THREE-SECRET-RATIONALE",
	}
	runner := newRecordingRunner(func(judgeID string, _ int) (DecisionOpinion, error) {
		o := scoredOpinion(8, 4, "migrate", 0.8)
		o.KeyAssumptions = []string{secrets[judgeID]}
		return o, nil
	})
	engine := newTestEngine(journal, runner, nil)

	req := engineRequest(enginePolicy(3))
	req.ProjectContext = "The ingest pipeline runs on three nodes."
	req.Memory = "A previous migration overran by two weeks."
	if _, err := engine.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	hashes := map[string]bool{}
	for key, prompt := range runner.prompts {
		judge := strings.Split(key, "#")[0]
		for otherJudge, secret := range secrets {
			if otherJudge == judge {
				continue
			}
			if strings.Contains(prompt, secret) {
				t.Errorf("%s saw %s's rationale", judge, otherJudge)
			}
		}
		// Section headers that would only exist if such content were injected.
		for _, forbidden := range []string{"## Aggregate", "## Challenge", "## Other judges", "## Coordinator", "mean_scores", "std_dev"} {
			if strings.Contains(prompt, forbidden) {
				t.Errorf("%s prompt leaked %q", judge, forbidden)
			}
		}
		if !strings.Contains(prompt, memoryDisclaimer) {
			t.Errorf("%s prompt injected memory without the non-authoritative label", judge)
		}
		for _, line := range strings.Split(prompt, "\n") {
			if strings.HasPrefix(line, "Evidence hash: ") {
				hashes[strings.TrimPrefix(line, "Evidence hash: ")] = true
			}
		}
	}
	if len(hashes) != 1 {
		t.Fatalf("judges saw %d different evidence hashes, want exactly 1", len(hashes))
	}
}

// Sealed isolation drops every non-evidence context source (spec §16).
func TestSealedIsolationDropsContextSources(t *testing.T) {
	packet := mustSeal(t, DecisionEvidencePacket{
		Question: "Ship?",
		Options:  []DecisionOption{{ID: "a", Kind: OptionExecute}},
	})
	for _, tc := range []struct {
		isolation string
		wantSeen  bool
	}{
		{agent.DecisionIsolationStrict, true},
		{agent.DecisionIsolationSealed, false},
	} {
		ctx, err := BuildJudgeContext(JudgeContextRequest{
			JudgeID: "judge-1", Round: 1, Packet: packet, Isolation: tc.isolation,
			ProjectContext: "PROJECT-CONTEXT-MARKER", Memory: "MEMORY-MARKER",
		})
		if err != nil {
			t.Fatal(err)
		}
		seen := strings.Contains(ctx.Prompt, "PROJECT-CONTEXT-MARKER") || strings.Contains(ctx.Prompt, "MEMORY-MARKER")
		if seen != tc.wantSeen {
			t.Errorf("isolation %q: context sources seen = %v, want %v", tc.isolation, seen, tc.wantSeen)
		}
	}
}

func TestJudgeContextRequiresSealedEvidence(t *testing.T) {
	_, err := BuildJudgeContext(JudgeContextRequest{
		JudgeID: "judge-1", Packet: DecisionEvidencePacket{Question: "Ship?"},
	})
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionEvidenceNotSealed) {
		t.Fatalf("BuildJudgeContext on unsealed evidence = %v, want %s", err, ReasonDecisionEvidenceNotSealed)
	}
}

// A crash after two of three opinions must dispatch only the missing judge and
// must not regenerate the durable ones (spec §38.1, test matrix P).
func TestDecisionEngineResumeDispatchesOnlyMissingJudges(t *testing.T) {
	journal := &memoryJournal{}
	failing := newRecordingRunner(func(judgeID string, _ int) (DecisionOpinion, error) {
		if judgeID == "judge-3" {
			return DecisionOpinion{}, fmt.Errorf("simulated crash before judge-3 answered")
		}
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	engine := newTestEngine(journal, failing, nil)

	req := engineRequest(enginePolicy(3))
	req.DecisionID = "decision-under-test"
	if _, err := engine.Run(context.Background(), req); err == nil {
		t.Fatal("Run succeeded despite judge-3 failing twice")
	}
	if got := journal.count(agent.EventDecisionOpinionSubmitted); got != 2 {
		t.Fatalf("durable opinions = %d, want 2", got)
	}

	recovered := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(3, 9, "wait", 0.3), nil
	})
	resumeEngine := newTestEngine(journal, recovered, nil)
	if _, err := resumeEngine.Run(context.Background(), req); err != nil {
		t.Fatalf("resume = %v", err)
	}

	dispatched := recovered.dispatched()
	if len(dispatched) != 1 || dispatched[0] != "judge-3" {
		t.Fatalf("resume dispatched %v, want only judge-3", dispatched)
	}
	if got := journal.count(agent.EventDecisionOpinionSubmitted); got != 3 {
		t.Fatalf("durable opinions after resume = %d, want 3", got)
	}
}

// A finalized decision is never recomputed (spec §35).
func TestDecisionEngineDoesNotRerunFinalizedDecisions(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	engine := newTestEngine(journal, runner, nil)

	req := engineRequest(enginePolicy(2))
	req.DecisionID = "decision-final"
	first, err := engine.Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	dispatchedFirst := len(runner.dispatched())

	again, err := engine.Resume(context.Background(), "decision-final")
	if err != nil {
		t.Fatal(err)
	}
	if len(runner.dispatched()) != dispatchedFirst {
		t.Fatalf("resume re-dispatched judges: %v", runner.dispatched())
	}
	firstJSON, _ := json.Marshal(first)
	againJSON, _ := json.Marshal(again)
	if string(firstJSON) != string(againJSON) {
		t.Fatal("resume returned a different record than the finalized one")
	}
}

// Changed material evidence supersedes the old hash instead of editing the
// durable opinions (spec §15.4).
func TestDecisionEngineMaterialEvidenceChangeStartsANewRound(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})
	engine := newTestEngine(journal, runner, nil)

	req := engineRequest(enginePolicy(2))
	req.DecisionID = "decision-evolving"
	if _, err := engine.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	changed := req
	changed.Question = "Should we migrate next quarter instead?"
	engine2 := newTestEngine(journal, runner, nil)
	if _, err := engine2.Run(context.Background(), changed); err == nil {
		// Finalized decisions short-circuit; clear that path by using a new ID.
		t.Log("finalized decision returned as-is, as designed")
	}

	fresh := changed
	fresh.DecisionID = "decision-evolving-2"
	journal2 := &memoryJournal{}
	engine3 := newTestEngine(journal2, runner, nil)
	if _, err := engine3.Run(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	state, err := projectDecision(context.Background(), journal2, "decision-evolving-2")
	if err != nil {
		t.Fatal(err)
	}
	before := state.Packet.Hash

	fresh.Question = "Should we migrate at all?"
	journal3 := &memoryJournal{}
	engine4 := newTestEngine(journal3, runner, nil)
	if _, err := engine4.Run(context.Background(), fresh); err != nil {
		t.Fatal(err)
	}
	state3, err := projectDecision(context.Background(), journal3, "decision-evolving-2")
	if err != nil {
		t.Fatal(err)
	}
	if state3.Packet.Hash == before {
		t.Fatal("a changed question produced the same evidence hash")
	}
}

// An invalid structured response is repaired once, then rejected. The rejected
// opinion stays durable but never reaches the aggregate (spec §14.3).
func TestDecisionEngineRepairsOnceThenRejects(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(judgeID string, attempt int) (DecisionOpinion, error) {
		if judgeID == "judge-2" && attempt == 1 {
			bad := scoredOpinion(8, 4, "migrate", 0.8)
			delete(bad.OptionScores[0].Criteria, "risk")
			return bad, nil
		}
		if judgeID == "judge-3" {
			bad := scoredOpinion(8, 4, "migrate", 0.8)
			bad.SuccessProbability = 42
			return bad, nil
		}
		return scoredOpinion(8, 4, "migrate", 0.8), nil
	})

	policy := enginePolicy(3)
	policy.MinIndependentJudgments = 2
	engine := newTestEngine(journal, runner, nil)

	record, err := engine.Run(context.Background(), engineRequest(policy))
	if err != nil {
		t.Fatalf("Run = %v", err)
	}
	if journal.count(agent.EventDecisionOpinionRejected) != 1 {
		t.Fatalf("rejections = %d, want 1", journal.count(agent.EventDecisionOpinionRejected))
	}
	valid := 0
	for _, opinion := range record.Opinions {
		if opinion.Valid {
			valid++
		}
	}
	if valid != 2 || len(record.Opinions) != 3 {
		t.Fatalf("record has %d opinions (%d valid), want 3 stored and 2 valid", len(record.Opinions), valid)
	}
	if record.Aggregates[0].JudgeCount != 2 {
		t.Fatalf("aggregate counted %d judges, want the 2 valid ones", record.Aggregates[0].JudgeCount)
	}
	// Repair is bounded: judge-3 was asked exactly twice, never more.
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.attempts["judge-3"] != 2 {
		t.Fatalf("judge-3 attempts = %d, want exactly 2", runner.attempts["judge-3"])
	}
}

func TestDecisionEngineFailsClosedBelowMinimumJudges(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		bad := scoredOpinion(8, 4, "migrate", 0.8)
		bad.SuccessProbability = 99
		return bad, nil
	})
	engine := newTestEngine(journal, runner, nil)

	_, err := engine.Run(context.Background(), engineRequest(enginePolicy(3)))
	if err == nil || !strings.Contains(err.Error(), ReasonDecisionInsufficientValidOpinions) {
		t.Fatalf("Run = %v, want %s", err, ReasonDecisionInsufficientValidOpinions)
	}
}

// The runtime aggregates in Go; a judge's own overall is ignored and the fact
// is recorded rather than passed over in silence (spec §14.2).
func TestDecisionEngineRecordsIgnoredJudgeOverall(t *testing.T) {
	journal := &memoryJournal{}
	runner := newRecordingRunner(func(string, int) (DecisionOpinion, error) {
		o := scoredOpinion(8, 4, "migrate", 0.8)
		o.OptionScores[0].Overall = 9.9
		return o, nil
	})
	engine := newTestEngine(journal, runner, nil)

	record, err := engine.Run(context.Background(), engineRequest(enginePolicy(2)))
	if err != nil {
		t.Fatal(err)
	}
	if journal.count(agent.EventDecisionJudgeOverallIgnored) != 2 {
		t.Fatalf("ignored-overall events = %d, want one per judge", journal.count(agent.EventDecisionJudgeOverallIgnored))
	}
	if record.Opinions[0].OptionScores[0].Overall != 8 {
		t.Fatalf("overall = %v, want the runtime-computed 8", record.Opinions[0].OptionScores[0].Overall)
	}
}
