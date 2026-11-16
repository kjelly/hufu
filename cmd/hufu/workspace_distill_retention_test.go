package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/spf13/cobra"
)

func TestExtractionOnlyPlanNeverAuthorizesSourceDeletion(t *testing.T) {
	for _, action := range []string{"retain", "discard", "distill", taskdistill.ActionExtractOnly} {
		t.Run(action, func(t *testing.T) {
			plan := workspaceDistillationPlan{ExtractOnly: true, Sources: []taskdistill.Source{{ID: "source", Content: "frozen evidence", Protected: true}}}
			plan.Draft.Decisions = []taskdistill.Decision{{SourceID: "source", Action: action, Reason: "model classification"}}
			if action == "distill" || action == taskdistill.ActionExtractOnly {
				plan.Draft.Knowledge = []taskdistill.Knowledge{{Text: "Supported procedure with its limitations.", SourceIDs: []string{"source"}}}
			}
			projected := distillationGenerationSources(plan)
			if projected[0].Protected || !plan.Sources[0].Protected || projected[0].Content != plan.Sources[0].Content {
				t.Fatal("generation mutated canonical source evidence")
			}
			retainDistillationSources(&plan.Draft)
			if err := validateDistillationPlanDraft(plan); err != nil || !taskdistill.RetainsSource(plan.Draft.Decisions[0].Action) {
				t.Fatalf("unsafe retained plan: %+v err=%v", plan.Draft, err)
			}
			plan.Sources[0].Protected = false
			plan.Draft.Decisions[0].Action = "discard"
			if err := validateDistillationPlanDraft(plan); !errors.Is(err, taskdistill.ErrInvalid) {
				t.Fatal("extraction-only scope admitted deletion of an unprotected source")
			}
		})
	}
}

func TestExtractionOnlyCheckpointsCannotReuseCleanupAdmission(t *testing.T) {
	workspace := t.TempDir()
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetErr(new(bytes.Buffer))
	generator := identityTextGenerator{identity: "fixture-policy", generate: func(context.Context, string) (string, error) {
		t.Fatal("checkpoint scope construction called a model")
		return "", nil
	}}
	plan := workspaceDistillationPlan{Sources: []taskdistill.Source{{ID: "source", Protected: true}}, MaxModelCalls: 32}
	ordinary, closeOrdinary, err := newDistillationCheckpointGenerator(command, workspace, plan, generator, generator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeOrdinary)
	plan.ExtractOnly = true
	retained, closeRetained, err := newDistillationCheckpointGenerator(command, workspace, plan, generator, generator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeRetained)
	if ordinary.(*distillationCheckpointGenerator).job == retained.(*distillationCheckpointGenerator).job {
		t.Fatal("different cleanup admissions share checkpoints")
	}
	plan.MaxModelCalls = 28
	resumed, closeResumed, err := newDistillationCheckpointGenerator(command, workspace, plan, generator, generator)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeResumed)
	if resumed.(*distillationCheckpointGenerator).job != retained.(*distillationCheckpointGenerator).job {
		t.Fatal("remaining call allowance invalidated verified retained-source checkpoints")
	}
}
