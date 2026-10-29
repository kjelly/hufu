package team

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

var errDependencyResultViewUnprepared = errors.New("selected dependency context requires canonical source preparation")

// compileWorkerContext is the runtime boundary shared by dispatch, auxiliary
// repair, nested workers and fanout. A compiler service cannot replace the
// canonical source resolver or omit required selected fields before a call.
func (c *Coordinator) compileWorkerContext(ctx context.Context, input WorkerContextInput) (CompiledContext, error) {
	prepared, err := c.prepareDependencyResultView(ctx, input)
	if err != nil {
		return CompiledContext{}, err
	}
	prepared, err = c.prepareHistoricalResultContext(ctx, prepared)
	if err != nil {
		return CompiledContext{}, err
	}
	compiled, err := c.ContextCompiler().CompileWorkerContext(ctx, prepared)
	if err != nil {
		return compiled, err
	}
	if err := validateRequiredEvidenceContext(prepared, compiled); err != nil {
		return CompiledContext{}, err
	}
	if err := validateHistoricalResultContext(prepared, compiled); err != nil {
		return CompiledContext{}, err
	}
	return compiled, nil
}

func (c *Coordinator) dependencyViewOwner(taskID string) *Coordinator {
	if c.isModelFanoutLeaf(taskID) {
		return c.modelFanout.parent
	}
	return c
}

func (c *Coordinator) prepareDependencyResultView(ctx context.Context, input WorkerContextInput) (WorkerContextInput, error) {
	policy, err := input.TaskDef.DependencyResultView.normalized()
	if err != nil {
		return input, err
	}
	if policy == nil {
		if input.DependencyView != nil {
			return input, fmt.Errorf("full dependency context cannot accept a selected override")
		}
		return input, nil
	}
	owner := c.dependencyViewOwner(input.Request.TaskID)
	item := owner.todoItemByID(input.Request.TaskID)
	if item == nil {
		return input, fmt.Errorf("selected dependency view requires an admitted task")
	}
	frozen := taskDefFromTodoItem(item)
	def, _, err := owner.resolveAgentName(item.Agent)
	if err != nil {
		return input, err
	}
	if err := validateDependencyResultViewTask(frozen, def, true); err != nil {
		return input, err
	}
	if input.TaskDef.ContractID != frozen.ContractID || input.TaskDef.ContractHash != frozen.ContractHash || input.TaskDef.ContractRevision != frozen.ContractRevision {
		return input, fmt.Errorf("selected dependency view static identity changed")
	}
	callerHash, err := dependencyViewPolicyDigest(policy)
	if err != nil {
		return input, err
	}
	frozenPolicy, err := frozen.DependencyResultView.normalized()
	if err != nil || frozenPolicy == nil {
		return input, fmt.Errorf("selected dependency view frozen policy is unavailable")
	}
	frozenHash, err := dependencyViewPolicyDigest(frozenPolicy)
	if err != nil || callerHash != frozenHash {
		return input, fmt.Errorf("selected dependency view policy drift")
	}
	sources, err := owner.dependencyViewSources(ctx, item, input.DependencyResults)
	if err != nil {
		return input, err
	}
	view, err := buildDependencyResultView(frozenPolicy, input.DependencyResults, sources)
	if err != nil {
		return input, err
	}
	if err := validateFrozenDependencyResultViews(item, &view.attribution); err != nil {
		return input, err
	}
	input.DependencyView = view
	return input, nil
}

func validateFrozenDependencyResultViews(item *TodoItem, expected *DependencyResultViewAttribution) error {
	for _, manifest := range item.ContextManifests {
		if !authoritativeDependencyViewRequest(manifest, item) {
			continue
		}
		if !supportedDeliveryContextManifest(manifest) {
			return fmt.Errorf("selected dependency view prior manifest is invalid")
		}
		found := false
		for _, entry := range manifest.Items {
			if entry.ID == "dependency_results" && entry.Kind == "dependency_result" && entry.Included {
				if !reflect.DeepEqual(entry.DependencyResultView, expected) {
					return fmt.Errorf("selected dependency view source or policy changed since dispatch")
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("selected dependency view prior dispatch proof is missing")
		}
	}
	return nil
}

func authoritativeDependencyViewRequest(manifest ContextInjectionManifest, item *TodoItem) bool {
	return manifest.TaskID == item.ID && manifest.ModelCalled && strings.EqualFold(manifest.Agent, item.Agent) &&
		((manifest.Trigger == ContextTriggerTaskDispatch && manifest.Purpose == contextPurposeForTrigger(ContextTriggerTaskDispatch)) ||
			(manifest.Trigger == ContextTriggerRetry && manifest.Purpose == contextPurposeForTrigger(ContextTriggerRetry)))
}

func validateDependencyResultViewInput(input WorkerContextInput) error {
	policy, err := input.TaskDef.DependencyResultView.normalized()
	if err != nil {
		return err
	}
	if policy == nil {
		if input.DependencyView != nil {
			return fmt.Errorf("full dependency context cannot carry a selected view")
		}
		return nil
	}
	if input.DependencyView == nil {
		return errDependencyResultViewUnprepared
	}
	expected, err := buildDependencyResultView(policy, input.DependencyResults, input.DependencyView.attribution.Sources)
	if err != nil {
		return err
	}
	if expected.content != input.DependencyView.content || !reflect.DeepEqual(expected.attribution, input.DependencyView.attribution) {
		return fmt.Errorf("selected dependency presentation or provenance changed")
	}
	return nil
}

func validateSelectedDependencyContext(input WorkerContextInput, compiled CompiledContext) error {
	policy, err := input.TaskDef.DependencyResultView.normalized()
	if err != nil || policy == nil {
		return err
	}
	if input.TaskDef.Execution.RequiresEvidence || len(input.TaskDef.EvidenceFrom) != 0 {
		return fmt.Errorf("selected dependency context cannot stand in for required complete evidence")
	}
	for _, item := range compiled.IncludedItems {
		if item.ID != "dependency_results" || item.Kind != "dependency_result" || !item.Required || item.Compressed || item.DependencyResultView == nil {
			continue
		}
		expected, err := buildDependencyResultView(policy, input.DependencyResults, item.DependencyResultView.Sources)
		if err != nil {
			return err
		}
		if expected.content == item.Content && reflect.DeepEqual(expected.attribution, *item.DependencyResultView) && strings.Contains(compiled.Prompt, expected.content) {
			if input.DependencyView != nil && (expected.content != input.DependencyView.content || !reflect.DeepEqual(expected.attribution, input.DependencyView.attribution)) {
				return fmt.Errorf("selected dependency compiler changed runtime-prepared source attribution")
			}
			return nil
		}
	}
	return fmt.Errorf("selected dependency context was omitted, compressed or changed")
}
