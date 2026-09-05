package team

import (
	"strings"
	"testing"
	"time"
)

func evidenceFixture() DecisionEvidencePacket {
	return DecisionEvidencePacket{
		ID:       "dec-1-evidence",
		Question: "Should we migrate the ingest pipeline now?",
		Options: []DecisionOption{
			{ID: "migrate", Kind: OptionExecute, Title: "Migrate now"},
			{ID: "defer", Kind: OptionDefer, Title: "Wait one quarter"},
		},
		Criteria: []DecisionCriterion{
			{ID: "cost", Weight: 2, Statement: "total cost"},
			{ID: "risk", Weight: 1, Statement: "operational risk"},
		},
		Facts:     map[string]any{"daily_events": 1200000, "team_size": 4},
		Artifacts: []ArtifactRef{{Path: "reports/load.json", SHA256: "aaa", MediaType: "application/json", Role: "evidence"}},
		BaseRates: []BaseRateEvidence{{
			ReferenceClass: "pipeline migrations", Metric: "on-time completion", SampleSize: 30,
			Distribution: DistributionSummary{Mean: 0.55, Median: 0.5, P10: 0.2, P90: 0.9},
			Source:       ArtifactRef{SHA256: "bbb"},
		}},
		Assumptions: []DecisionAssumption{{ID: "A1", Statement: "traffic stays flat", Critical: true}},
		CreatedAt:   time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC),
	}
}

func mustSeal(t *testing.T, packet DecisionEvidencePacket) DecisionEvidencePacket {
	t.Helper()
	sealed, err := packet.Seal()
	if err != nil {
		t.Fatalf("Seal = %v", err)
	}
	return sealed
}

// The digest must be stable across repeated seals and across input ordering:
// judges are told they share "the same evidence", and that has to be exact.
func TestEvidenceHashIsStable(t *testing.T) {
	packet := evidenceFixture()
	first := mustSeal(t, packet).Hash
	if first == "" || len(first) != 64 {
		t.Fatalf("hash = %q, want a 64-character hex digest", first)
	}
	for i := 0; i < 20; i++ {
		if again := mustSeal(t, packet).Hash; again != first {
			t.Fatalf("seal %d produced %q, want %q", i, again, first)
		}
	}

	reordered := evidenceFixture()
	reordered.Options = []DecisionOption{reordered.Options[1], reordered.Options[0]}
	reordered.Criteria = []DecisionCriterion{reordered.Criteria[1], reordered.Criteria[0]}
	if got := mustSeal(t, reordered).Hash; got != first {
		t.Fatalf("reordering inputs changed the hash: %q vs %q", got, first)
	}
}

// Material changes must produce a new hash (spec §15.2).
func TestMaterialChangesAlterTheHash(t *testing.T) {
	base := mustSeal(t, evidenceFixture()).Hash
	tests := []struct {
		name   string
		mutate func(*DecisionEvidencePacket)
	}{
		{"question", func(p *DecisionEvidencePacket) { p.Question = "Should we migrate later?" }},
		{"option added", func(p *DecisionEvidencePacket) {
			p.Options = append(p.Options, DecisionOption{ID: "reduce", Kind: OptionReduceScope})
		}},
		{"option kind", func(p *DecisionEvidencePacket) { p.Options[0].Kind = OptionNegotiate }},
		{"criterion weight", func(p *DecisionEvidencePacket) { p.Criteria[0].Weight = 5 }},
		{"fact value", func(p *DecisionEvidencePacket) { p.Facts["team_size"] = 5 }},
		{"artifact content", func(p *DecisionEvidencePacket) { p.Artifacts[0].SHA256 = "ccc" }},
		{"base rate sample", func(p *DecisionEvidencePacket) { p.BaseRates[0].SampleSize = 31 }},
		{"assumption statement", func(p *DecisionEvidencePacket) { p.Assumptions[0].Statement = "traffic doubles" }},
		{"assumption criticality", func(p *DecisionEvidencePacket) { p.Assumptions[0].Critical = false }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packet := evidenceFixture()
			tt.mutate(&packet)
			if got := mustSeal(t, packet).Hash; got == base {
				t.Fatalf("changing %s did not change the hash", tt.name)
			}
		})
	}
}

// Non-material changes must NOT alter the hash. Hashing them would make every
// routine bookkeeping update invalidate a whole judgment round (spec §15.2).
func TestNonMaterialChangesPreserveTheHash(t *testing.T) {
	base := mustSeal(t, evidenceFixture()).Hash
	tests := []struct {
		name   string
		mutate func(*DecisionEvidencePacket)
	}{
		{"created at", func(p *DecisionEvidencePacket) { p.CreatedAt = time.Now() }},
		{"packet id", func(p *DecisionEvidencePacket) { p.ID = "another-id" }},
		{"contract ref", func(p *DecisionEvidencePacket) { p.RequestContractRef = "contract-9" }},
		{"artifact path", func(p *DecisionEvidencePacket) { p.Artifacts[0].Path = "elsewhere/load.json" }},
		{"artifact run identity", func(p *DecisionEvidencePacket) {
			p.Artifacts[0].RunID = "run-9"
			p.Artifacts[0].TaskID = "task-9"
			p.Artifacts[0].Attempt = 3
			p.Artifacts[0].Bytes = 4096
		}},
		{"assumption status", func(p *DecisionEvidencePacket) {
			p.Assumptions[0].Status = AssumptionContradicted
			p.Assumptions[0].CheckedAt = time.Now()
		}},
		{"base rate source and limitations", func(p *DecisionEvidencePacket) {
			p.BaseRates[0].Source = ArtifactRef{SHA256: "zzz"}
			p.BaseRates[0].Limitations = []string{"small sample"}
		}},
		{"surrounding whitespace", func(p *DecisionEvidencePacket) {
			p.Question = "  " + p.Question + "\n"
			p.Options[0].Title = " Migrate now "
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packet := evidenceFixture()
			tt.mutate(&packet)
			if got := mustSeal(t, packet).Hash; got != base {
				t.Fatalf("changing %s changed the hash: %q vs %q", tt.name, got, base)
			}
		})
	}
}

// The canonical byte form is the hash input; pin it so a future encoder change
// cannot silently invalidate every persisted decision.
func TestCanonicalMaterialGolden(t *testing.T) {
	packet := DecisionEvidencePacket{
		Question: "Ship?",
		Options: []DecisionOption{
			{ID: "b", Kind: OptionDefer},
			{ID: "a", Kind: OptionExecute, Title: "Ship it"},
		},
		Criteria: []DecisionCriterion{{ID: "risk", Weight: 3}, {ID: "cost", Weight: 1}},
		Facts:    map[string]any{"b": 2, "a": "x"},
	}
	canonical, err := packet.canonicalMaterial()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"criteria":[{"id":"cost","normalized_weight":0.250000},{"id":"risk","normalized_weight":0.750000}],` +
		`"facts":{"a":"x","b":2},` +
		`"options":[{"id":"a","kind":"execute","title":"Ship it"},{"id":"b","kind":"defer"}],` +
		`"question":"Ship?"}`
	if string(canonical) != want {
		t.Fatalf("canonical form:\n got %s\nwant %s", canonical, want)
	}
}

// Absent and empty must hash identically so an empty list can never look like
// different evidence than a missing one (spec §15.3 rule 5).
func TestCanonicalOmitsEmptyValues(t *testing.T) {
	absent := DecisionEvidencePacket{Question: "Ship?", Options: []DecisionOption{{ID: "a", Kind: OptionExecute}}}
	empty := DecisionEvidencePacket{
		Question:    "Ship?",
		Options:     []DecisionOption{{ID: "a", Kind: OptionExecute, Title: "", Description: ""}},
		Facts:       map[string]any{},
		Artifacts:   []ArtifactRef{},
		BaseRates:   []BaseRateEvidence{},
		Assumptions: []DecisionAssumption{},
	}
	if mustSeal(t, absent).Hash != mustSeal(t, empty).Hash {
		t.Fatal("absent and empty collections hashed differently")
	}
}

// Unicode normalization keeps two byte-different but canonically identical
// strings from looking like different evidence.
func TestCanonicalNormalizesUnicode(t *testing.T) {
	composed := DecisionEvidencePacket{Question: "Ship café?", Options: []DecisionOption{{ID: "a", Kind: OptionExecute}}}
	decomposed := DecisionEvidencePacket{Question: "Ship café?", Options: []DecisionOption{{ID: "a", Kind: OptionExecute}}}
	if mustSeal(t, composed).Hash != mustSeal(t, decomposed).Hash {
		t.Fatal("NFC-equivalent questions hashed differently")
	}
}

func TestEvidenceValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*DecisionEvidencePacket)
		wantErr string
	}{
		{name: "baseline", mutate: func(*DecisionEvidencePacket) {}},
		{name: "no question", mutate: func(p *DecisionEvidencePacket) { p.Question = "  " }, wantErr: "requires a question"},
		{name: "empty option id", mutate: func(p *DecisionEvidencePacket) { p.Options[0].ID = "" }, wantErr: "must not be empty"},
		{name: "duplicate option id", mutate: func(p *DecisionEvidencePacket) { p.Options[1].ID = p.Options[0].ID }, wantErr: "duplicated"},
		{name: "unknown option kind", mutate: func(p *DecisionEvidencePacket) { p.Options[0].Kind = "wishful" }, wantErr: "not a declared option kind"},
		{name: "duplicate criterion", mutate: func(p *DecisionEvidencePacket) { p.Criteria[1].ID = p.Criteria[0].ID }, wantErr: "duplicated"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			packet := evidenceFixture()
			tt.mutate(&packet)
			err := packet.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestPacketOptionHelpers(t *testing.T) {
	packet := evidenceFixture()
	if !packet.HasOption("migrate") || packet.HasOption("nope") {
		t.Fatal("HasOption is wrong")
	}
	ids := packet.OptionIDs()
	if len(ids) != 2 || ids[0] != "defer" || ids[1] != "migrate" {
		t.Fatalf("OptionIDs = %v, want ascending order", ids)
	}
	if got := packet.NoGoOption(); got != "defer" {
		t.Fatalf("NoGoOption = %q, want %q", got, "defer")
	}
	packet.Options = []DecisionOption{{ID: "a", Kind: OptionExecute}}
	if got := packet.NoGoOption(); got != "" {
		t.Fatalf("NoGoOption = %q, want empty when no alternative exists", got)
	}
}
