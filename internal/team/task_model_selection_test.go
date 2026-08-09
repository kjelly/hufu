package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/config"
)

func TestSelectModelForComplexityUsesRelativeTierOnly(t *testing.T) {
	t.Parallel()
	profiles := []struct {
		name    string
		profile TaskComplexityProfile
		index   int
	}{
		{name: "small read", profile: TaskComplexityProfile{StepCount: 1}, index: 0},
		{name: "moderate validated workflow", profile: TaskComplexityProfile{StepCount: 4, RequiresVerification: true}, index: 1},
		{name: "high risk repaired mutation", profile: TaskComplexityProfile{StepCount: 6, MutationSteps: 1, RepairHistory: 1, RequiresVerification: true}, index: 2},
	}
	modelSets := [][]config.ModelEntry{
		{{ID: "provider-a/tiny"}, {ID: "provider-a/medium"}, {ID: "provider-a/large"}},
		{{ID: "unrelated-x/one"}, {ID: "unrelated-y/two"}, {ID: "random-provider-name/three"}},
	}
	for _, models := range modelSets {
		models := models
		for _, tc := range profiles {
			t.Run(models[0].ID+"/"+tc.name, func(t *testing.T) {
				if got := SelectModelForComplexity(models, tc.profile); got != models[tc.index].ID {
					t.Fatalf("SelectModelForComplexity() = %q, want relative tier %q", got, models[tc.index].ID)
				}
			})
		}
	}
}

func TestTaskComplexityProfileUsesContractSignals(t *testing.T) {
	t.Parallel()
	task := TaskDef{
		Goal:      "apply a validated change",
		DependsOn: []int{0, 1},
		Execution: ExecutionContract{
			RequiresVerification: true,
			Steps: []ExecutionStep{
				{ID: "produce", Effect: ExecutionEffectProduce},
				{ID: "validate", Effect: ExecutionEffectValidate},
				{ID: "mutate", Effect: ExecutionEffectMutate},
			},
		},
	}
	got := taskComplexityProfile(task)
	if got.StepCount != 3 || got.MutationSteps != 1 || got.DependencyCount != 2 || !got.RequiresVerification {
		t.Fatalf("unexpected profile: %+v", got)
	}
}

func TestSelectTaskModelPreservesExplicitOverride(t *testing.T) {
	t.Parallel()
	c := &Coordinator{modelList: []config.ModelEntry{{ID: "weak"}, {ID: "strong"}}}
	if got := c.selectTaskModel(TaskDef{Model: "chosen"}); got != "chosen" {
		t.Fatalf("selectTaskModel() = %q, want explicit override", got)
	}
}

func TestStructuredStepModelUsesProviderNeutralCheckpointProfiles(t *testing.T) {
	t.Parallel()
	models := []config.ModelEntry{{ID: "random-a/low"}, {ID: "unrelated-b/mid"}, {ID: "opaque-c/high"}}
	c := &Coordinator{modelList: models}
	task := TaskDef{Execution: ExecutionContract{Steps: []ExecutionStep{
		{ID: "discover", Effect: ExecutionEffectRead},
		{ID: "produce", Effect: ExecutionEffectProduce},
		{ID: "validate", Effect: ExecutionEffectValidate},
		{ID: "mutate", Effect: ExecutionEffectMutate},
		{ID: "verify", Effect: ExecutionEffectVerify},
		{ID: "publish", Effect: ExecutionEffectMutate},
	}}}
	producer := task.Execution.Steps[1]
	validator := task.Execution.Steps[2]
	if got := c.selectStructuredStepModel(task, validator, 0); got != models[0].ID {
		t.Fatalf("validator model = %q, want low relative tier", got)
	}
	if got := c.selectStructuredStepModel(task, producer, 0); got != models[1].ID {
		t.Fatalf("producer model = %q, want middle relative tier", got)
	}
	if got := c.selectStructuredStepModel(task, producer, 1); got != models[2].ID {
		t.Fatalf("repair model = %q, want strongest relative tier", got)
	}
}
