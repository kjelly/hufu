package context

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestConsolidationPreservesSourceEvidence(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	proposal := ConsolidationProposal{
		ID: "proposal-1", ProjectID: "project", CandidateContextItemID: "candidate-1",
		SourceIDs:          []string{"source-a", "source-b"},
		SourceRevisions:    map[string]string{"source-a": "hash-a", "source-b": "hash-b"},
		AggregateRevisions: map[string]int64{"source-a": 3, "source-b": 7},
		Status:             "proposed", CreatedAt: time.Unix(100, 0).UTC(),
	}
	if err := repo.SaveConsolidationProposal(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetConsolidationProposal(context.Background(), proposal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.SourceIDs, proposal.SourceIDs) || !reflect.DeepEqual(got.SourceRevisions, proposal.SourceRevisions) || !reflect.DeepEqual(got.AggregateRevisions, proposal.AggregateRevisions) {
		t.Fatalf("proposal provenance = %+v", got)
	}
}
