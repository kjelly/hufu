package team

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
)

// A coordinator's task payload must never be able to select a decision
// profile: that would let an LLM lower configured rigor. The field is
// json:"-" for exactly this reason (spec §9, acceptance test 7).
func TestTaskPayloadCannotSetDecisionProfile(t *testing.T) {
	payloads := []string{
		`{"agent":"worker","goal":"do it","decision_profile":"off"}`,
		`{"agent":"worker","goal":"do it","decision-profile":"off"}`,
		`{"agent":"worker","goal":"do it","DecisionProfile":"off"}`,
	}
	for _, payload := range payloads {
		var task TaskDef
		if err := json.Unmarshal([]byte(payload), &task); err != nil {
			t.Fatalf("Unmarshal(%s) = %v", payload, err)
		}
		if task.DecisionProfile != "" {
			t.Fatalf("payload %s set DecisionProfile = %q, want it to stay empty", payload, task.DecisionProfile)
		}
	}
}

// The field must still be settable from static team configuration.
func TestTaskYAMLCanSetDecisionProfile(t *testing.T) {
	var task TaskDef
	if err := yaml.Unmarshal([]byte("agent: worker\ngoal: do it\ndecision-profile: high-stakes\n"), &task); err != nil {
		t.Fatalf("yaml decode: %v", err)
	}
	if task.DecisionProfile != "high-stakes" {
		t.Fatalf("DecisionProfile = %q, want %q", task.DecisionProfile, "high-stakes")
	}
}

// A marshalled TaskDef must not leak the field into the coordinator-visible
// JSON surface either.
func TestTaskMarshalOmitsDecisionProfile(t *testing.T) {
	data, err := json.Marshal(TaskDef{Agent: "worker", Goal: "do it", DecisionProfile: "high-stakes"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "decision") {
		t.Fatalf("marshalled TaskDef = %s, want no decision profile field", data)
	}
}

func fullDecisionRecord() DecisionRecord {
	ts := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	return DecisionRecord{
		SchemaVersion:      DecisionRecordSchemaVersion,
		ID:                 "dec-1",
		RunID:              "run-1",
		TaskID:             "task-1",
		Profile:            "standard",
		EvidenceHash:       "abc123",
		RequestContractRef: "contract-1",
		Options: []DecisionOption{
			{ID: "a", Kind: OptionExecute, Title: "Ship"},
			{ID: "b", Kind: OptionDefer, Title: "Wait"},
		},
		Assumptions: []DecisionAssumption{
			{ID: "A1", Statement: "traffic stays flat", Status: AssumptionSupported, Critical: true, CheckedAt: ts},
		},
		Opinions: []DecisionOpinion{{
			ID: "op-1", JudgeID: "judge-1", EvidenceHash: "abc123", Round: 1, Valid: true,
			OptionScores: []OptionScore{
				{OptionID: "a", Criteria: map[string]float64{"cost": 7, "risk": 4}, Overall: 5.5},
				{OptionID: "b", Criteria: map[string]float64{"cost": 3, "risk": 8}, Overall: 5.5},
			},
			PreferredOption: "a", SuccessProbability: 0.7, Confidence: 0.6,
		}},
		Aggregates: []DecisionAggregate{{
			ID: "agg-1", EvidenceHash: "abc123", Round: 1, Method: agent.AggregationMeanScore, JudgeCount: 1,
			MeanScores:      map[string]float64{"a": 5.5, "b": 5.5},
			StdDev:          map[string]float64{"a": 0, "b": 0},
			MAD:             map[string]float64{"a": 0, "b": 0},
			MeanProbability: map[string]float64{"a": 0.7},
			PreferredOption: "a",
		}},
		Challenges: []DecisionChallenge{{ID: "ch-1", EvidenceHash: "abc123", TargetOption: "a", Severity: 0.4}},
		Revisions:  []DecisionRevision{{ID: "rev-1", OriginalOpinionID: "op-1", JudgeID: "judge-1", Changed: false}},
		Premortem: &PremortemResult{ID: "pm-1", AssumedOutcome: "failure", FailureModes: []FailureMode{
			{ID: "F1", Description: "rollout stalls", Likelihood: 0.2, Impact: 0.8},
		}},
		FinalOption:         "a",
		FinalizationMode:    agent.FinalizationAggregate,
		Probability:         0.7,
		AlternativesChecked: true,
		NoGoOptionID:        "b",
		StopPolicySnapshot: StopPolicy{
			MaxAttempts: 3, CheckpointEvery: 1, RequireKillCriteria: true,
			KillCriteria: []KillCriterion{{ID: "tokens", Kind: agent.KillKindBudgetTokens, Threshold: 100000}},
		},
		SourceCount:            3,
		IndependenceGroupCount: 1,
		SharedOriginWarnings:   []string{"three mirrors of one origin"},
		ReplanTriggers:         []string{"material_evidence_changed"},
		Degradations:           []DecisionDegradation{{Step: "challenge.count", From: "2", To: "1", Reason: "budget", Timestamp: ts}},
		KeyAssumptions:         []string{"A1"},
		CreatedAt:              ts,
	}
}

// Persisted decision state must serialize deterministically: replay and hash
// chains depend on byte-stable encodings (spec §35, Phase 0 acceptance 4).
func TestDecisionRecordSerializationIsDeterministic(t *testing.T) {
	record := fullDecisionRecord()

	first, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(first, again) {
			t.Fatalf("marshal iteration %d differs:\n%s\n%s", i, first, again)
		}
	}

	var decoded DecisionRecord
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	roundTripped, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, roundTripped) {
		t.Fatalf("round trip changed encoding:\nwant %s\ngot  %s", first, roundTripped)
	}
}

// A zero-valued record must encode to a compact object: absent decision state
// must not bloat every persisted session.
func TestZeroDecisionRecordEncodesCompactly(t *testing.T) {
	data, err := json.Marshal(DecisionRecord{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(data); got != `{"schema_version":0,"id":""}` {
		t.Fatalf("zero DecisionRecord = %s", got)
	}
}

func TestDecisionRecordSchemaVersion(t *testing.T) {
	var r DecisionRecord
	if err := r.ValidateSchemaVersion(); err == nil {
		t.Fatal("zero version accepted")
	}
	r.Normalize()
	if err := r.ValidateSchemaVersion(); err != nil {
		t.Fatalf("normalized schema rejected: %v", err)
	}
	if r.SchemaVersion != DecisionRecordSchemaVersion {
		t.Fatalf("Normalize() left SchemaVersion = %d", r.SchemaVersion)
	}
	future := DecisionRecord{ID: "x", SchemaVersion: DecisionRecordSchemaVersion + 1}
	if err := future.ValidateSchemaVersion(); err == nil {
		t.Fatal("future schema version accepted, want rejection")
	}
}

func TestDecisionOptionKind(t *testing.T) {
	tests := []struct {
		kind  DecisionOptionKind
		valid bool
		noGo  bool
	}{
		{OptionExecute, true, false},
		{OptionDefer, true, true},
		{OptionAbandon, true, true},
		{OptionRequestInfo, true, false},
		{OptionReduceScope, true, false},
		{OptionNegotiate, true, false},
		{OptionCustom, true, false},
		{DecisionOptionKind("wishful"), false, false},
	}
	for _, tt := range tests {
		if got := tt.kind.Valid(); got != tt.valid {
			t.Errorf("%q.Valid() = %v, want %v", tt.kind, got, tt.valid)
		}
		if got := tt.kind.IsNoGo(); got != tt.noGo {
			t.Errorf("%q.IsNoGo() = %v, want %v", tt.kind, got, tt.noGo)
		}
	}
}

func TestAssumptionStatusHelpers(t *testing.T) {
	if got := (DecisionAssumption{}).EffectiveStatus(); got != AssumptionUnknown {
		t.Fatalf("EffectiveStatus() = %q, want %q", got, AssumptionUnknown)
	}
	for _, status := range []string{AssumptionUnknown, AssumptionSupported, AssumptionContradicted, AssumptionStale} {
		if !ValidAssumptionStatus(status) {
			t.Fatalf("ValidAssumptionStatus(%q) = false", status)
		}
	}
	if ValidAssumptionStatus("probably") {
		t.Fatal("ValidAssumptionStatus(probably) = true, want false")
	}
	if got := AssumptionStatusEvent(AssumptionContradicted); got != EventAssumptionContradicted {
		t.Fatalf("AssumptionStatusEvent = %q, want %q", got, EventAssumptionContradicted)
	}
	if got := AssumptionStatusEvent("anything-else"); got != EventAssumptionDeclared {
		t.Fatalf("AssumptionStatusEvent fallback = %q, want %q", got, EventAssumptionDeclared)
	}
}

// A team.yaml written before this feature existed must load with decision
// support inert (spec §8 compatibility requirement).
func TestParseTeamYMLWithoutDecisionBlock(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: legacy\nmax-rounds: 4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	if cfg.Decision.DefaultProfile != "" || len(cfg.Decision.Profiles) != 0 {
		t.Fatalf("Decision = %#v, want zero value", cfg.Decision)
	}
	resolution, err := ResolveDecisionProfile(cfg.Decision, "", TaskDef{})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Enabled() || resolution.Profile != DecisionProfileOff {
		t.Fatalf("resolution = %#v, want the reserved off profile", resolution)
	}
}

func TestParseTeamYMLDecisionBlock(t *testing.T) {
	dir := t.TempDir()
	content := `name: gated
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      context-isolation: strict
      score-scale: 0-10
      aggregation:
        method: mean-score
      max-rounds: 2
      discipline:
        stop:
          checkpoint-every: 1
          require-kill-criteria: true
          kill-criteria:
            - id: tokens
              kind: budget_tokens
              threshold: 100000
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	if cfg.Decision.DefaultProfile != "standard" {
		t.Fatalf("DefaultProfile = %q", cfg.Decision.DefaultProfile)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok {
		t.Fatal("standard profile not resolvable")
	}
	if policy.IndependentJudgments != 3 || len(policy.Discipline.Stop.KillCriteria) != 1 {
		t.Fatalf("policy = %#v", policy)
	}
}

func TestParseTeamYMLRejectsInvalidDecisionBlock(t *testing.T) {
	dir := t.TempDir()
	content := `decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      discipline:
        stop:
          kill-criteria:
            - id: ev
              kind: expected_value
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := parseTeamYML(dir, nil)
	if err == nil || !strings.Contains(err.Error(), agent.ReasonStopPolicyUnknownKillKind) {
		t.Fatalf("parseTeamYML error = %v, want %s", err, agent.ReasonStopPolicyUnknownKillKind)
	}
}

func TestParseTeamYMLRejectsUnknownDecisionField(t *testing.T) {
	dir := t.TempDir()
	content := `decision:
  default-profile: standard
  profiles:
    standard:
      independent_judgments: 3
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil || !strings.Contains(err.Error(), "independent_judgments") {
		t.Fatalf("parseTeamYML error = %v, want unknown-field rejection", err)
	}
}

// discipline.routing.capability-aware (plan.md Stage 8) must round-trip
// through the strict team.yaml decoder like every other discipline field.
func TestParseTeamYMLDecisionRoutingCapabilityAware(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
decision:
  default-profile: standard
  profiles:
    standard:
      independent-judgments: 3
      discipline:
        routing:
          capability-aware: true
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	policy, ok := DecisionPolicyFor(cfg.Decision, "standard")
	if !ok || !policy.Discipline.Routing.CapabilityAware {
		t.Fatalf("policy.Discipline.Routing = %#v, want capability-aware: true", policy.Discipline.Routing)
	}
}

// capability-registry (plan.md Stage 8) is a maintainer-authored, team-level
// trust source, independent of whether decision profiles are configured.
func TestParseTeamYMLCapabilityRegistry(t *testing.T) {
	dir := t.TempDir()
	content := `name: routed
capability-registry:
  reviewer:
    - capability: security-review
      confidence: 0.6
      cost-class: low
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := parseTeamYML(dir, nil)
	if err != nil {
		t.Fatalf("parseTeamYML = %v", err)
	}
	decls := cfg.CapabilityRegistry["reviewer"]
	if len(decls) != 1 || decls[0].Capability != "security-review" || decls[0].Confidence != 0.6 {
		t.Fatalf("CapabilityRegistry[reviewer] = %#v", decls)
	}
}

// A malformed declaration must fail team load, not fail silently the first
// time a routing decision happens to consult it.
func TestParseTeamYMLRejectsInvalidCapabilityRegistry(t *testing.T) {
	dir := t.TempDir()
	content := `capability-registry:
  reviewer:
    - capability: security-review
      confidence: 2.0
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := parseTeamYML(dir, nil); err == nil || !strings.Contains(err.Error(), "capability-registry.reviewer") {
		t.Fatalf("parseTeamYML error = %v, want capability-registry validation failure", err)
	}
}
