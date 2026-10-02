package context

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRetireConfirmedWithdrawsCurrentKnowledge(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, repo *SQLiteRepository) []string
		reason  string
		wantErr error
	}{
		{name: "current confirmed knowledge", reason: "stale guidance", prepare: func(t *testing.T, repo *SQLiteRepository) []string {
			seedConsolidationSource(t, repo, "src-a", "Run go test before committing.", nil)
			return []string{"src-a", "src-a"}
		}},
		{name: "candidate is refused", reason: "stale", wantErr: ErrLifecycleTransition, prepare: func(t *testing.T, repo *SQLiteRepository) []string {
			if err := repo.Append(context.Background(), ContextItem{ID: "cand", Kind: ContextDecision, Content: "Run go vet before committing.", Scope: consolidationTestScope, Lifecycle: LifecycleCandidate}); err != nil {
				t.Fatal(err)
			}
			return []string{"cand"}
		}},
		{name: "superseded knowledge is refused", reason: "stale", wantErr: ErrLifecycleTransition, prepare: func(t *testing.T, repo *SQLiteRepository) []string {
			seedTwoConsolidationSources(t, repo)
			if err := repo.MarkSuperseded(context.Background(), []string{"src-a"}, "src-b"); err != nil {
				t.Fatal(err)
			}
			return []string{"src-a"}
		}},
		{name: "retired knowledge is refused", reason: "stale", wantErr: ErrLifecycleTransition, prepare: func(t *testing.T, repo *SQLiteRepository) []string {
			seedConsolidationSource(t, repo, "src-a", "Run go test before committing.", nil)
			if err := repo.RetireConfirmed(context.Background(), []string{"src-a"}, "first"); err != nil {
				t.Fatal(err)
			}
			return []string{"src-a"}
		}},
		{name: "consolidated candidate is refused", reason: "stale", wantErr: ErrReservedSourceType, prepare: func(t *testing.T, repo *SQLiteRepository) []string {
			seedTwoConsolidationSources(t, repo)
			proposal := approvedConsolidation(t, repo, "merged guidance", "src-a", "src-b")
			return []string{proposal.CandidateContextItemID}
		}},
		{name: "reason is required", reason: "  ", wantErr: errReasonRequired, prepare: func(t *testing.T, repo *SQLiteRepository) []string {
			seedConsolidationSource(t, repo, "src-a", "Run go test before committing.", nil)
			return []string{"src-a"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			ids := tt.prepare(t, repo)
			err := repo.RetireConfirmed(context.Background(), ids, tt.reason)
			if tt.wantErr != nil {
				if tt.wantErr == errReasonRequired {
					if err == nil || !strings.Contains(err.Error(), "reason is required") {
						t.Fatalf("RetireConfirmed error = %v, want a missing-reason error", err)
					}
					return
				}
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("RetireConfirmed error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			item, err := repo.Get(context.Background(), ids[0])
			if err != nil {
				t.Fatal(err)
			}
			if IsCurrentPersistentKnowledge(item, time.Now()) || item.ExpiresAt == nil || item.Metadata["retired_reason"] != tt.reason {
				t.Fatalf("retired item = %+v, want expired with reason %q", item, tt.reason)
			}
			var events int
			if err := repo.db.QueryRow("SELECT COUNT(*) FROM context_events WHERE event_type='retire' AND item_id=?", ids[0]).Scan(&events); err != nil || events != 1 {
				t.Fatalf("retire events = %d (err %v), want 1", events, err)
			}
		})
	}
}

func TestRetireConfirmedRedactsAndBoundsReason(t *testing.T) {
	repo := newConsolidationTestRepo(t)
	seedConsolidationSource(t, repo, "src-a", "Run go test before committing.", nil)
	reason := "leaked password=hunter2-retire-secret " + strings.Repeat("x", 2*maxRetireReasonRunes)
	if err := repo.RetireConfirmed(context.Background(), []string{"src-a"}, reason); err != nil {
		t.Fatal(err)
	}
	item, err := repo.Get(context.Background(), "src-a")
	if err != nil {
		t.Fatal(err)
	}
	stored := item.Metadata["retired_reason"]
	if strings.Contains(stored, "hunter2-retire-secret") || len([]rune(stored)) > maxRetireReasonRunes {
		t.Fatalf("stored reason = %q, want redacted and at most %d runes", stored, maxRetireReasonRunes)
	}
}

// errReasonRequired selects the missing-reason expectation in the table.
var errReasonRequired = errors.New("reason required")
