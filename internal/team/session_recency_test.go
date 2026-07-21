package team

import (
	"fmt"
	"strings"
	"testing"
)

func TestSessionRecency_Entries41_80_200(t *testing.T) {
	counts := []int{41, 80, 200}

	for _, count := range counts {
		t.Run(fmt.Sprintf("%d_entries", count), func(t *testing.T) {
			sd := NewSession()
			sd.Rounds = count / 2

			// Add filler entries
			for i := 0; i < count-2; i++ {
				sd.AddEntry("user", fmt.Sprintf("Filler message %d", i))
			}

			// Add latest user correction
			userCorrection := fmt.Sprintf("USER_CORRECTION: Do not edit file_%d.go, edit main.go instead", count)
			sd.AddEntry("user", userCorrection)

			// Add latest assistant decision
			assistantDecision := fmt.Sprintf("ASSISTANT_DECISION: Switched target to main.go for session test %d", count)
			sd.AddEntry("assistant", assistantDecision)

			summary := sd.ContextSummary()

			// 1. Verify expected omitted count message: "... (N older exchanges omitted)"
			expectedOmitted := count - maxSessionEntries
			wantOmittedMsg := fmt.Sprintf("... (%d older exchanges omitted)", expectedOmitted)
			if !strings.Contains(summary, wantOmittedMsg) {
				t.Errorf("ContextSummary() missing expected omitted header %q, got:\n%s", wantOmittedMsg, summary)
			}

			// 2. Verify latest user correction exists
			if !strings.Contains(summary, userCorrection) {
				t.Errorf("ContextSummary() for %d entries missing latest user correction %q", count, userCorrection)
			}

			// 3. Verify latest assistant decision exists
			if !strings.Contains(summary, assistantDecision) {
				t.Errorf("ContextSummary() for %d entries missing latest assistant decision %q", count, assistantDecision)
			}

			// 4. Verify oldest entry (Filler message 0) is NOT present because it was omitted
			oldestEntry := "Filler message 0"
			if strings.Contains(summary, oldestEntry) {
				t.Errorf("ContextSummary() for %d entries should NOT contain oldest entry %q", count, oldestEntry)
			}

			// Also test GenerateSessionMD for recency
			md := GenerateSessionMD(sd, "test-team")
			wantMdOmitted := fmt.Sprintf("*... %d older exchanges omitted*", expectedOmitted)
			if !strings.Contains(md, wantMdOmitted) {
				t.Errorf("GenerateSessionMD() missing expected omitted header %q", wantMdOmitted)
			}
			if !strings.Contains(md, userCorrection) {
				t.Errorf("GenerateSessionMD() for %d entries missing latest user correction", count)
			}
			if !strings.Contains(md, assistantDecision) {
				t.Errorf("GenerateSessionMD() for %d entries missing latest assistant decision", count)
			}
		})
	}
}

func TestResumedAndUninterruptedSessionEquivalentContext(t *testing.T) {
	workspace := t.TempDir()

	// 1. Construct uninterrupted session data with 200 entries
	uninterruptedSession := NewSession()
	for i := 0; i < 198; i++ {
		uninterruptedSession.AddEntry("user", fmt.Sprintf("Uninterrupted filler %d", i))
	}
	userCorrection := "USER_CORRECTION: Keep current configuration intact"
	assistantDecision := "ASSISTANT_DECISION: Preserving configuration as requested"
	uninterruptedSession.AddEntry("user", userCorrection)
	uninterruptedSession.AddEntry("assistant", assistantDecision)

	uninterruptedContext := uninterruptedSession.ContextSummary()

	// 2. Save session to simulate persistence
	if err := SaveSession(workspace, uninterruptedSession); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// 3. Load session to simulate resumed session
	resumedSession := LoadSession(workspace)
	if resumedSession == nil {
		t.Fatal("LoadSession returned nil")
	}

	resumedContext := resumedSession.ContextSummary()

	// 4. Verify uninterrupted context and resumed context are EQUIVALENT
	if uninterruptedContext != resumedContext {
		t.Errorf("Resumed session context differs from uninterrupted session context!\nUninterrupted:\n%s\nResumed:\n%s", uninterruptedContext, resumedContext)
	}

	// 5. Verify both contain latest user correction and assistant decision
	if !strings.Contains(resumedContext, userCorrection) {
		t.Errorf("Resumed session context missing user correction: %q", userCorrection)
	}
	if !strings.Contains(resumedContext, assistantDecision) {
		t.Errorf("Resumed session context missing assistant decision: %q", assistantDecision)
	}
}
