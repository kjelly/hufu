package context

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestEvaluateConsolidationProposalReasons(t *testing.T) {
	cases := []struct {
		name    string
		approve bool
		mutate  func(t *testing.T, repo *SQLiteRepository, p ConsolidationProposal)
		state   ConsolidationFreshnessState
		reason  ConsolidationReason
	}{
		{name: "fresh", state: ConsolidationFresh},
		{name: "candidate missing", mutate: execSQL("DELETE FROM context_items WHERE id=?", candidateArg), state: ConsolidationInvalid, reason: ReasonCandidateMissing},
		{name: "candidate link mismatch", mutate: execSQL(`UPDATE context_items SET source_json='{"type":"consolidation_proposal","ref":"other"}' WHERE id=?`, candidateArg), state: ConsolidationInvalid, reason: ReasonCandidateLinkMismatch},
		{name: "candidate shared", mutate: func(t *testing.T, repo *SQLiteRepository, p ConsolidationProposal) {
			if _, err := repo.db.Exec(`INSERT INTO consolidation_proposals(id,project_id,team_id,candidate_context_item_id,source_ids_json,source_revisions_json,aggregate_revisions_json,status,reason,created_at) SELECT 'consolidation-copy',project_id,team_id,candidate_context_item_id,source_ids_json,source_revisions_json,aggregate_revisions_json,'rejected','',created_at FROM consolidation_proposals WHERE id=?`, p.ID); err != nil {
				t.Fatal(err)
			}
		}, state: ConsolidationInvalid, reason: ReasonCandidateShared},
		{name: "approved but candidate rejected", approve: true, mutate: execSQL("UPDATE context_items SET lifecycle='rejected' WHERE id=?", candidateArg), state: ConsolidationInvalid, reason: ReasonLifecycleMismatch},
		{name: "edge missing", mutate: execSQL("DELETE FROM context_edges WHERE from_id=? AND to_id='src-a'", candidateArg), state: ConsolidationInvalid, reason: ReasonEdgeMismatch},
		{name: "frozen revision missing", mutate: execSQL("UPDATE consolidation_proposals SET source_revisions_json='{}' WHERE id=?", proposalArg), state: ConsolidationInvalid, reason: ReasonSourceSetInvalid},
		{name: "source conflict", mutate: func(t *testing.T, repo *SQLiteRepository, _ ConsolidationProposal) {
			a, _ := repo.Get(context.Background(), "src-a")
			b, _ := repo.Get(context.Background(), "src-b")
			j := PairJudgment{ProjectID: "project", TeamID: "team", ItemAID: a.ID, ItemBID: b.ID, ItemAContentHash: a.ContentHash, ItemBContentHash: b.ContentHash, Verdict: PairVerdictContradicts, JudgePolicyVersion: CurrentConflictJudgePolicyVersion, JudgeModel: "judge"}
			NormalizePairJudgment(&j)
			if _, _, err := repo.SavePairJudgment(context.Background(), j, "operator", false); err != nil {
				t.Fatal(err)
			}
		}, state: ConsolidationBlocked, reason: ReasonSourceConflictOpen},
		{name: "marked stale", mutate: execSQL("UPDATE consolidation_proposals SET status='stale' WHERE id=?", proposalArg), state: ConsolidationStale, reason: ReasonMarkedStale},
		{name: "source missing", mutate: execSQL("DELETE FROM context_items WHERE id='src-b' AND ?<>''", proposalArg), state: ConsolidationStale, reason: ReasonSourceMissing},
		{name: "source not confirmed", mutate: execSQL("UPDATE context_items SET lifecycle='rejected' WHERE id='src-b' AND ?<>''", proposalArg), state: ConsolidationStale, reason: ReasonSourceNotConfirmed},
		{name: "source superseded", mutate: execSQL("UPDATE context_items SET superseded_by='src-a' WHERE id='src-b' AND ?<>''", proposalArg), state: ConsolidationStale, reason: ReasonSourceSuperseded},
		{name: "source expired", mutate: execSQL("UPDATE context_items SET expires_at=1 WHERE id='src-b' AND ?<>''", proposalArg), state: ConsolidationStale, reason: ReasonSourceExpired},
		{name: "source outside validity", mutate: execSQL("UPDATE context_items SET valid_until=1 WHERE id='src-b' AND ?<>''", proposalArg), state: ConsolidationStale, reason: ReasonSourceOutsideValidity},
		{name: "source revision changed", mutate: execSQL("UPDATE context_items SET content_hash='changed' WHERE id='src-b' AND ?<>''", proposalArg), state: ConsolidationStale, reason: ReasonSourceRevisionChanged},
		{name: "source scope changed", mutate: execSQL("UPDATE context_items SET team_id='other' WHERE id='src-b' AND ?<>''", proposalArg), state: ConsolidationStale, reason: ReasonSourceScopeMismatch},
		{name: "source contradiction", mutate: execSQL(`UPDATE context_items SET metadata_json='{"contradicts_ids":"src-a"}' WHERE id='src-b' AND ?<>''`, proposalArg), state: ConsolidationStale, reason: ReasonSourceContradiction},
		{name: "approved candidate superseded", approve: true, mutate: execSQL("UPDATE context_items SET superseded_by='src-a' WHERE id=?", candidateArg), state: ConsolidationStale, reason: ReasonCandidateSuperseded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newConsolidationTestRepo(t)
			seedTwoConsolidationSources(t, repo)
			proposal := mustCreateConsolidation(t, repo, "merged guidance", "src-a", "src-b")
			if tc.approve {
				if _, err := repo.ApproveConsolidationProposal(context.Background(), consolidationReview(proposal.ID, "approve")); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mutate != nil {
				tc.mutate(t, repo, proposal)
			}
			current, err := repo.GetConsolidationProposal(context.Background(), proposal.ID)
			if err != nil {
				t.Fatal(err)
			}
			got, err := repo.EvaluateConsolidationProposal(context.Background(), current, consolidationTestPolicy, false, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.state || (tc.reason != "" && !slices.Contains(got.Reasons, tc.reason)) {
				t.Fatalf("freshness = %+v, want %s with %s", got, tc.state, tc.reason)
			}
			if (got.Err() == nil) != (tc.state == ConsolidationFresh) {
				t.Fatalf("Err() = %v for state %s", got.Err(), got.State)
			}
		})
	}
}

type sqlArg func(ConsolidationProposal) string

func candidateArg(p ConsolidationProposal) string { return p.CandidateContextItemID }
func proposalArg(p ConsolidationProposal) string  { return p.ID }

func execSQL(query string, arg sqlArg) func(*testing.T, *SQLiteRepository, ConsolidationProposal) {
	return func(t *testing.T, repo *SQLiteRepository, p ConsolidationProposal) {
		t.Helper()
		if _, err := repo.db.Exec(query, arg(p)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConsolidationStatePrecedence(t *testing.T) {
	cases := []struct {
		reasons []ConsolidationReason
		want    ConsolidationFreshnessState
	}{
		{nil, ConsolidationFresh},
		{[]ConsolidationReason{ReasonSourceSuperseded}, ConsolidationStale},
		{[]ConsolidationReason{ReasonSourceSuperseded, ReasonSourceConflictOpen}, ConsolidationBlocked},
		{[]ConsolidationReason{ReasonSourceConflictOpen, ReasonEdgeMismatch}, ConsolidationInvalid},
		{[]ConsolidationReason{ReasonSourceMissing, ReasonCandidateMissing}, ConsolidationInvalid},
	}
	for _, tc := range cases {
		set := map[ConsolidationReason]bool{}
		for _, reason := range tc.reasons {
			set[reason] = true
		}
		if _, got := consolidationStateFor(set); got != tc.want {
			t.Fatalf("state for %v = %s, want %s", tc.reasons, got, tc.want)
		}
	}
}

func TestConsolidationConcurrentCreateAndReview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	seed, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	seedTwoConsolidationSources(t, seed)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	handles := make([]*SQLiteRepository, 2)
	for i := range handles {
		if handles[i], err = OpenSQLite(path); err != nil {
			t.Fatal(err)
		}
		defer func(repo *SQLiteRepository) { _ = repo.Close() }(handles[i])
	}
	var wg sync.WaitGroup
	createdCount := make([]bool, len(handles))
	ids := make([]string, len(handles))
	errs := make([]error, len(handles))
	for i, repo := range handles {
		wg.Add(1)
		go func(i int, repo *SQLiteRepository) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var proposal ConsolidationProposal
			proposal, createdCount[i], errs[i] = repo.CreateConsolidationProposal(ctx, consolidationInput("merged guidance", "src-a", "src-b"))
			ids[i] = proposal.ID
		}(i, repo)
	}
	wg.Wait()
	created := 0
	for i := range handles {
		if errs[i] != nil {
			t.Fatalf("concurrent create %d: %v", i, errs[i])
		}
		if createdCount[i] {
			created++
		}
	}
	if created != 1 || ids[0] != ids[1] {
		t.Fatalf("concurrent create created=%d ids=%v, want exactly one proposal", created, ids)
	}
	reviewErrs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, reviewErrs[0] = handles[0].ApproveConsolidationProposal(context.Background(), consolidationReview(ids[0], "approve"))
	}()
	go func() {
		defer wg.Done()
		_, reviewErrs[1] = handles[1].RejectConsolidationProposal(context.Background(), consolidationReview(ids[0], "reject"))
	}()
	wg.Wait()
	if (reviewErrs[0] == nil) == (reviewErrs[1] == nil) {
		t.Fatalf("approve err=%v reject err=%v, want exactly one success", reviewErrs[0], reviewErrs[1])
	}
	for _, err := range reviewErrs {
		if err != nil && !errors.Is(err, ErrConsolidationNotPending) {
			t.Fatalf("losing review err = %v, want ErrConsolidationNotPending", err)
		}
	}
	proposal, err := handles[0].GetConsolidationProposal(context.Background(), ids[0])
	if err != nil {
		t.Fatal(err)
	}
	freshness, err := handles[0].EvaluateConsolidationProposal(context.Background(), proposal, consolidationTestPolicy, false, 0)
	if err != nil || slices.Contains(freshness.Reasons, ReasonLifecycleMismatch) {
		t.Fatalf("final state inconsistent: %+v err=%v", freshness, err)
	}
}

// TestConsolidationCreateWaitsForConcurrentWriter holds one handle's create
// transaction open after its first write. The second handle must wait for
// that commit and return the same proposal instead of exhausting its busy
// retries on a read snapshot that SQLite cannot upgrade to a write.
func TestConsolidationCreateWaitsForConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "context.sqlite")
	seed, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	seedTwoConsolidationSources(t, seed)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	handles := make([]*SQLiteRepository, 2)
	for i := range handles {
		if handles[i], err = OpenSQLite(path); err != nil {
			t.Fatal(err)
		}
		defer func(repo *SQLiteRepository) { _ = repo.Close() }(handles[i])
	}
	held, release := make(chan struct{}), make(chan struct{})
	var holdOnce sync.Once
	consolidationTxTestHook = func(stage string) error {
		if stage == "candidate" {
			holdOnce.Do(func() {
				close(held)
				<-release
			})
		}
		return nil
	}
	t.Cleanup(func() { consolidationTxTestHook = nil })

	type createResult struct {
		proposal ConsolidationProposal
		created  bool
		err      error
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	create := func(repo *SQLiteRepository) <-chan createResult {
		done := make(chan createResult, 1)
		go func() {
			proposal, created, err := repo.CreateConsolidationProposal(ctx, consolidationInput("merged guidance", "src-a", "src-b"))
			done <- createResult{proposal, created, err}
		}()
		return done
	}
	firstDone := create(handles[0])
	select {
	case <-held:
	case got := <-firstDone:
		t.Fatalf("first create finished before holding its transaction: %+v", got)
	}
	secondDone := create(handles[1])
	// Hold the first transaction well past the busy-retry backoff. A second
	// create that cannot wait for the writer fails inside this window.
	var second createResult
	select {
	case second = <-secondDone:
		close(release)
	case <-time.After(500 * time.Millisecond):
		close(release)
		second = <-secondDone
	}
	first := <-firstDone
	if first.err != nil || !first.created {
		t.Fatalf("first create = created %v, err %v; want a new proposal", first.created, first.err)
	}
	if second.err != nil || second.created || second.proposal.ID != first.proposal.ID {
		t.Fatalf("second create = %q created %v, err %v; want existing proposal %q", second.proposal.ID, second.created, second.err, first.proposal.ID)
	}
}
