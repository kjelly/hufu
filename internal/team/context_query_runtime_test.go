package team

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestContextQueryRuntimeStagesSharedAndPrivateWithoutAnotherPrimary(t *testing.T) {
	c, input, ctx := queryRuntimeFixture(t)
	c.contextRepo = frozenQueryRuntimeRepository(t, c.contextRepo, input)
	timings := &queryRuntimeTimings{Repository: c.contextRepo}
	c.contextRepo = timings
	beforeShared := queryQualityIDs(input.CanonicalMemory.SharedPersistent)
	beforePrivate := workerQueryIDs(input.WorkerMemory)
	var work contextstore.QueryWork
	ctx = contextstore.WithQueryWorkObserver(ctx, func(_ context.Context, item contextstore.QueryWork) {
		work.ExactCalls += item.ExactCalls
		work.LexicalCalls += item.LexicalCalls
		work.VectorCalls += item.VectorCalls
		work.CandidateRows += item.CandidateRows
	})
	selected, attribution, err := c.evaluateWorkerContextQueries(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if attribution == nil || attribution.Mode != QueryModeActive {
		t.Fatalf("missing staged active selection: %+v; completed query work=%+v; timings=%v", attribution, work, timings.stages)
	}
	t.Logf("staged selection timings: %v", timings.stages)
	if !slices.Contains(queryQualityIDs(selected.CanonicalMemory.SharedPersistent), "shared-target") || !slices.Contains(workerQueryIDs(selected.WorkerMemory), "session-target") {
		t.Fatalf("scope hints did not repair misses: shared=%v private=%v", queryQualityIDs(selected.CanonicalMemory.SharedPersistent), workerQueryIDs(selected.WorkerMemory))
	}
	if len(selected.WorkerMemory.Items) > input.AgentDef.Memory.MaxItems || selected.WorkerMemory.Trace.Tokens > input.AgentDef.Memory.MaxTokens {
		t.Fatal("private budget increased")
	}
	if work.ExactCalls != 0 || work.LexicalCalls != 3 || work.VectorCalls != 0 || work.CandidateRows != 3 {
		t.Fatalf("repeated primary or unbounded source work: %+v", work)
	}
	if !reflect.DeepEqual(beforeShared, queryQualityIDs(input.CanonicalMemory.SharedPersistent)) || !reflect.DeepEqual(beforePrivate, workerQueryIDs(input.WorkerMemory)) {
		t.Fatal("candidate changed retained legacy bundles")
	}
	compiled, err := (&defaultContextCompiler{c: c}).CompileWorkerContext(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, "worker", contextstore.EvaluationTime(ctx), c.session.Config.MemoryLearning)
	if err != nil || !supportedDeliveryContextManifest(manifest) || manifest.SchemaVersion != 3 {
		t.Fatalf("compiled query manifest invalid: %v", err)
	}
	memory := buildMemoryInjectionManifestFromContextManifest(compiled, &manifest, input.Request.RunID, input.Request.TaskID, 1, "worker", input.Request.RetrievalQuery(), c.session.Config.MemoryLearning)
	if memory == nil || memory.QueryHash != hashQueryAttribution(*attribution) {
		t.Fatal("actual memory exposure hash omitted query identity")
	}
}

func TestContextQueryRuntimePersistentOwnerDeliveryIsRequired(t *testing.T) {
	// Session records have intentional tier precedence. Seed only shared and
	// persistent records to test owner delivery at the unchanged four-item cap.
	c, input, ctx := queryRuntimeFixtureForTiers(t, "shared", "persistent")
	frozen := frozenQueryRuntimeRepository(t, c.contextRepo, input, "shared-target", "persistent-target")
	// Even an overbroad eligibility projection must not authorize a different
	// worker's private record. Keep the original primary snapshots and revision.
	foreign := frozen.lexical[input.WorkerMemory.querySnapshot.sources[1].Request.Scope]
	foreign.ID, foreign.Scope.AgentID = "foreign-private", "another-worker"
	frozen.private = append(frozen.private, foreign)
	activation := frozen.activations["persistent-target"]
	activation.ContextItemID = foreign.ID
	frozen.activations[foreign.ID] = activation
	c.contextRepo = frozen
	sources := c.queryEvaluationSources(input, input.CanonicalMemory.querySnapshot, input.WorkerMemory.querySnapshot)
	if err := c.preparePrivateQueryEligibility(ctx, input.Request, sources); err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if _, ok := source.Eligible[foreign.ID]; ok {
			t.Fatal("private eligibility admitted a foreign worker")
		}
	}
	if _, ok := sources[2].Eligible["persistent-target"]; !ok {
		t.Fatal("owner's persistent record was not eligible")
	}
	compiled, err := (&defaultContextCompiler{c: c}).CompileWorkerContext(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := BuildContextInjectionManifest(input.Request, compiled, nil, "worker", contextstore.EvaluationTime(ctx), c.session.Config.MemoryLearning)
	if err != nil || manifest.Query == nil || querySourceAttribution(*manifest.Query, QueryPrivatePersistent).Mode != QueryModeActive {
		t.Fatalf("persistent owner selection was not active: %v", err)
	}
	if !slices.ContainsFunc(manifest.Items, func(item ContextManifestItem) bool {
		return item.ID == "persistent-target" && item.Included && item.Source == "worker_persistent"
	}) {
		t.Fatalf("owner's persistent record was not delivered in the manifest: %+v", manifest.Items)
	}
	if slices.ContainsFunc(manifest.Items, func(item ContextManifestItem) bool { return item.ID == foreign.ID && item.Included }) {
		t.Fatal("foreign private record reached the compiled manifest")
	}
}

func TestContextQueryRuntimeEligibilityDeadlineRollsBackAllSources(t *testing.T) {
	c, input, ctx := queryRuntimeFixture(t)
	repo := &queryEligibilityDeadline{queryRuntimeFrozenRepository: frozenQueryRuntimeRepository(t, c.contextRepo, input)}
	c.contextRepo = repo
	selected, attribution, err := c.evaluateWorkerContextQueries(ctx, input)
	if err != nil || attribution == nil || !repo.expired || attribution.Mode != QueryModeLegacy || !reflect.DeepEqual(input, selected) {
		t.Fatalf("eligibility deadline did not preserve exact primary bundles: err=%v attribution=%+v", err, attribution)
	}
	if len(attribution.Sources) != 3 {
		t.Fatal("deadline omitted a participating source")
	}
	for _, source := range attribution.Sources {
		if source.Mode != QueryModeLegacy || source.Fallback != QueryFallbackTimeout || source.SupplementalHash != "" {
			t.Fatalf("eligibility timeout published partial or false attribution: %+v", source)
		}
	}
}

type queryEligibilityDeadline struct {
	*queryRuntimeFrozenRepository
	expired bool
}

func (r *queryEligibilityDeadline) IterateAgentActivations(ctx context.Context, _ contextstore.RepositoryQuery, _ func(contextstore.ContextItem, contextstore.ActivationRecord) error) error {
	<-ctx.Done()
	r.expired = true
	return ctx.Err()
}

// SQLite's race/checkptr overhead is not a product latency SLA. A real store
// may legitimately exhaust the fixed child deadline. This test still requires
// either a complete active transaction or exact, attributed legacy rollback;
// the deterministic test above requires the active branch for all three scopes.
func TestContextQueryRuntimeSQLiteHonorsActiveOrCompleteDeadlineRollback(t *testing.T) {
	c, input, ctx := queryRuntimeFixture(t)
	var work contextstore.QueryWork
	ctx = contextstore.WithQueryWorkObserver(ctx, func(_ context.Context, item contextstore.QueryWork) {
		work.ExactCalls += item.ExactCalls
		work.LexicalCalls += item.LexicalCalls
		work.VectorCalls += item.VectorCalls
		work.CandidateRows += item.CandidateRows
	})
	selected, attribution, err := c.evaluateWorkerContextQueries(ctx, input)
	if err != nil || attribution == nil {
		t.Fatalf("real SQLite transaction failed: %v", err)
	}
	if work.ExactCalls != 0 || work.VectorCalls != 0 || work.LexicalCalls > 3 || work.CandidateRows > 3 {
		t.Fatalf("real SQLite repeated primary or exceeded fixture work: %+v", work)
	}
	if attribution.Mode == QueryModeActive {
		for _, kind := range []ContextQuerySourceKind{QuerySharedPersistent, QueryPrivateSession, QueryPrivatePersistent} {
			if source := querySourceAttribution(*attribution, kind); source.Mode != QueryModeActive || source.Fallback != QueryFallbackNone {
				t.Fatalf("partially committed active transaction: %+v", source)
			}
		}
		if !slices.Contains(queryQualityIDs(selected.CanonicalMemory.SharedPersistent), "shared-target") || !slices.Contains(workerQueryIDs(selected.WorkerMemory), "session-target") || work.LexicalCalls != 3 || work.CandidateRows != 3 {
			t.Fatal("real SQLite active selection did not repair misses at the existing budgets")
		}
		return
	}
	if attribution.Mode != QueryModeLegacy || !reflect.DeepEqual(input, selected) {
		t.Fatal("real SQLite deadline published partial selection or changed legacy")
	}
	for _, source := range attribution.Sources {
		if source.Mode != QueryModeLegacy || source.Fallback != QueryFallbackTimeout || source.SupplementalHash != "" {
			t.Fatalf("deadline rollback has false attribution: %+v", source)
		}
	}
}

func TestContextQueryRuntimeRestrictedAncestorPreservesEntirePrivateBundle(t *testing.T) {
	c, input, ctx := queryRuntimeFixture(t)
	input.WorkerMemory.querySnapshot.sources[0].RestrictedLineage = true
	guard := &queryPrivateIterateGuard{Repository: c.contextRepo}
	c.contextRepo = guard
	before := *input.WorkerMemory
	selected, attribution, err := c.evaluateWorkerContextQueries(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if attribution.Mode != QueryModeMixed || !reflect.DeepEqual(before, *selected.WorkerMemory) {
		t.Fatal("restricted private recall changed")
	}
	if guard.privateReads != 0 {
		t.Fatal("restricted ancestry performed new private eligibility reads")
	}
	for _, kind := range []ContextQuerySourceKind{QueryPrivateSession, QueryPrivatePersistent} {
		if source := querySourceAttribution(*attribution, kind); source.Fallback != QueryFallbackRestrictedLineage || source.Mode != QueryModeLegacy {
			t.Fatalf("false private attribution: %+v", source)
		}
	}
}

func TestContextQueryRuntimeEligibilityFailureRetainsAllLegacySources(t *testing.T) {
	c, input, ctx := queryRuntimeFixture(t)
	c.contextRepo = &queryEligibilityFailure{Repository: c.contextRepo}
	selected, attribution, err := c.evaluateWorkerContextQueries(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if attribution.Mode != QueryModeLegacy || !reflect.DeepEqual(input.CanonicalMemory, selected.CanonicalMemory) || !reflect.DeepEqual(input.WorkerMemory, selected.WorkerMemory) {
		t.Fatal("eligibility read failure changed a retained legacy bundle")
	}
	for _, source := range attribution.Sources {
		if source.Mode != QueryModeLegacy || source.Fallback != QueryFallbackSearchFailed {
			t.Fatalf("failed transaction has false attribution: %+v", source)
		}
	}
}

func TestContextQueryRuntimeOuterCancellationStopsBeforeSelection(t *testing.T) {
	c, input, ctx := queryRuntimeFixture(t)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.evaluateWorkerContextQueries(cancelled, input); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled invocation continued with legacy")
	}
}

func TestContextQueryRuntimeCandidateLimitDoesNotReuseLegacyLineageOverfetch(t *testing.T) {
	c, input, _ := queryRuntimeFixture(t)
	private := input.WorkerMemory.querySnapshot
	before := private.sources[0].Request.Limit
	if before != 100000 {
		t.Fatal("legacy lineage fetch control changed")
	}
	sources := c.queryEvaluationSources(input, input.CanonicalMemory.querySnapshot, private)
	for _, source := range sources {
		if source.Request.Limit <= 0 || source.Request.Limit > c.effectiveMemoryRankingPolicy().CandidateTopK {
			t.Fatalf("active candidate merge inherited an overfetch limit: %s=%d", source.Kind, source.Request.Limit)
		}
	}
	if private.sources[0].Request.Limit != before {
		t.Fatal("active cap rewrote the retained legacy request")
	}
}

func queryRuntimeFixture(t *testing.T) (*Coordinator, WorkerContextInput, context.Context) {
	t.Helper()
	return queryRuntimeFixtureForTiers(t, "shared", "session", "persistent")
}

func queryRuntimeFixtureForTiers(t *testing.T, tiers ...string) (*Coordinator, WorkerContextInput, context.Context) {
	t.Helper()
	c, repo := rankingTestCoordinator(t, agent.MemoryLearningObserve)
	c.executionRunID, c.taskTracker = "run-query", NewTaskTracker()
	def := &agent.AgentDef{Name: "worker", Role: "worker", MemoryID: "worker", Memory: agent.DefaultWorkerMemoryPolicy()}
	def.Memory.Mode, def.Memory.MaxItems, def.Memory.MaxTokens = agent.WorkerMemoryPersistent, 4, 500
	c.session.Agents = map[string]*agent.AgentDef{"worker": def}
	c.workerMemorySvc = NewWorkerMemoryService(repo, nil)
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "inspect router"}})[0]
	c.markAdmittedTodoIDs([]*TodoItem{item})
	request := validTestContextRequest()
	request.RunID, request.TaskID, request.AgentName, request.AgentRole = c.executionRunID, item.ID, def.Name, def.Role
	request.Goal, request.Constraints, request.Phase, request.Trigger = "inspect router", "migration sentinel", PhaseExecute, ContextTriggerTaskDispatch
	request.AssignRequestID()
	scope := c.contextScope()
	session := resolveWorkerScope(scope, def, "main")
	persistent := session
	persistent.SessionID, persistent.BranchID = "", ""
	shared := persistentContextScope(scope)
	var records []contextstore.ContextItem
	for index, source := range []struct {
		scope  contextstore.Scope
		prefix string
	}{{shared, "shared"}, {session, "session"}, {persistent, "persistent"}} {
		if !slices.Contains(tiers, source.prefix) {
			continue
		}
		records = append(records, contextstore.ContextItem{ID: source.prefix + "-target", Content: "migration sentinel " + source.prefix, Kind: contextstore.ContextPattern, Scope: source.scope, Lifecycle: contextstore.LifecycleConfirmed, TrustLevel: contextstore.TrustTrusted, Confidence: 1})
		for noise := range 6 {
			records = append(records, contextstore.ContextItem{ID: fmt.Sprintf("noise-%d-%d", index, noise), Content: request.RetrievalQuery() + fmt.Sprintf(" distractor %d-%d", index, noise), Kind: contextstore.ContextPattern, Scope: source.scope, Lifecycle: contextstore.LifecycleConfirmed, TrustLevel: contextstore.TrustTrusted, Confidence: 1})
		}
	}
	if err := repo.Append(t.Context(), records...); err != nil {
		t.Fatal(err)
	}
	ctx, err := WithContextQueryEvaluationProfile(t.Context(), "candidate")
	if err != nil {
		t.Fatal(err)
	}
	canonical, _, _, err := c.canonicalContextBundleForRequest(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	private := c.recallWorkerMemory(withContextQueryIdentity(ctx, request), def, request)
	if private == nil || private.querySnapshot == nil || canonical == nil || canonical.querySnapshot == nil {
		t.Fatal("ordinary recall did not capture its primary")
	}
	return c, WorkerContextInput{Request: request, Goal: request.Goal, AgentDef: def, TaskDef: TaskDef{Agent: def.Name, Goal: request.Goal}, CanonicalMemory: canonical, WorkerMemory: private, ModelContext: ModelContextSpec{ContextWindow: 8192, MaxOutputTokens: 128, SafetyMarginTokens: 32}}, ctx
}

func workerQueryIDs(bundle *WorkerMemoryBundle) []string {
	if bundle == nil {
		return nil
	}
	ids := make([]string, len(bundle.Items))
	for index, item := range bundle.Items {
		ids[index] = item.ID
	}
	return ids
}

type queryPrivateIterateGuard struct {
	contextstore.Repository
	privateReads int
}

func (g *queryPrivateIterateGuard) Iterate(ctx context.Context, query contextstore.RepositoryQuery, visit func(contextstore.ContextItem) error) error {
	if query.Scope.AgentID != "" {
		g.privateReads++
	}
	return g.Repository.Iterate(ctx, query, visit)
}

type queryEligibilityFailure struct{ contextstore.Repository }

func (g *queryEligibilityFailure) Iterate(ctx context.Context, query contextstore.RepositoryQuery, visit func(contextstore.ContextItem) error) error {
	if query.Scope.AgentID != "" {
		return errors.New("injected private eligibility read failure")
	}
	return g.Repository.Iterate(ctx, query, visit)
}

type queryRuntimeTimings struct {
	contextstore.Repository
	stages []string
}

type queryRuntimeFrozenRepository struct {
	contextstore.Repository
	revision    int64
	private     []contextstore.ContextItem
	activations map[string]contextstore.ActivationRecord
	lexical     map[contextstore.Scope]contextstore.ContextItem
	workerScope contextstore.Scope
}

// Fixture setup freezes actual canonical/activation rows. The expected target
// IDs are authored, not inferred from the candidate ranker. SQL authorization
// is independently covered by the real-store test and context package tests.
func frozenQueryRuntimeRepository(t *testing.T, repo contextstore.Repository, input WorkerContextInput, targets ...string) *queryRuntimeFrozenRepository {
	t.Helper()
	revision, err := repo.Revision(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	workerScope := input.WorkerMemory.querySnapshot.sources[0].Request.Scope
	frozen := &queryRuntimeFrozenRepository{Repository: repo, revision: revision, workerScope: workerScope, activations: make(map[string]contextstore.ActivationRecord), lexical: make(map[contextstore.Scope]contextstore.ContextItem)}
	if err := repo.Iterate(t.Context(), contextstore.RepositoryQuery{Scope: workerScope}, func(item contextstore.ContextItem) error {
		if item.Scope.AgentID == workerScope.AgentID {
			frozen.private = append(frozen.private, item)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, item := range frozen.private {
		record, err := repo.(typedActivationReader).ActivationForItem(t.Context(), item.ID)
		if err != nil {
			t.Fatal(err)
		}
		frozen.activations[item.ID] = record
	}
	if len(targets) == 0 {
		targets = []string{"shared-target", "session-target", "persistent-target"}
	}
	for _, id := range targets {
		item, err := repo.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		frozen.lexical[item.Scope] = item
	}
	return frozen
}

func (r *queryRuntimeFrozenRepository) Revision(ctx context.Context) (int64, error) {
	return r.revision, ctx.Err()
}

func (r *queryRuntimeFrozenRepository) IterateAgentActivations(ctx context.Context, query contextstore.RepositoryQuery, visit func(contextstore.ContextItem, contextstore.ActivationRecord) error) error {
	if query.Scope != r.workerScope || query.Limit != 0 {
		return errors.New("unexpected private eligibility scope")
	}
	for _, item := range r.private {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(item, r.activations[item.ID]); err != nil {
			return err
		}
	}
	return nil
}

func (r *queryRuntimeFrozenRepository) SearchLexical(ctx context.Context, request contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	item, ok := r.lexical[request.Scope]
	if request.Query != "migration sentinel" || request.Limit <= 0 || request.Limit > contextstore.SupplementalMaxSourceRows {
		return nil, errors.New("unexpected supplemental query")
	}
	if !ok && request.Scope == r.workerScope && request.AllowedItemIDs != nil && len(request.AllowedItemIDs) == 0 {
		return []contextstore.SearchResult{}, nil
	}
	if !ok || !slices.Contains(request.AllowedItemIDs, item.ID) {
		return nil, errors.New("unexpected supplemental query or authorization")
	}
	return contextstore.CloneSearchResults([]contextstore.SearchResult{{Item: item, Score: 1}}), nil
}

func (r *queryRuntimeFrozenRepository) SearchSupplementalLexical(ctx context.Context, request contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	return r.SearchLexical(ctx, request)
}

func (r *queryRuntimeTimings) Iterate(ctx context.Context, query contextstore.RepositoryQuery, visit func(contextstore.ContextItem) error) error {
	started := time.Now()
	err := r.Repository.Iterate(ctx, query, visit)
	r.stages = append(r.stages, fmt.Sprintf("iterate(%s)=%s", query.Scope.AgentID, time.Since(started)))
	return err
}

func (r *queryRuntimeTimings) IterateAgentActivations(ctx context.Context, query contextstore.RepositoryQuery, visit func(contextstore.ContextItem, contextstore.ActivationRecord) error) error {
	started := time.Now()
	err := r.Repository.(interface {
		IterateAgentActivations(context.Context, contextstore.RepositoryQuery, func(contextstore.ContextItem, contextstore.ActivationRecord) error) error
	}).IterateAgentActivations(ctx, query, visit)
	r.stages = append(r.stages, fmt.Sprintf("agent_activation_iteration=%s", time.Since(started)))
	return err
}

func (r *queryRuntimeTimings) ActivationsForItems(ctx context.Context, ids []string) (map[string]contextstore.ActivationRecord, error) {
	started := time.Now()
	result, err := r.Repository.(interface {
		ActivationsForItems(context.Context, []string) (map[string]contextstore.ActivationRecord, error)
	}).ActivationsForItems(ctx, ids)
	r.stages = append(r.stages, fmt.Sprintf("activation(%d)=%s", len(ids), time.Since(started)))
	return result, err
}

func (r *queryRuntimeTimings) SearchLexical(ctx context.Context, query contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	started := time.Now()
	result, err := r.Repository.SearchLexical(ctx, query)
	r.stages = append(r.stages, fmt.Sprintf("lexical(%s)=%s", query.Scope.AgentID, time.Since(started)))
	return result, err
}

func (r *queryRuntimeTimings) SearchSupplementalLexical(ctx context.Context, query contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	started := time.Now()
	result, err := r.Repository.(interface {
		SearchSupplementalLexical(context.Context, contextstore.SearchRequest) ([]contextstore.SearchResult, error)
	}).SearchSupplementalLexical(ctx, query)
	r.stages = append(r.stages, fmt.Sprintf("supplemental_lexical(%s)=%s", query.Scope.AgentID, time.Since(started)))
	return result, err
}
