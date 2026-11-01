package team

import (
	"reflect"
	"testing"
)

func TestResumePreflightRetainsFrozenNativeOutputContext(t *testing.T) {
	c := newRecoveredArtifactCoordinator(t, configureInputBoundContractRecovery)
	source, err := ReadResumeSource(c.session.Workspace, c.activeBranchID(), "")
	if err != nil {
		t.Fatal(err)
	}
	before, err := collectionDigest(source.Session)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"selected", "unselected", "wrong_branch"} {
		t.Run(kind, func(t *testing.T) {
			preview := newTestCoordinator(t)
			preview.executionRunID = ""
			preview.session, preview.projectDir = c.session, c.projectDir
			store, err := OpenEventStoreReadOnly(c.session.Workspace)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			previousData, previousTracker := preview.sessionData, preview.taskTracker
			func() {
				restore := preview.installResumeValidationSource(source, store)
				defer restore()
				switch kind {
				case "unselected":
					preview.runInputSelectionRunID = ""
				case "wrong_branch":
					store.SetBranchID("main")
				}
				value, err := preview.resolveRuntimeOutputValue(FactRef{Name: "scope", TaskID: "source", RuntimeOutput: "scope"}, "scope")
				if kind == "selected" {
					if err != nil || !sameCanonicalEvidence(value, map[string]any{"satisfied": true}) || preview.executionRunID == "source-run" {
						t.Fatalf("frozen native output=%v error=%v", value, err)
					}
				} else if err == nil {
					t.Fatal("native output accepted without selected canonical recovery scope")
				}
			}()
			if preview.sessionData != previousData || preview.taskTracker != previousTracker || preview.eventStore != nil || preview.executionRunID != "" || preview.runInputSelectionRunID != "" {
				t.Fatal("preview retained its temporary recovery context")
			}
		})
	}
	after, err := collectionDigest(source.Session)
	if err != nil || before != after {
		t.Fatal("preview changed its canonical source")
	}
}

func TestResumePreflightUsesCanonicalSharedWorksetReceipt(t *testing.T) {
	c, f, source := metadataContractRecoveryFixture(t, func(c *Coordinator) {
		// The legacy event omits this field even though its contract hash
		// includes it. Check both preflight hydration and canonical comparison.
		c.session.ContractTasks[1].InvariantVerification = InvariantVerificationReport
	})
	ctx := WithMetadataOnlyContractRecovery(t.Context())
	plan, err := c.AssessContractRecovery(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := c.ApplyContractRecovery(ctx, source, "checked-recovery", plan.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	child, err := ReadResumeSource(f.control, branch, "")
	if err != nil {
		t.Fatal(err)
	}
	if child.Session.Tasks[1].WorksetReceipt == nil || child.Session.Tasks[2].WorksetReceipt != nil {
		t.Fatal("fixture does not use the runtime's single shared receipt layout")
	}
	if child.Session.Tasks[1].InvariantVerification != "" {
		t.Fatal("fixture does not preserve the legacy omitted invariant mode")
	}
	before := mustReadEvents(t, f.events)
	beforeSource, err := collectionDigest(child.Session)
	if err != nil {
		t.Fatal(err)
	}
	for _, drift := range []bool{false, true} {
		preview := newTestCoordinator(t)
		preview.executionRunID = ""
		preview.session, preview.providerManager, preview.maxConcurrent, preview.projectDir = c.session, c.providerManager, c.maxConcurrent, c.projectDir
		if drift {
			preview.maxConcurrent++
		}
		assessment, err := preview.AssessResumeSource(t.Context(), child)
		if drift {
			if err == nil {
				t.Fatal("preflight accepted execution policy drift")
			}
		} else if err != nil || assessment.Accepted != 1 || assessment.Resumable != 2 || assessment.TerminalUnresolved != 0 || assessment.Branch != branch {
			t.Fatalf("resume preflight: %+v %v", assessment, err)
		}
		if preview.eventStore != nil || preview.executionRunID != "" || len(preview.taskTracker.TodoList().Items()) != 0 {
			t.Fatal("preflight retained an active execution or source projection")
		}
	}
	afterSource, err := collectionDigest(child.Session)
	if err != nil || beforeSource != afterSource || !reflect.DeepEqual(before, mustReadEvents(t, f.events)) {
		t.Fatal("read-only preflight changed its source or event history")
	}
}

func TestRecoveredArtifactOperatorDiagnosticPreservesModelBoundary(t *testing.T) {
	c := newRecoveredArtifactCoordinator(t)
	consumer := c.todoItemByID("2")
	id := consumer.WorksetBinding.Inputs[0].ID
	// Canonical replay cannot authorize a receipt without its event store.
	c.eventStore = nil
	if _, _, err := c.resolveAuthorizedWorksetInput(consumer, id); err == nil {
		t.Fatal("operator preflight lost the underlying rejection")
	}
	if _, _, ok := c.authorizedWorksetInput(consumer, id); ok {
		t.Fatal("a diagnostic change admitted the missing canonical source")
	}
	if _, err := c.buildArtifactAccessScope(consumer.ID, 1); err == nil {
		t.Fatal("scope preflight accepted the missing canonical source")
	}
}
