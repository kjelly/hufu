package team

import "testing"

// TestTaskTranscriptArtifactCarriesProducerAgent guards E-33: a verbatim task
// attaches its transcript to the typed result, and a dependent task accepts
// that artifact only when its recorded agent matches the producer's result.
func TestTaskTranscriptArtifactCarriesProducerAgent(t *testing.T) {
	cases := []struct {
		name        string
		agent       string
		resultAgent string
		wantValid   bool
	}{
		{name: "producer agent recorded", agent: "deployer", resultAgent: "deployer", wantValid: true},
		{name: "surrounding whitespace is ignored", agent: " deployer ", resultAgent: "deployer", wantValid: true},
		{name: "another agent's result is rejected", agent: "deployer", resultAgent: "verifier", wantValid: false},
		{name: "missing agent is rejected", agent: "", resultAgent: "deployer", wantValid: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			transcript, err := newTaskTranscriptForAttempt(workspace, "1", "run-1", 1, tc.agent)
			if err != nil {
				t.Fatalf("newTaskTranscriptForAttempt() error = %v", err)
			}
			t.Cleanup(func() { _ = transcript.Close() })
			if err := transcript.RecordAssistantOutput("done"); err != nil {
				t.Fatalf("RecordAssistantOutput() error = %v", err)
			}
			ref, err := transcript.Manifest()
			if err != nil {
				t.Fatalf("Manifest() error = %v", err)
			}
			result := &TaskResult{TaskID: "1", Attempt: 1, Agent: tc.resultAgent}
			if err := attachVerbatimTaskResult(ref, transcript, result); err != nil {
				t.Fatalf("attachVerbatimTaskResult() error = %v", err)
			}
			c := &Coordinator{executionRunID: "run-1"}
			for _, dependencyRef := range artifactRefsFromTaskResult(result) {
				err := c.validateCurrentProducerArtifactOccurrence(dependencyRef, "1", result)
				if gotValid := err == nil; gotValid != tc.wantValid {
					t.Fatalf("validateCurrentProducerArtifactOccurrence(%s) = %v, want valid=%v", dependencyRef.ID, err, tc.wantValid)
				}
			}
		})
	}
}
