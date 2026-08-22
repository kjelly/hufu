package team

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// ArtifactAccessScope is the immutable, attempt-local capability set for
// artifact_ref access. AuthorizedRefs are the only references the artifact
// opener may resolve for this attempt; DeniedRefs are used to block their
// backing/source paths from path-capable tools. The scope is coordinator-owned
// and is also committed on the task's execution receipt.
type ArtifactAccessScope struct {
	RunID          string        `json:"run_id"`
	TaskID         string        `json:"task_id"`
	Attempt        int           `json:"attempt"`
	AuthorizedRefs []ArtifactRef `json:"authorized_refs,omitempty"`
	DeniedRefs     []ArtifactRef `json:"denied_refs,omitempty"`
}

type artifactAccessScopeKeyType struct{}

var artifactAccessScopeKey = artifactAccessScopeKeyType{}

func cloneArtifactAccessScope(scope *ArtifactAccessScope) *ArtifactAccessScope {
	if scope == nil {
		return nil
	}
	copyScope := *scope
	copyScope.AuthorizedRefs = append([]ArtifactRef(nil), scope.AuthorizedRefs...)
	copyScope.DeniedRefs = append([]ArtifactRef(nil), scope.DeniedRefs...)
	return &copyScope
}

func artifactAccessScopeFromContext(ctx context.Context) (*ArtifactAccessScope, bool) {
	scope, ok := ctx.Value(artifactAccessScopeKey).(*ArtifactAccessScope)
	return scope, ok && scope != nil
}

// openArtifactRef is the only bridge from a model-visible opaque artifact ID
// to stored bytes. It deliberately resolves IDs, not paths: an invalid or
// mistyped reference fails here before a filesystem tool or path-consent layer
// can interpret model text as a new path.
func (c *Coordinator) openArtifactRef(ctx context.Context, id string) (io.ReadCloser, error) {
	id = strings.TrimSpace(id)
	if c == nil || c.session == nil || id == "" {
		return nil, fmt.Errorf("artifact reference is required")
	}
	ref, producerTaskID, ok := c.authorizedArtifactRef(ctx, id)
	if !ok {
		return nil, fmt.Errorf("artifact reference %q is unknown or not authorized for this task", id)
	}
	if ref.ID != id || strings.TrimSpace(ref.SHA256) == "" {
		return nil, fmt.Errorf("artifact reference %q is not immutable", id)
	}
	if ref.TaskID != "" && producerTaskID != "" && ref.TaskID != producerTaskID {
		return nil, fmt.Errorf("artifact reference %q has mismatched producer ownership", id)
	}

	store, err := NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	if err != nil {
		return nil, fmt.Errorf("open artifact store: %w", err)
	}
	if err := store.Verify(ctx, ref); err != nil {
		return nil, fmt.Errorf("artifact reference %q failed integrity verification: %w", id, err)
	}
	reader, err := store.Open(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("open artifact reference %q: %w", id, err)
	}
	return reader, nil
}

func (c *Coordinator) authorizedArtifactRef(ctx context.Context, id string) (ArtifactRef, string, bool) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return ArtifactRef{}, "", false
	}
	if scope, scoped := artifactAccessScopeFromContext(ctx); scoped {
		if scope.RunID != "" && c.executionRunID != "" && scope.RunID != c.executionRunID {
			return ArtifactRef{}, "", false
		}
		if todoID, _ := ctx.Value(todoIDKey{}).(string); todoID != "" && scope.TaskID != todoID {
			return ArtifactRef{}, "", false
		}
		if attempt, _ := ctx.Value(executionAttemptKey{}).(int); attempt > 0 && scope.Attempt != attempt {
			return ArtifactRef{}, "", false
		}
		for _, ref := range scope.AuthorizedRefs {
			if ref.ID == id {
				producerTaskID := ref.TaskID
				return ref, producerTaskID, true
			}
		}
		return ArtifactRef{}, "", false
	}
	consumerTaskID, _ := ctx.Value(todoIDKey{}).(string)
	allowedProducers := make(map[string]bool)
	if consumerTaskID != "" {
		allowedProducers[consumerTaskID] = true
		consumer := c.todoItemByID(consumerTaskID)
		if consumer == nil {
			return ArtifactRef{}, "", false
		}
		if consumer.WorksetBinding != nil {
			return c.authorizedWorksetInput(consumer, id)
		}
		// Structured fan-out children receive exact opaque references in their
		// immutable workset binding only when the runtime receipt commits this
		// exact child. A binding by itself is not authorization evidence.
		if input, producerID, ok := c.authorizedWorksetInput(consumer, id); ok {
			return input, producerID, true
		}
		for _, dependency := range consumer.DependsOn {
			allowedProducers[dependency] = true
		}
	}

	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil || item.Status != TaskDone {
			continue
		}
		if consumerTaskID != "" && !allowedProducers[item.ID] {
			continue
		}
		result := item.TypedResult
		if result == nil || !taskResultStatusIsSuccessful(result.Status) {
			continue
		}
		if ref, ok := artifactRefByID(result, id); ok {
			if err := c.validateCurrentProducerArtifactOccurrence(ref, item.ID, result); err != nil {
				continue
			}
			return ref, item.ID, true
		}
	}
	return ArtifactRef{}, "", false
}

// buildArtifactAccessScope snapshots the task's artifact capabilities at
// dispatch. Bound children receive only receipt-authorized inputs. Unbound
// dependents retain the historical dependency-result capability set.
func (c *Coordinator) buildArtifactAccessScope(todoID string, attempt int) (*ArtifactAccessScope, error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil, fmt.Errorf("artifact scope requires task state")
	}
	item := c.todoItemByID(todoID)
	if item == nil {
		return nil, fmt.Errorf("artifact scope task %q does not exist", todoID)
	}
	if attempt <= 0 {
		return nil, fmt.Errorf("artifact scope attempt must be positive")
	}
	runID := strings.TrimSpace(c.executionRunID)
	if runID == "" {
		runID = strings.TrimSpace(c.taskTracker.TodoList().RunID())
	}
	scope := &ArtifactAccessScope{RunID: runID, TaskID: todoID, Attempt: attempt}
	addUnique := func(target *[]ArtifactRef, ref ArtifactRef) {
		if strings.TrimSpace(ref.ID) == "" {
			return
		}
		for _, existing := range *target {
			if existing.ID == ref.ID {
				return
			}
		}
		*target = append(*target, ref)
	}

	if item.WorksetBinding != nil {
		binding := item.WorksetBinding
		for _, input := range binding.Inputs {
			ref, _, ok := c.authorizedWorksetInput(item, input.ID)
			if !ok {
				return nil, fmt.Errorf("artifact scope input %q is not authorized by the committed workset receipt", input.ID)
			}
			addUnique(&scope.AuthorizedRefs, ref)
		}
	}

	for _, dependencyID := range item.DependsOn {
		dependency := c.todoItemByID(dependencyID)
		if dependency == nil || dependency.Status != TaskDone || dependency.TypedResult == nil || !taskResultStatusIsSuccessful(dependency.TypedResult.Status) {
			continue
		}
		for _, ref := range artifactRefsFromTaskResult(dependency.TypedResult) {
			if item.WorksetBinding == nil {
				if err := c.validateCurrentProducerArtifactOccurrence(ref, dependency.ID, dependency.TypedResult); err != nil {
					return nil, fmt.Errorf("artifact scope dependency %q ref %q has invalid current producer occurrence: %w", dependency.ID, ref.ID, err)
				}
			}
			addUnique(&scope.DeniedRefs, ref)
			if item.WorksetBinding == nil {
				addUnique(&scope.AuthorizedRefs, ref)
			}
		}
	}
	for _, ref := range scope.AuthorizedRefs {
		addUnique(&scope.DeniedRefs, ref)
	}
	if item.WorksetBinding == nil && len(scope.AuthorizedRefs) == 0 {
		return nil, nil
	}
	return scope, nil
}

func projectDependencyResultForWorker(result *TaskResult, binding *WorksetBinding) TaskResult {
	if result == nil {
		return TaskResult{}
	}
	projected := *cloneTaskResult(result)
	if binding == nil {
		return projected
	}
	authorized := make(map[string]bool, len(binding.Inputs))
	for _, input := range binding.Inputs {
		if input.ID != "" {
			authorized[input.ID] = true
		}
	}
	filtered := projected.Artifacts[:0]
	for _, ref := range projected.Artifacts {
		if authorized[ref.ID] {
			ref.Path = ""
			filtered = append(filtered, ref)
		}
	}
	projected.Artifacts = filtered
	if projected.RawOutputRef != nil {
		ref := *projected.RawOutputRef
		if authorized[ref.ID] {
			ref.Path = ""
			projected.RawOutputRef = &ref
		} else {
			projected.RawOutputRef = nil
		}
	}
	if len(projected.Outputs) > 0 {
		projected.Outputs = make(map[string]StructuredOutputValue, len(result.Outputs))
		for name, output := range result.Outputs {
			if output.Artifact == nil || !authorized[output.Artifact.ID] {
				continue
			}
			ref := *output.Artifact
			ref.Path = ""
			output.Artifact = &ref
			projected.Outputs[name] = output
		}
	}
	return projected
}

func artifactRefsFromTaskResult(result *TaskResult) []ArtifactRef {
	if result == nil {
		return nil
	}
	refs := append([]ArtifactRef(nil), result.Artifacts...)
	if result.RawOutputRef != nil {
		refs = append(refs, *result.RawOutputRef)
	}
	for _, output := range result.Outputs {
		if output.Artifact != nil {
			refs = append(refs, *output.Artifact)
		}
	}
	return refs
}

// artifactScopePathCandidates returns both runtime backing paths and the
// source locations recorded by an artifact. The tools package canonicalizes
// these candidates before matching, so symlink aliases cannot bypass policy.
func (c *Coordinator) artifactScopePathCandidates(scope *ArtifactAccessScope) []string {
	if c == nil || c.session == nil || scope == nil {
		return nil
	}
	paths := make([]string, 0, len(scope.DeniedRefs)*4+2)
	seen := make(map[string]bool)
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	artifactRoot := filepath.Join(c.session.Workspace, logsDir, "artifacts")
	add(filepath.Join(artifactRoot, "data"))
	add(filepath.Join(artifactRoot, "meta"))
	for _, ref := range scope.DeniedRefs {
		if strings.TrimSpace(ref.ID) != "" {
			add(filepath.Join(artifactRoot, "data", ref.ID))
			add(filepath.Join(artifactRoot, "meta", ref.ID+".json"))
		}
		if strings.TrimSpace(ref.Path) == "" {
			continue
		}
		if filepath.IsAbs(ref.Path) {
			add(filepath.Clean(ref.Path))
			continue
		}
		add(filepath.Join(c.session.Workspace, ref.Path))
		if c.projectDir != "" && filepath.Clean(c.projectDir) != filepath.Clean(c.session.Workspace) {
			add(filepath.Join(c.projectDir, ref.Path))
		}
	}
	return paths
}

func (c *Coordinator) authorizedWorksetInput(consumer *TodoItem, id string) (ArtifactRef, string, bool) {
	if c == nil || consumer == nil || consumer.WorksetBinding == nil || id == "" {
		return ArtifactRef{}, "", false
	}
	binding := consumer.WorksetBinding
	visible := make([]*WorksetExpansionReceipt, 0)
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.WorksetReceipt != nil {
			visible = append(visible, item.WorksetReceipt)
		}
	}
	receipts, conflicts := collectWorksetReceipts(visible)
	if _, conflicted := conflicts[binding.WorksetID]; conflicted {
		return ArtifactRef{}, "", false
	}
	receipt := receipts[binding.WorksetID]
	if receipt == nil || receipt.Children[binding.ItemKey] != consumer.ID {
		return ArtifactRef{}, "", false
	}
	if receipt.ParentTaskID != binding.ParentTaskID ||
		receipt.SourceArtifactID != binding.SourceArtifactID ||
		receipt.SourceSHA256 != binding.SourceSHA256 {
		return ArtifactRef{}, "", false
	}
	for _, input := range binding.Inputs {
		if input.ID != id || strings.TrimSpace(input.SHA256) == "" ||
			strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.TaskID) == "" || input.Attempt <= 0 || strings.TrimSpace(input.Agent) == "" {
			continue
		}
		producer := c.todoItemByID(input.TaskID)
		if producer == nil || producer.Status != TaskDone || producer.TypedResult == nil ||
			!taskResultStatusIsSuccessful(producer.TypedResult.Status) {
			return ArtifactRef{}, "", false
		}
		declared, ok := artifactRefByID(producer.TypedResult, id)
		if !ok || !sameArtifactOccurrence(input, declared) {
			return ArtifactRef{}, "", false
		}
		if err := c.validateCurrentProducerArtifactOccurrence(input, input.TaskID, producer.TypedResult); err != nil {
			return ArtifactRef{}, "", false
		}
		return input, input.TaskID, true
	}
	return ArtifactRef{}, "", false
}

func sameArtifactOccurrence(first, second ArtifactRef) bool {
	return first.ID == second.ID && first.SHA256 == second.SHA256 &&
		first.Bytes == second.Bytes && first.ByteSize == second.ByteSize &&
		first.RunID == second.RunID && first.TaskID == second.TaskID &&
		first.Attempt == second.Attempt && first.Agent == second.Agent
}

func artifactRefByID(result *TaskResult, id string) (ArtifactRef, bool) {
	if result == nil || strings.TrimSpace(id) == "" {
		return ArtifactRef{}, false
	}
	ref, err := resolveTaskResultArtifact(result, id)
	return ref, err == nil
}
