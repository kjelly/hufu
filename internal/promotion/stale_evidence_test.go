package promotion

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// TestEligibleSourcesRequireRecentStrongEvidence excludes a supported source
// that was not verified within the stale-after window and says why.
func TestEligibleSourcesRequireRecentStrongEvidence(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name         string
		verifiedAt   time.Time
		staleAfter   time.Duration
		wantEligible bool
	}{
		{name: "recently verified", verifiedAt: now.Add(-time.Hour), staleAfter: 30 * 24 * time.Hour, wantEligible: true},
		{name: "verified long ago", verifiedAt: now.Add(-40 * 24 * time.Hour), staleAfter: 30 * 24 * time.Hour},
		{name: "no recorded verification", staleAfter: 30 * 24 * time.Hour},
		{name: "zero stale-after skips the check", staleAfter: 0, wantEligible: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer repo.Close()
			item := contextstore.ContextItem{ID: "pattern", Kind: contextstore.ContextPattern, Content: "Run the verifier after generating code", Scope: contextstore.Scope{ProjectID: "p", TeamID: "demo"}, Lifecycle: contextstore.LifecycleConfirmed, Metadata: map[string]string{"memory_lifetime": "persistent"}}
			if err := repo.Append(ctx, item); err != nil {
				t.Fatal(err)
			}
			observedAt := tc.verifiedAt
			if observedAt.IsZero() {
				observedAt = now.Add(-time.Hour)
			}
			for _, task := range []string{"run-1/1", "run-2/1"} {
				if _, err := repo.ApplyExperienceObservation(ctx, contextstore.ExperienceObservation{
					IdempotencyKey: "verified-" + task, ContextItemID: item.ID, PolicyVersion: "memory-policy-v1", ProjectID: "p", TaskID: task,
					AppliedDelta: 1, VerifiedSupportDelta: 1, PositiveWeight: 1, StrongEvidence: !tc.verifiedAt.IsZero(), ObservedAt: observedAt,
				}); err != nil {
					t.Fatal(err)
				}
			}
			policy := agent.DefaultMemoryLearningPolicy()
			policy.StaleAfter = tc.staleAfter
			eligible, diagnostics, err := EligibleSources(ctx, repo, EligibilityOptions{ProjectID: "p", TeamID: "demo", PolicyVersion: "memory-policy-v1"}, policy)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantEligible {
				if len(eligible) != 1 || len(diagnostics) != 0 {
					t.Fatalf("eligible=%v diagnostics=%v, want the source eligible", eligible, diagnostics)
				}
				return
			}
			if len(eligible) != 0 || len(diagnostics) != 1 || diagnostics[0] != (Diagnostic{SourceID: item.ID, Reason: DiagnosticStrongEvidenceStale}) {
				t.Fatalf("eligible=%v diagnostics=%v, want the source excluded as %s", eligible, diagnostics, DiagnosticStrongEvidenceStale)
			}
		})
	}
}
