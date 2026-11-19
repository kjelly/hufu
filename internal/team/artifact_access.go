package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/media"
)

// ArtifactAccessScope is the immutable, attempt-local capability set for
// artifact_ref access. AuthorizedRefs are the only references the artifact
// opener may resolve for this attempt; DeniedRefs are used to block their
// backing/source paths from path-capable tools. The scope is coordinator-owned
// and is also committed on the task's execution receipt.
type ArtifactAccessScope struct {
	RunID   string `json:"run_id"`
	TaskID  string `json:"task_id"`
	Attempt int    `json:"attempt"`
	// StoreRoot identifies the Hufu-owned CAS for this runtime. It is a
	// process-local path used while canonicalizing an external result; the
	// authorization decision still comes from the exact refs below.
	StoreRoot      string        `json:"store_root,omitempty"`
	AuthorizedRefs []ArtifactRef `json:"authorized_refs,omitempty"`
	DeniedRefs     []ArtifactRef `json:"denied_refs,omitempty"`
	// ManagedSkillRefs are immutable snapshots of skill instructions that were
	// disclosed to this attempt. They are deliberately separate from task
	// output refs: a provider may claim that it read a selected skill, but it
	// cannot turn an arbitrary absolute path into evidence.
	ManagedSkillRefs []ArtifactRef `json:"managed_skill_refs,omitempty"`
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
	copyScope.ManagedSkillRefs = append([]ArtifactRef(nil), scope.ManagedSkillRefs...)
	return &copyScope
}

func artifactAccessScopeFromContext(ctx context.Context) (*ArtifactAccessScope, bool) {
	scope, ok := ctx.Value(artifactAccessScopeKey).(*ArtifactAccessScope)
	return scope, ok && scope != nil
}

func (c *Coordinator) artifactStoreRootPath() string {
	if c == nil {
		return ""
	}
	if root := strings.TrimSpace(c.artifactStoreRoot); root != "" {
		return root
	}
	if c.session != nil {
		return strings.TrimSpace(c.session.Workspace)
	}
	return ""
}

// managedSkillArtifactRefs snapshots the exact skill instructions disclosed
// to an attempt into the Hufu CAS. The provider-facing path remains only a
// lookup key; the canonical result records the opaque CAS ID after the
// snapshot is verified. This lets a skill live outside the execution-world
// root without granting the provider an arbitrary absolute-path capability.
func (c *Coordinator) managedSkillArtifactRefs(agentDef *agent.AgentDef, goal string) ([]ArtifactRef, error) {
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" || agentDef == nil {
		return nil, nil
	}
	definitions, err := c.selectedSkillDefinitions(agentDef, goal)
	if err != nil {
		return nil, err
	}
	storeRoot := c.artifactStoreRootPath()
	store, err := NewFileArtifactStore(storeRoot, c.projectDir)
	if err != nil {
		return nil, err
	}
	refs := make([]ArtifactRef, 0, len(definitions))
	for _, definition := range definitions {
		if definition == nil {
			continue
		}
		content := []byte(definition.Content)
		if len(content) == 0 && strings.TrimSpace(definition.Path) != "" {
			content, err = os.ReadFile(definition.Path)
			if err != nil {
				return nil, fmt.Errorf("read skill %q: %w", definition.Name, err)
			}
		}
		if len(content) == 0 {
			return nil, fmt.Errorf("skill %q has no immutable content", definition.Name)
		}
		path := strings.TrimSpace(definition.Path)
		if path == "" {
			return nil, fmt.Errorf("skill %q has no source path", definition.Name)
		}
		if !filepath.IsAbs(path) {
			base := c.projectDir
			if base == "" {
				base = c.session.Workspace
			}
			path = filepath.Join(base, path)
		}
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve skill %q path: %w", definition.Name, err)
		}
		digestBytes := sha256.Sum256(content)
		digest := hex.EncodeToString(digestBytes[:])
		pathDigest := sha256.Sum256([]byte(path))
		id := "skill-" + digest + "-" + hex.EncodeToString(pathDigest[:8])

		if existing, getErr := store.Get(context.Background(), id); getErr == nil {
			if existing.Kind != "skill" || existing.Role != "instruction" || existing.Path != path || existing.SHA256 != digest || existing.ByteSize != int64(len(content)) {
				return nil, fmt.Errorf("skill artifact %q has conflicting immutable metadata", id)
			}
			if err := store.Verify(context.Background(), existing); err != nil {
				return nil, fmt.Errorf("verify skill artifact %q: %w", id, err)
			}
			refs = append(refs, existing)
			continue
		}
		stored, putErr := store.Put(context.Background(), PutArtifactRequest{
			ID:          id,
			Kind:        "skill",
			Role:        "instruction",
			Path:        path,
			Description: definition.Name,
			Content:     content,
		})
		if putErr != nil {
			return nil, fmt.Errorf("persist skill %q: %w", definition.Name, putErr)
		}
		refs = append(refs, stored.ArtifactRef)
	}
	return refs, nil
}

// openArtifactRef is the only bridge from a model-visible opaque artifact ID
// to stored bytes. It deliberately resolves IDs, not paths: an invalid or
// mistyped reference fails here before a filesystem tool or path-consent layer
// can interpret model text as a new path.
func (c *Coordinator) openArtifactRef(ctx context.Context, id string) (io.ReadCloser, error) {
	id = strings.TrimSpace(id)
	if isMediaArtifactID(id) {
		return nil, media.Reject(media.Unsupported, "media requires an admitted native image input")
	}
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

	storeRoot := c.artifactStoreRootPath()
	if scope, scoped := artifactAccessScopeFromContext(ctx); scoped && strings.TrimSpace(scope.StoreRoot) != "" {
		storeRoot = scope.StoreRoot
	}
	// Store errors wrap *os.PathError values that name absolute paths. This
	// error reaches the model through view, so report a fixed reason only.
	store, err := NewFileArtifactStore(storeRoot, c.projectDir)
	if err != nil {
		return nil, fmt.Errorf("artifact reference %q is unavailable", id)
	}
	if err := store.Verify(ctx, ref); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("artifact reference %q is unavailable", id)
		}
		return nil, fmt.Errorf("artifact reference %q failed integrity verification", id)
	}
	reader, err := store.Open(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("artifact reference %q could not be opened", id)
	}
	seeker, ok := reader.(io.ReadSeekCloser)
	if !ok {
		_ = reader.Close()
		return nil, fmt.Errorf("artifact reference %q does not support immutable reads", id)
	}
	return &verifiedArtifactReader{ReadSeekCloser: seeker, sha256: ref.SHA256, byteSize: ref.ByteSize}, nil
}

// Identity is exposed only after the store has verified the authorized version.
type verifiedArtifactReader struct {
	io.ReadSeekCloser
	sha256   string
	byteSize int64
}

func (r *verifiedArtifactReader) ArtifactReadIdentity() (string, int64) {
	return r.sha256, r.byteSize
}

func (c *Coordinator) authorizedArtifactRef(ctx context.Context, id string) (ArtifactRef, string, bool) {
	if service := contextArtifactServiceFromContext(ctx); service != nil {
		// A context artifact is readable only through the service that
		// published it; the frozen ArtifactAccessScope is never widened.
		if ref, ok := service.authorizedRef(c, ctx, id); ok {
			return ref, service.taskID, true
		}
	}
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
		for _, ref := range scope.ManagedSkillRefs {
			if ref.ID == id {
				return ref, "", true
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
		for _, dependency := range resultDependencyIDs(consumer) {
			allowedProducers[dependency] = true
		}
		for _, source := range consumer.EvidenceFrom {
			for _, input := range c.dependencyReviewedInputs(c.todoItemByID(source)) {
				if input.ID == id {
					return input, input.TaskID, true
				}
			}
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
//
// The optional goal is the exact prompt-selection goal used by the caller.
// Durable projections remain the fallback for resume/declared-step paths, but
// a live worker must bind managed skills from the same goal that selected the
// skills in its prompt.
func (c *Coordinator) buildArtifactAccessScope(ctx context.Context, todoID string, attempt int, goals ...string) (*ArtifactAccessScope, error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil, fmt.Errorf("artifact scope requires task state")
	}
	authorizer := c.newArtifactInputAuthorizer(ctx, c.taskTracker.TodoList().Items())
	item := authorizer.tasks[todoID]
	if item == nil {
		return nil, fmt.Errorf("artifact scope task %q does not exist", todoID)
	}
	if attempt <= 0 {
		return nil, fmt.Errorf("artifact scope attempt must be positive")
	}
	progress := c.beginRuntimeValidation("artifact_scope", todoID)
	defer progress.close()
	runID := strings.TrimSpace(c.executionRunID)
	if runID == "" {
		runID = strings.TrimSpace(c.taskTracker.TodoList().RunID())
	}
	scope := &ArtifactAccessScope{RunID: runID, TaskID: todoID, Attempt: attempt}
	scope.StoreRoot = c.artifactStoreRootPath()
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
			ref, _, err := authorizer.resolve(item, input.ID)
			if err != nil {
				return nil, fmt.Errorf("artifact scope input %q is not authorized by the committed workset receipt: %w", input.ID, err)
			}
			addUnique(&scope.AuthorizedRefs, ref)
		}
		progress.checked("assigned workset inputs")
	}

	for _, dependencyID := range resultDependencyIDs(item) {
		dependency := authorizer.tasks[dependencyID]
		if dependency == nil || dependency.Status != TaskDone || dependency.TypedResult == nil || !taskResultStatusIsSuccessful(dependency.TypedResult.Status) {
			continue
		}
		for _, ref := range artifactRefsFromTaskResult(dependency.TypedResult) {
			if item.WorksetBinding == nil {
				if err := authorizer.validateProducer(ref, dependency); err != nil {
					return nil, fmt.Errorf("artifact scope dependency %q ref %q has invalid current producer occurrence: %w", dependency.ID, ref.ID, err)
				}
			}
			addUnique(&scope.DeniedRefs, ref)
			if item.WorksetBinding == nil {
				addUnique(&scope.AuthorizedRefs, ref)
			}
		}
		if item.WorksetBinding == nil && slices.Contains(item.EvidenceFrom, dependency.ID) {
			for _, ref := range authorizer.reviewedInputs(dependency) {
				addUnique(&scope.DeniedRefs, ref)
				addUnique(&scope.AuthorizedRefs, ref)
			}
		}
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		progress.checked("dependency " + dependencyID)
	}
	for _, ref := range scope.AuthorizedRefs {
		addUnique(&scope.DeniedRefs, ref)
	}
	if c.session != nil {
		if agentDef, _, resolveErr := c.AgentPool().ResolveAgentName(item.Agent); resolveErr == nil && agentDef != nil {
			goal := taskDefFromTodoItem(item).Goal
			if len(goals) > 0 && strings.TrimSpace(goals[0]) != "" {
				goal = goals[0]
			}
			managed, skillErr := c.managedSkillArtifactRefs(agentDef, goal)
			if skillErr != nil {
				return nil, fmt.Errorf("bind managed skill scope: %w", skillErr)
			}
			scope.ManagedSkillRefs = managed
		}
	}
	// Every worker attempt receives a scope, including unbound workers. An
	// empty authorization set is meaningful: the policy blocks the opaque
	// artifact data/meta roots so cwd-based ls/view cannot enumerate another
	// task's backing store.
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
	paths := make([]string, 0, len(scope.DeniedRefs)*4+3)
	seen := make(map[string]bool)
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path != "" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	storeRoot := strings.TrimSpace(scope.StoreRoot)
	if storeRoot == "" {
		storeRoot = c.artifactStoreRootPath()
	}
	artifactRoot := filepath.Join(storeRoot, logsDir, "artifacts")
	add(filepath.Join(artifactRoot, "data"))
	add(filepath.Join(artifactRoot, "meta"))
	// Generic path tools never hydrate image bytes or read private metadata.
	// Native image consumers resolve their canonical grants through the store.
	add(filepath.Join(artifactRoot, "media"))
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
		add(filepath.Join(storeRoot, ref.Path))
		if c.projectDir != "" && filepath.Clean(c.projectDir) != filepath.Clean(storeRoot) {
			add(filepath.Join(c.projectDir, ref.Path))
		}
	}
	return paths
}

func (c *Coordinator) authorizedWorksetInput(consumer *TodoItem, id string) (ArtifactRef, string, bool) {
	if c == nil || c.taskTracker == nil || consumer == nil || consumer.WorksetBinding == nil {
		return ArtifactRef{}, "", false
	}
	authorizer := c.newArtifactInputAuthorizer(context.Background(), c.taskTracker.TodoList().Items())
	ref, producerID, err := authorizer.resolve(consumer, id)
	return ref, producerID, err == nil
}

// artifactInputAuthorizer is scoped to one synchronous capability projection.
// Task snapshots, canonical lineage and proven origins never cross calls.
type artifactInputAuthorizer struct {
	coordinator *Coordinator
	ctx         context.Context
	tasks       map[string]*TodoItem
	receipts    map[string]*WorksetExpansionReceipt
	conflicts   map[string]struct{}
	replay      func() ([]*TodoItem, error)
	origins     map[artifactProducerOccurrence]struct{}
}

func (c *Coordinator) newArtifactInputAuthorizer(ctx context.Context, items []*TodoItem) *artifactInputAuthorizer {
	tasks := make(map[string]*TodoItem, len(items))
	visible := make([]*WorksetExpansionReceipt, 0)
	for _, item := range items {
		if item == nil {
			continue
		}
		tasks[item.ID] = item
		if item.WorksetReceipt != nil {
			visible = append(visible, item.WorksetReceipt)
		}
	}
	receipts, conflicts := collectWorksetReceipts(visible)
	replay := sync.OnceValues(func() ([]*TodoItem, error) {
		lineage, err := c.dependencyViewLineage(ctx)
		if err != nil {
			return nil, err
		}
		return ReplayTodoList(lineage)
	})
	return &artifactInputAuthorizer{coordinator: c, ctx: ctx, tasks: tasks, receipts: receipts, conflicts: conflicts,
		replay: replay, origins: make(map[artifactProducerOccurrence]struct{})}
}

func (v *artifactInputAuthorizer) validateWorksetReceipt(consumer *TodoItem) error {
	binding := consumer.WorksetBinding
	if _, conflicted := v.conflicts[binding.WorksetID]; conflicted {
		return fmt.Errorf("conflicting workset expansion receipts")
	}
	receipt := v.receipts[binding.WorksetID]
	if receipt == nil || receipt.Children[binding.ItemKey] != consumer.ID {
		return fmt.Errorf("workset receipt does not assign this child")
	}
	if receipt.ParentTaskID != binding.ParentTaskID ||
		receipt.SourceArtifactID != binding.SourceArtifactID ||
		receipt.SourceSHA256 != binding.SourceSHA256 {
		return fmt.Errorf("workset source identity differs from its binding")
	}
	c := v.coordinator
	if receipt.RunID != c.executionRunID || slices.ContainsFunc(binding.Inputs, func(input ArtifactRef) bool { return input.RunID != c.executionRunID }) {
		snapshot, _ := c.terminalRunInputs(c.executionRunID)
		if snapshot == nil || ValidateRunInputSnapshot(snapshot) != nil {
			return fmt.Errorf("recovered workset receipt: workset recovery has no selected frozen inputs")
		}
		items, err := v.replay()
		if err == nil {
			err = validateRecoveredWorksetReplay(snapshot, items, receipt, consumer, func(id string) *TodoItem { return v.tasks[id] })
		}
		if err != nil {
			return fmt.Errorf("recovered workset receipt: %w", err)
		}
	}
	return nil
}

func (v *artifactInputAuthorizer) validateProducer(ref ArtifactRef, producer *TodoItem) error {
	c := v.coordinator
	if err := context.Cause(v.ctx); err != nil {
		return err
	}
	if err := validateProducerArtifactIdentity(ref, producer.ID, producer.TypedResult, c.executionRunID); err != nil {
		return err
	}
	if ref.RunID == c.executionRunID {
		return nil
	}
	origin := artifactProducerOccurrence{ref.RunID, ref.TaskID, ref.Attempt, ref.Agent}
	if _, validated := v.origins[origin]; !validated {
		if err := c.validateRecoveredArtifactOrigin(ref, producer.ID, producer.TypedResult); err != nil {
			return fmt.Errorf("belongs to run %q, want current run %q or an admitted recovery source: %w", ref.RunID, c.executionRunID, err)
		}
		v.origins[origin] = struct{}{}
	}
	return nil
}

// Operator preflight preserves the rejection cause. Model-facing artifact
// lookups retain the boolean boundary and do not disclose storage errors.
func (v *artifactInputAuthorizer) resolve(consumer *TodoItem, id string) (ArtifactRef, string, error) {
	if err := context.Cause(v.ctx); err != nil {
		return ArtifactRef{}, "", err
	}
	if id == "" || consumer == nil || consumer.WorksetBinding == nil {
		return ArtifactRef{}, "", fmt.Errorf("missing workset input binding")
	}
	if err := v.validateWorksetReceipt(consumer); err != nil {
		return ArtifactRef{}, "", err
	}
	binding := consumer.WorksetBinding
	for _, input := range binding.Inputs {
		if input.ID != id || strings.TrimSpace(input.SHA256) == "" ||
			strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.TaskID) == "" || input.Attempt <= 0 || strings.TrimSpace(input.Agent) == "" {
			continue
		}
		producer := v.tasks[input.TaskID]
		if producer == nil || producer.Status != TaskDone || producer.TypedResult == nil ||
			!taskResultStatusIsSuccessful(producer.TypedResult.Status) {
			return ArtifactRef{}, "", fmt.Errorf("input producer has no accepted result")
		}
		declared, err := resolveTaskResultArtifact(producer.TypedResult, id)
		if err != nil {
			return ArtifactRef{}, "", fmt.Errorf("input producer declaration: %w", err)
		}
		if !sameArtifactOccurrence(input, declared) {
			return ArtifactRef{}, "", fmt.Errorf("input occurrence differs from the producer's declared artifact")
		}
		if err := v.validateProducer(input, producer); err != nil {
			return ArtifactRef{}, "", fmt.Errorf("input producer occurrence: %w", err)
		}
		return input, input.TaskID, nil
	}
	return ArtifactRef{}, "", fmt.Errorf("no immutable occurrence for the assigned input")
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
