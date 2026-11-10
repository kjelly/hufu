package team

import (
	"testing"

	"charm.land/fantasy"
)

func TestCompactionRetentionReconcilesSupersededAttestations(t *testing.T) {
	for _, scenario := range []string{"same generation", "replaced generation"} {
		t.Run(scenario, func(t *testing.T) {
			workspace := t.TempDir()
			state := testSnapshotCompactionState(t, workspace)
			old := state.Branches["main"]
			generation := state.Generations[old.GenerationID]
			oldGenerationEvent, err := compactionGenerationAttestationEvent(generation)
			if err != nil {
				t.Fatal(err)
			}
			oldCheckpointEvent, err := compactionCheckpointAttestationEvent(old, generation)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "replaced generation" {
				generation.ID += "-replacement"
				generation.Checksum = digestGeneration(generation)
				state.Generations[generation.ID] = generation
			}
			current := old
			current.GenerationID = generation.ID
			current.History = append(cloneMessages(old.History), fantasy.NewUserMessage("new input"))
			current.SourceCounts = append(append([]int(nil), old.SourceCounts...), 1)
			current.SourceRanges = append(cloneSourceRanges(old.SourceRanges), []CompactionRange{{StartIndex: old.NextSourceIndex, EndIndex: old.NextSourceIndex, MsgCount: 1}})
			current.NextSourceIndex++
			current.HistoryDigest = digestMessages(current.History)
			current.EventID = compactionCheckpointEventID(current)
			state.Branches["main"] = current
			state.Checkpoints["main"] = append(state.Checkpoints["main"], current)
			currentGenerationEvent, err := compactionGenerationAttestationEvent(generation)
			if err != nil {
				t.Fatal(err)
			}
			currentCheckpointEvent, err := compactionCheckpointAttestationEvent(current, generation)
			if err != nil {
				t.Fatal(err)
			}
			events := []RunEvent{oldGenerationEvent, oldCheckpointEvent}
			if scenario == "replaced generation" {
				events = append(events, currentGenerationEvent)
			}
			events = append(events, currentCheckpointEvent)
			es, err := NewEventStore(workspace, "retention", "retention")
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if _, err := es.AppendPersisted(event); err != nil {
					t.Fatal(err)
				}
			}
			if err := es.Close(); err != nil {
				t.Fatal(err)
			}
			if err := SaveSessionTree(workspace, NewSessionTree()); err != nil {
				t.Fatal(err)
			}
			if err := SaveConversationCompactionState(workspace, state); err != nil {
				t.Fatal(err)
			}
			loaded, exists, err := LoadConversationCompactionState(workspace)
			if err != nil || !exists || len(loaded.Checkpoints["main"]) != 1 || len(loaded.Generations) != 1 {
				t.Fatal("retention did not bound canonical checkpoint bodies", exists, err)
			}
			if gaps, err := validateCompactionBranchContext(loaded, "main", events, nil, ""); err != nil || len(gaps) != 0 {
				t.Fatal("superseded attestations blocked the current checkpoint", gaps, err)
			}
			for _, mutation := range []string{"old identity", "old branch", "current payload", "missing successor"} {
				t.Run(mutation, func(t *testing.T) {
					changed := append([]RunEvent(nil), events...)
					switch mutation {
					case "old identity":
						changed[1].ID = "unrelated-checkpoint"
					case "old branch":
						ref := CompactionCheckpointReference{BranchID: "other", GenerationID: old.GenerationID, GenerationChecksum: generation.Checksum, CheckpointDigest: digestCompactionCheckpoint(old)}
						changed[1].Payload = compactionJSON(ref)
					case "current payload":
						changed[len(changed)-1].Payload = oldCheckpointEvent.Payload
					case "missing successor":
						changed = changed[:len(changed)-1]
					}
					if _, err := validateCompactionBranchContext(loaded, "main", changed, nil, ""); err == nil {
						t.Fatal("invalid or unattested retention boundary was accepted")
					}
				})
			}
		})
	}
}
