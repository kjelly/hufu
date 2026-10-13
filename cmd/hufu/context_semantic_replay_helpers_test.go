package main

import (
	"testing"
)

// isolateContextSemanticConfig keeps explain-memory and ranking-replay tests
// from reading the developer's hufu.yaml or teams, which may enable the
// semantic lane against a live encoder, and resets their semantic flags.
func isolateContextSemanticConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	oldSearchPath := opts.agentTeamSearchPath
	opts.agentTeamSearchPath = t.TempDir()
	t.Cleanup(func() { opts.agentTeamSearchPath = oldSearchPath })
	contextSemanticMode, rankingReplayCandidateSM = "", ""
	for _, flag := range []string{"semantic-mode", "candidate-semantic-mode", "candidate-fusion"} {
		if f := contextRankingReplayCmd.Flags().Lookup(flag); f != nil {
			f.Changed = false
		}
	}
	if f := contextExplainMemoryCmd.Flags().Lookup("semantic-mode"); f != nil {
		f.Changed = false
	}
}
