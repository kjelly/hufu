package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"path/filepath"
	"slices"

	"github.com/kjelly/hufu/internal/agent"
)

type TaskExecutionEnvelope struct {
	OccurrenceDigest     string
	Tools                ResolvedWorkerTools
	ResourceScope        EffectiveTaskResourceScope
	LogicalToolsetDigest string
	ResourceScopeDigest  string
}

type taskExecutionEnvelopeContextKey struct{}

func withTaskExecutionEnvelope(ctx context.Context, envelope TaskExecutionEnvelope) context.Context {
	return context.WithValue(ctx, taskExecutionEnvelopeContextKey{}, cloneTaskExecutionEnvelope(envelope))
}

func taskExecutionEnvelopeFromContext(ctx context.Context) (TaskExecutionEnvelope, bool) {
	if ctx == nil {
		return TaskExecutionEnvelope{}, false
	}
	envelope, ok := ctx.Value(taskExecutionEnvelopeContextKey{}).(TaskExecutionEnvelope)
	return cloneTaskExecutionEnvelope(envelope), ok
}

func newSHA256Hasher() hash.Hash { return sha256.New() }

func encodeHash(hasher hash.Hash) string { return hex.EncodeToString(hasher.Sum(nil)) }

func cloneTaskExecutionEnvelope(src TaskExecutionEnvelope) TaskExecutionEnvelope {
	cloned := src
	cloned.Tools = cloneResolvedWorkerTools(src.Tools)
	cloned.ResourceScope.Claims = slices.Clone(src.ResourceScope.Claims)
	cloned.ResourceScope.AllowedReadPaths = slices.Clone(src.ResourceScope.AllowedReadPaths)
	cloned.ResourceScope.AllowedWritePaths = slices.Clone(src.ResourceScope.AllowedWritePaths)
	return cloned
}

func (c *Coordinator) taskMutableRoot() (string, error) {
	if c == nil {
		return "", fmt.Errorf("mutable root is unavailable")
	}
	root := c.projectDir
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		root = c.phaseWorkflow.executionContext().RuntimeWorkspace.Root
	}
	if root == "" && c.session != nil {
		root = c.session.Workspace
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func (c *Coordinator) resolveTaskExecutionEnvelope(ctx context.Context, task TaskDef, todo *TodoItem) (TaskExecutionEnvelope, error) {
	if todo == nil {
		return TaskExecutionEnvelope{}, fmt.Errorf("resolve task execution envelope: Todo is required")
	}
	if err := validateDynamicToolAuthorizationSnapshot(todo.DynamicToolAuthorization); err != nil {
		return TaskExecutionEnvelope{}, err
	}
	if err := validateTaskResourceScopeSnapshot(todo.ResourceScopeSnapshot, todo.SideEffect); err != nil {
		return TaskExecutionEnvelope{}, err
	}
	var tools ResolvedWorkerTools
	if task.Action != nil || len(task.Execution.Steps) > 0 {
		hasher := newSHA256Hasher()
		writeDigestRecord(hasher, "structured_task_tools", "1", task.Agent, task.Goal)
		frozenDigest := "legacy"
		if todo.DynamicToolAuthorization != nil {
			frozenDigest = todo.DynamicToolAuthorization.FrozenCatalogDigest
		}
		writeDigestRecord(hasher, "frozen_catalog_digest", frozenDigest)
		tools.LogicalToolsetDigest = encodeHash(hasher)
		tools.ProviderSurfaceDigest = tools.LogicalToolsetDigest
	} else {
		def, _, err := c.AgentPool().ResolveAgentName(todo.Agent)
		if err != nil {
			return TaskExecutionEnvelope{}, err
		}
		tools, err = c.ToolResolver().ResolveTaskTools(ctx, def, WorkerToolResolutionRequest{
			Task: task, TodoID: todo.ID, Mode: workerToolResolutionModeForTask(task), ProspectiveTodo: todo,
		})
		if err != nil {
			return TaskExecutionEnvelope{}, err
		}
	}
	root, err := c.taskMutableRoot()
	if err != nil {
		return TaskExecutionEnvelope{}, fmt.Errorf("resource_scope_unreproducible: %w", err)
	}
	scope, err := c.bindFrozenTaskResourceScope(task, todo, tools, root)
	if err != nil {
		return TaskExecutionEnvelope{}, err
	}
	projection, err := newTaskOccurrenceProjection(todo)
	if err != nil {
		return TaskExecutionEnvelope{}, err
	}
	digest, err := decisionOccurrenceInputDigest(projection)
	if err != nil {
		return TaskExecutionEnvelope{}, err
	}
	resourceDigest := "legacy"
	if todo.ResourceScopeSnapshot != nil {
		resourceDigest = todo.ResourceScopeSnapshot.Digest
	}
	return TaskExecutionEnvelope{
		OccurrenceDigest: digest, Tools: cloneResolvedWorkerTools(tools), ResourceScope: scope,
		LogicalToolsetDigest: tools.LogicalToolsetDigest, ResourceScopeDigest: resourceDigest,
	}, nil
}

func (c *Coordinator) blockTaskExecutionEnvelopePreflight(task TaskDef, todo *TodoItem, err error) error {
	if err == nil || c == nil || todo == nil {
		return err
	}
	detail := c.FailureDetail(err, FailureSourceError)
	c.PersistFailureWithClassAndStatus(todo.Agent, task.Goal, todo.ID, detail, NeedsHuman, FailurePolicy, TaskBlocked)
	c.report(c.newEvent("needs_human").withAgent(todo.Agent).withTodoID(todo.ID).withMessage(detail))
	return err
}

func (c *Coordinator) prepareTaskExecutionEnvelope(ctx context.Context, task TaskDef, todoID string, leafExecution bool) (TaskExecutionEnvelope, error) {
	item := c.todoItemByID(todoID)
	if item == nil {
		return TaskExecutionEnvelope{}, fmt.Errorf("resource_scope_unreproducible: Todo %q does not exist", todoID)
	}
	envelope, hasEnvelope := taskExecutionEnvelopeFromContext(ctx)
	if leafExecution || !hasEnvelope || item.ResourceScopeSnapshot == nil {
		var err error
		envelope, err = c.resolveTaskExecutionEnvelope(ctx, task, item)
		if err != nil {
			return TaskExecutionEnvelope{}, c.blockTaskExecutionEnvelopePreflight(task, item, err)
		}
	} else if err := validateTaskExecutionEnvelopeBinding(item, envelope); err != nil {
		return TaskExecutionEnvelope{}, c.blockTaskExecutionEnvelopePreflight(task, item, err)
	}
	if !resourceClaimsReported(ctx) {
		if err := c.recordResourceClaimsResolved(ctx, item); err != nil {
			return TaskExecutionEnvelope{}, c.blockTaskExecutionEnvelopePreflight(task, item, err)
		}
	}
	return envelope, nil
}

func (c *Coordinator) preflightTaskExecutionRetry(ctx context.Context, task TaskDef, todoID string) error {
	item := c.todoItemByID(todoID)
	if item == nil {
		return fmt.Errorf("resource_scope_unreproducible: Todo %q disappeared before retry", todoID)
	}
	if _, err := c.resolveTaskExecutionEnvelope(ctx, task, item); err != nil {
		return c.blockTaskExecutionEnvelopePreflight(task, item, err)
	}
	return nil
}

func (c *Coordinator) recordTaskExecutionRetryScope(ctx context.Context, task TaskDef, todoID string) error {
	item := c.todoItemByID(todoID)
	if item == nil {
		return fmt.Errorf("resource_scope_unreproducible: Todo %q disappeared after retry transition", todoID)
	}
	if err := c.recordResourceClaimsResolved(ctx, item); err != nil {
		return c.blockTaskExecutionEnvelopePreflight(task, item, err)
	}
	return nil
}

func (c *Coordinator) resolveAttemptWorkerTools(ctx context.Context, task TaskDef, todoID string, agentDef *agent.AgentDef) (ResolvedWorkerTools, error) {
	resolved, err := c.ToolResolver().ResolveTaskTools(ctx, agentDef, WorkerToolResolutionRequest{
		Task: task, TodoID: todoID, Mode: workerToolResolutionModeForTask(task),
	})
	if err == nil {
		return resolved, nil
	}
	item := c.todoItemByID(todoID)
	return ResolvedWorkerTools{}, c.blockTaskExecutionEnvelopePreflight(task, item, fmt.Errorf("resolve worker tools: %w", err))
}

func validateTaskExecutionEnvelopes(tasks []TaskDef, items []*TodoItem, envelopes []TaskExecutionEnvelope) error {
	if len(tasks) != len(items) || len(tasks) != len(envelopes) {
		return fmt.Errorf("scheduler task/Todo/envelope count mismatch: %d/%d/%d", len(tasks), len(items), len(envelopes))
	}
	seen := make(map[string]bool, len(items))
	indexByID := make(map[string]int, len(items))
	for i, item := range items {
		if item != nil && item.ID != "" {
			indexByID[item.ID] = i
		}
	}
	for i := range tasks {
		if items[i] == nil || items[i].ID == "" {
			return fmt.Errorf("scheduler envelope %d has no Todo occurrence", i)
		}
		if seen[items[i].ID] {
			return fmt.Errorf("scheduler envelope has duplicate Todo %q", items[i].ID)
		}
		seen[items[i].ID] = true
		if err := compareTaskDefWithTodoOccurrence(tasks[i], items[i], indexByID); err != nil {
			return err
		}
		if envelopes[i].OccurrenceDigest == "" || envelopes[i].LogicalToolsetDigest == "" || envelopes[i].ResourceScopeDigest == "" {
			return fmt.Errorf("scheduler envelope %d is incomplete", i)
		}
		if items[i].ResourceScopeSnapshot == nil || envelopes[i].ResourceScopeDigest != items[i].ResourceScopeSnapshot.Digest {
			return fmt.Errorf("scheduler envelope %d resource digest mismatch", i)
		}
		projection, err := newTaskOccurrenceProjection(items[i])
		if err != nil {
			return err
		}
		digest, err := decisionOccurrenceInputDigest(projection)
		if err != nil || digest != envelopes[i].OccurrenceDigest {
			return fmt.Errorf("scheduler envelope %d occurrence digest mismatch", i)
		}
	}
	return nil
}

func validateTaskExecutionEnvelopeBinding(item *TodoItem, envelope TaskExecutionEnvelope) error {
	if item == nil || item.ID == "" {
		return fmt.Errorf("task execution envelope has no Todo occurrence")
	}
	if item.ResourceScopeSnapshot == nil || envelope.ResourceScopeDigest != item.ResourceScopeSnapshot.Digest {
		return fmt.Errorf("task %s resource scope digest mismatch", item.ID)
	}
	projection, err := newTaskOccurrenceProjection(item)
	if err != nil {
		return err
	}
	digest, err := decisionOccurrenceInputDigest(projection)
	if err != nil || digest != envelope.OccurrenceDigest {
		return fmt.Errorf("task %s occurrence digest mismatch", item.ID)
	}
	if envelope.LogicalToolsetDigest == "" {
		return fmt.Errorf("task %s logical toolset digest is empty", item.ID)
	}
	return nil
}

func (c *Coordinator) prepareNewTaskExecutionEnvelopes(ctx context.Context, tasks []TaskDef, specs []TodoSpec, ids []string, freezeScopes bool) ([]TaskExecutionEnvelope, error) {
	if len(tasks) != len(specs) || len(tasks) != len(ids) {
		return nil, fmt.Errorf("task envelope input count mismatch: %d/%d/%d", len(tasks), len(specs), len(ids))
	}
	envelopes := make([]TaskExecutionEnvelope, len(tasks))
	prospectiveItems := make([]*TodoItem, len(tasks))
	for i := range tasks {
		prospective := todoItemFromSpec(specs[i], ids[i])
		var tools ResolvedWorkerTools
		if tasks[i].Action == nil && len(tasks[i].Execution.Steps) == 0 {
			def, _, err := c.AgentPool().ResolveAgentName(prospective.Agent)
			if err != nil {
				return nil, err
			}
			tools, err = c.ToolResolver().ResolveTaskTools(ctx, def, WorkerToolResolutionRequest{
				Task: tasks[i], TodoID: ids[i], Mode: workerToolResolutionModeForTask(tasks[i]), ProspectiveTodo: prospective,
			})
			if err != nil {
				return nil, err
			}
		}
		if freezeScopes {
			snapshot, err := c.resolveNewTaskResourceScope(tasks[i], prospective, tools)
			if err != nil {
				return nil, err
			}
			specs[i].ResourceScopeSnapshot = snapshot
			prospective = todoItemFromSpec(specs[i], ids[i])
		}
		envelope, err := c.resolveTaskExecutionEnvelope(ctx, tasks[i], prospective)
		if err != nil {
			return nil, err
		}
		envelopes[i] = envelope
		prospectiveItems[i] = prospective
	}
	if err := validateTaskExecutionEnvelopes(tasks, prospectiveItems, envelopes); err != nil {
		return nil, err
	}
	return envelopes, nil
}
