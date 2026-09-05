package team

import (
	"strings"
	"testing"
)

func TestValidOutcome(t *testing.T) {
	for _, value := range []string{OutcomeSucceeded, OutcomeFailed, OutcomeMixed, OutcomeSuperseded, OutcomeUnresolved} {
		if !ValidOutcome(value) {
			t.Errorf("ValidOutcome(%q) = false", value)
		}
	}
	for _, value := range []string{"", "good", "ok", "SUCCEEDED"} {
		if ValidOutcome(value) {
			t.Errorf("ValidOutcome(%q) = true", value)
		}
	}
}

func TestValidateOutcome(t *testing.T) {
	tests := []struct {
		name    string
		outcome DecisionOutcomeRecord
		wantErr string
	}{
		{
			name:    "baseline",
			outcome: DecisionOutcomeRecord{DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded},
		},
		{
			name:    "missing decision id",
			outcome: DecisionOutcomeRecord{ResolvedOutcome: OutcomeSucceeded},
			wantErr: "requires a decision id",
		},
		{
			name:    "undeclared classification",
			outcome: DecisionOutcomeRecord{DecisionID: "dec-1", ResolvedOutcome: "went well"},
			wantErr: "is not one of",
		},
		{
			name:    "forecast out of range",
			outcome: DecisionOutcomeRecord{DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded, Forecast: 1.5},
			wantErr: "outside [0, 1]",
		},
		{
			name: "future schema version",
			outcome: DecisionOutcomeRecord{
				DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded,
				SchemaVersion: OutcomeSchemaVersion + 1,
			},
			wantErr: "schema version",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome := tt.outcome
			err := ValidateOutcome(&outcome)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateOutcome = %v, want nil", err)
				}
				if outcome.SchemaVersion != OutcomeSchemaVersion {
					t.Fatalf("SchemaVersion = %d, want it stamped", outcome.SchemaVersion)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ValidateOutcome = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// This is the "verified outcome" definition Phase 5's entry conditions require
// (spec §49.2). It is deliberately narrow: evidence must resolve against real
// bytes, and an assertion is never enough.
func TestVerifyOutcomeEvidence(t *testing.T) {
	store := NewArtifactResolver([]ArtifactRef{
		{ID: "a", SHA256: "h1", Path: "reports/lag.json"},
		{ID: "b", SHA256: "h2", Path: "reports/cost.json"},
	})

	t.Run("all cited evidence resolves", func(t *testing.T) {
		outcome := DecisionOutcomeRecord{
			DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded,
			ObservedEvidence: []ArtifactRef{{SHA256: "h1"}, {SHA256: "h2"}},
		}
		if err := VerifyOutcomeEvidence(&outcome, store); err != nil {
			t.Fatal(err)
		}
		if !outcome.Verified || len(outcome.UnverifiedEvidence) != 0 {
			t.Fatalf("outcome = %#v, want verified", outcome)
		}
		// The resolved refs are filled in from the store, not from the caller.
		if outcome.ObservedEvidence[0].Path == "" {
			t.Fatal("resolved evidence was not enriched from the store")
		}
	})

	t.Run("a single unresolvable digest blocks verification", func(t *testing.T) {
		outcome := DecisionOutcomeRecord{
			DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded,
			ObservedEvidence: []ArtifactRef{{SHA256: "h1"}, {SHA256: "nope"}},
		}
		if err := VerifyOutcomeEvidence(&outcome, store); err != nil {
			t.Fatal(err)
		}
		if outcome.Verified {
			t.Fatal("an outcome citing a digest that does not exist was verified")
		}
		if len(outcome.UnverifiedEvidence) != 1 || outcome.UnverifiedEvidence[0] != "nope" {
			t.Fatalf("UnverifiedEvidence = %v, want the missing digest named", outcome.UnverifiedEvidence)
		}
	})

	t.Run("an assertion alone is not verification", func(t *testing.T) {
		outcome := DecisionOutcomeRecord{
			DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded,
			ResolvedBy: "a very confident operator",
		}
		if err := VerifyOutcomeEvidence(&outcome, store); err != nil {
			t.Fatal(err)
		}
		if outcome.Verified {
			t.Fatal("an outcome with no evidence was verified")
		}
		if !strings.Contains(outcome.VerificationSummary, "no observed evidence") {
			t.Fatalf("summary = %q", outcome.VerificationSummary)
		}
	})

	t.Run("evidence with no digest cannot verify", func(t *testing.T) {
		outcome := DecisionOutcomeRecord{
			DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded,
			ObservedEvidence: []ArtifactRef{{Path: "reports/lag.json"}},
		}
		if err := VerifyOutcomeEvidence(&outcome, store); err != nil {
			t.Fatal(err)
		}
		if outcome.Verified {
			t.Fatal("an outcome citing a path with no digest was verified")
		}
	})

	t.Run("no store available", func(t *testing.T) {
		outcome := DecisionOutcomeRecord{
			DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded,
			ObservedEvidence: []ArtifactRef{{SHA256: "h1"}},
		}
		if err := VerifyOutcomeEvidence(&outcome, nil); err != nil {
			t.Fatal(err)
		}
		if outcome.Verified {
			t.Fatal("an outcome was verified with no store to check against")
		}
	})
}

// Verification must not depend on the order digests were cited in.
func TestVerifyOutcomeEvidenceIsOrderIndependent(t *testing.T) {
	store := NewArtifactResolver([]ArtifactRef{{SHA256: "h1"}, {SHA256: "h2"}})
	forward := DecisionOutcomeRecord{
		DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded,
		ObservedEvidence: []ArtifactRef{{SHA256: "h1"}, {SHA256: "h2"}, {SHA256: "zz"}},
	}
	reverse := DecisionOutcomeRecord{
		DecisionID: "dec-1", ResolvedOutcome: OutcomeSucceeded,
		ObservedEvidence: []ArtifactRef{{SHA256: "zz"}, {SHA256: "h2"}, {SHA256: "h1"}},
	}
	if err := VerifyOutcomeEvidence(&forward, store); err != nil {
		t.Fatal(err)
	}
	if err := VerifyOutcomeEvidence(&reverse, store); err != nil {
		t.Fatal(err)
	}
	if forward.Verified != reverse.Verified ||
		forward.VerificationSummary != reverse.VerificationSummary ||
		strings.Join(forward.UnverifiedEvidence, ",") != strings.Join(reverse.UnverifiedEvidence, ",") {
		t.Fatalf("verification depended on citation order:\n%#v\n%#v", forward, reverse)
	}
}

func TestWorkspaceArtifactsMissingStore(t *testing.T) {
	artifacts, err := WorkspaceArtifacts(t.TempDir())
	if err != nil {
		t.Fatalf("WorkspaceArtifacts = %v, want an empty store to read as empty", err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("WorkspaceArtifacts = %v, want empty", artifacts)
	}
}
