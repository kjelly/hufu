package evalharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/improve"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

// defaultCaseTimeout bounds a single case's wall-clock time so a stuck
// scripted run (e.g. a fixture that never triggers the coordinator's
// natural stop) cannot hang CI indefinitely (§13 "per-case timeout"). A
// package-level var, not a const, so tests can shrink it instead of
// waiting out the real default -- see TestEvalTimeout.
var defaultCaseTimeout = 60 * time.Second

const maxRunErrorDiagnosticRunes = 500

// ErrCaseNotFound is wrapped into RunSuite's error when caseID is set but no
// case in the fixture matches it. A caller filtering by case across several
// suite fixtures should treat this as "not in this one" rather than fatal,
// and only fail once no fixture matched at all.
var ErrCaseNotFound = errors.New("case not found in suite")

// RunSuite executes every case in a suite fixture, or -- when caseID is
// non-empty -- only that one case, and returns their canonical results.
func RunSuite(ctx context.Context, fixture *SuiteFixture, caseID string) (EvalSuiteResult, error) {
	suiteResult := EvalSuiteResult{
		SuiteName:         fixture.Name,
		BenchmarkRevision: improve.BenchmarkRevision(fixture.BenchmarkFixture()),
	}
	matched := false
	for _, c := range fixture.Cases {
		if caseID != "" && c.ID != caseID {
			continue
		}
		matched = true
		caseResult, err := runCase(ctx, fixture, c)
		if err != nil {
			return suiteResult, fmt.Errorf("case %s/%s: %w", fixture.Name, c.ID, err)
		}
		suiteResult.Cases = append(suiteResult.Cases, caseResult)
	}
	if caseID != "" && !matched {
		return suiteResult, fmt.Errorf("suite %s: case %q not found: %w", fixture.Name, caseID, ErrCaseNotFound)
	}
	return suiteResult, nil
}

// runCase drives one case end-to-end: a fresh scripted provider and a fresh
// team.Coordinator, loaded from the suite's bundled team, run against the
// case's prompt and provider fixture, and the resulting RunResult/events are
// asserted against the case's ExpectSpec. No two cases share a Coordinator or
// workspace.
func runCase(ctx context.Context, fixture *SuiteFixture, c CaseFixture) (EvalCaseResult, error) {
	providerFixture, err := LoadProviderFixture(fixture.ProviderFixturePath(c))
	if err != nil {
		return EvalCaseResult{}, err
	}
	provider := newScriptedProvider(providerFixture)
	return runCaseWithHandler(ctx, fixture, c, provider, provider.unconsumed)
}

// runCaseWithHandler is runCase's implementation, taking the model driver as
// a plain http.Handler (plus its own unconsumed-step reporter, or nil) so
// tests can substitute a handler with different failure behavior --
// see TestEvalTimeout, which needs a handler that never responds.
func runCaseWithHandler(ctx context.Context, fixture *SuiteFixture, c CaseFixture, handler http.Handler, unconsumed func() []ProviderStep) (EvalCaseResult, error) {
	started := time.Now()

	server := httptest.NewServer(handler)
	defer server.Close()

	teamDir := fixture.TeamDir()
	session, err := team.LoadTeam(teamDir, nil, nil, nil)
	if err != nil {
		return EvalCaseResult{}, fmt.Errorf("load team %s: %w", teamDir, err)
	}
	if err := validateOfflineSession(session); err != nil {
		return EvalCaseResult{}, fmt.Errorf("offline eval team %s: %w", teamDir, err)
	}
	workspace, err := os.MkdirTemp("", "hufu-eval-*")
	if err != nil {
		return EvalCaseResult{}, fmt.Errorf("create case workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	session.Workspace = workspace
	if err := seedWorkspaceFiles(workspace, c.WorkspaceFiles); err != nil {
		return EvalCaseResult{}, err
	}
	projectDir, err := bindEvalCaseWorkspace(session, workspace)
	if err != nil {
		return EvalCaseResult{}, fmt.Errorf("bind eval workspace scope: %w", err)
	}
	if err := seedCanonicalContext(context.WithoutCancel(ctx), workspace, projectDir, session.Config.Name, c.ContextItems, c.SeedMemoryPolicy, session.Config.MemoryLearning); err != nil {
		return EvalCaseResult{}, err
	}

	coordinator, err := team.NewCoordinator(
		session,
		server.URL+"/v1", "eval-harness-key",
		nil, // mcpManager
		nil, // memoryStore
		nil, // modelList
		// Judge is set unconditionally: a case whose team.yaml enables a
		// decision profile needs a judge model configured or the decision
		// engine fails closed with "decision_budget_insufficient" before
		// ever reaching the scripted provider. Harmless for every other
		// case -- the "off" profile never calls RunJudge.
		team.RoleModels{Judge: evalModelDriverName},
		1,     // maxConcurrent
		false, // verbose
		false, // think
		false, // direnv
		nil,   // allowedPaths
		nil,   // pathConsent
		nil,   // hookRegistry
		false, // rbashMode
		"",    // restrictedPath
		true,  // noNet: eval workers may not use network-capable tools
		false, // forceMCP
		nil,   // forcedSkillNames
		false, // planMode
		false, // autoSkillsMode
	)
	if err != nil {
		return EvalCaseResult{}, fmt.Errorf("construct coordinator: %w", err)
	}
	defer func() { _ = coordinator.Close() }()
	if c.DecisionProfileOverride != "" {
		coordinator.SetDecisionProfile(c.DecisionProfileOverride)
	}
	// Follow the production CLI's crash-resume path when a fixture seeds a
	// session.json checkpoint. Loading it after coordinator construction and
	// before policy freeze makes ResumeInterruptedTasks exercise the same
	// durable Todo projection instead of treating the file as inert input.
	if restored := team.LoadSession(workspace); restored != nil {
		if c.SeedExecutionPolicySnapshot {
			if seedErr := seedPriorRunPolicyAndTasks(context.WithoutCancel(ctx), workspace, coordinator, restored, c.PriorRunDecisionAdmissionDigests); seedErr != nil {
				return EvalCaseResult{}, seedErr
			}
		}
		coordinator.SetSessionData(restored)
	} else if c.SeedExecutionPolicySnapshot {
		return EvalCaseResult{}, errors.New("seed execution policy snapshot requires workspace-files session.json")
	}
	if err := coordinator.FreezeExecutionPolicyAtStartup(); err != nil {
		return EvalCaseResult{}, fmt.Errorf("freeze execution policy: %w", err)
	}

	var events []team.StatusEvent
	coordinator.SetStatusReporter(func(e team.StatusEvent) {
		events = append(events, e)
	})

	caseCtx, cancel := context.WithTimeout(ctx, defaultCaseTimeout)
	defer cancel()
	_, runErr := coordinator.Run(caseCtx, c.Prompt)
	runResult := coordinator.LastRunResult()
	tasks := coordinator.TaskTracker().TodoList().Items()

	findings := assertRun(c.Expect, runResult, coordinator.TerminalLifecycleConfirmed(), events, tasks)
	durableEvents, durableEventsErr := coordinator.EventJournal().ReadEvents(context.WithoutCancel(caseCtx))
	if durableEventsErr != nil {
		findings = append(findings, EvalFinding{
			Dimension: "durable-events",
			Expected:  "readable append-only event journal",
			Actual:    durableEventsErr.Error(),
		})
	} else {
		findings = append(findings, assertDurableEvents(c.Expect.DurableEvents, durableEvents, tasks)...)
	}
	findings = append(findings, assertEvidence(context.WithoutCancel(caseCtx), workspace, c.Expect.Evidence, runResult, tasks)...)
	runID := ""
	if runResult != nil {
		runID = runResult.RunID
	}
	findings = append(findings, assertAudit(context.WithoutCancel(caseCtx), workspace, c.Expect.AuditVerdict, runID)...)
	findings = append(findings, assertMemoryAggregates(context.WithoutCancel(caseCtx), workspace, c.Expect.MemoryAggregates)...)
	if errors.Is(caseCtx.Err(), context.DeadlineExceeded) {
		findings = append(findings, EvalFinding{
			Dimension: "timeout",
			Expected:  fmt.Sprintf("run to finish within %s", defaultCaseTimeout),
			Actual:    "deadline exceeded",
		})
	}
	if runErr != nil && (runResult == nil || runResult.Outcome == team.RunOutcomeFailed) {
		// A populated failed result is normally the canonical projection of
		// unresolved work, but a failure before task admission also produces a
		// result through finalizePublicInvocationFailure. Preserve that error in
		// the harness output so provider-boundary/startup failures cannot be
		// reduced to misleading zero-task fixture mismatches.
		findings = append(findings, EvalFinding{
			Dimension: "run-error",
			Expected:  "no pre-admission Run error",
			Actual:    boundedRunErrorDiagnostic(runErr),
		})
	}
	if unconsumed != nil {
		for _, leftover := range unconsumed() {
			findings = append(findings, EvalFinding{
				Dimension: "provider-fixture",
				Expected:  "every scripted step consumed",
				Actual:    fmt.Sprintf("unused step: %+v", leftover),
			})
		}
	}

	outcome := ""
	normalizedRunID := ""
	if runResult != nil {
		outcome = string(runResult.Outcome)
		normalizedRunID = normalizeOpaqueID(runResult.RunID)
	}
	return EvalCaseResult{
		CaseID:     c.ID,
		Passed:     len(findings) == 0,
		RunOutcome: outcome,
		Findings:   findings,
		Metrics:    EvalMetrics{Duration: time.Since(started), RunID: normalizedRunID},
	}, nil
}

func boundedRunErrorDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	redacted := utils.RedactSecrets(err.Error())
	if len([]rune(redacted)) <= maxRunErrorDiagnosticRunes {
		return redacted
	}
	return utils.TruncateRunes(redacted, maxRunErrorDiagnosticRunes-len("..."))
}

func validateOfflineSession(session *team.TeamSession) error {
	if session == nil {
		return errors.New("team session is nil")
	}
	cfg := session.Config
	if strings.TrimSpace(cfg.ProviderURL) != "" || strings.TrimSpace(cfg.ProviderAPIKey) != "" {
		return errors.New("team-level provider-url/provider-api-key is forbidden; the harness owns the only provider endpoint")
	}
	if len(cfg.Providers) > 0 {
		return errors.New("named providers are forbidden; the harness owns the only provider endpoint")
	}
	if len(cfg.Backends) > 0 {
		return errors.New("configured execution backends are forbidden in offline evals")
	}
	if len(cfg.SubagentProviders) > 0 {
		return errors.New("external subagent providers are forbidden in offline evals")
	}
	if provider := strings.TrimSpace(cfg.SubagentProviderDefault); provider != "" && provider != "hufu-local" {
		return fmt.Errorf("team default subagent provider %q is not offline", provider)
	}
	teamSelectors := map[string]string{
		"team generation model": cfg.Generation.Model,
		"worker model":          cfg.WorkerModel,
		"coordinator model":     cfg.CoordinatorModel,
		"sidecar model":         cfg.SidecarModel,
		"guard model":           cfg.GuardModel,
		"judge model":           cfg.JudgeModel,
		"plan reviewer model":   cfg.PlanReviewerModel,
	}
	for _, label := range slices.Sorted(maps.Keys(teamSelectors)) {
		if err := validateOfflineSelector(label, teamSelectors[label]); err != nil {
			return err
		}
	}
	for index, model := range cfg.ModelList {
		if err := validateOfflineSelector(fmt.Sprintf("model-list[%d]", index), model.ID); err != nil {
			return err
		}
	}

	seen := make(map[*agent.AgentDef]bool)
	for _, name := range slices.Sorted(maps.Keys(session.Agents)) {
		definition := session.Agents[name]
		if definition == nil || seen[definition] {
			continue
		}
		seen[definition] = true
		if strings.TrimSpace(definition.ProviderURL) != "" {
			return fmt.Errorf("agent %q declares provider-url; the harness owns the only provider endpoint", definition.Name)
		}
		if provider := strings.TrimSpace(definition.SubagentProvider); provider != "" && provider != "hufu-local" {
			return fmt.Errorf("agent %q subagent provider %q is not offline", definition.Name, provider)
		}
		if err := validateOfflineSelector("agent "+definition.Name+" model", definition.Generation.Model); err != nil {
			return err
		}
		for index, model := range definition.ExtraModels {
			if err := validateOfflineSelector(fmt.Sprintf("agent %s extra-models[%d]", definition.Name, index), model); err != nil {
				return err
			}
		}
	}
	for index, task := range session.ContractTasks {
		if provider := strings.TrimSpace(task.SubagentProvider); provider != "" && provider != "hufu-local" {
			return fmt.Errorf("contract task %d subagent provider %q is not offline", index, provider)
		}
		if err := validateOfflineSelector(fmt.Sprintf("contract task %d model", index), task.Model); err != nil {
			return err
		}
		for modelIndex, model := range task.ModelTopology {
			if err := validateOfflineSelector(fmt.Sprintf("contract task %d model-topology[%d]", index, modelIndex), model); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateOfflineSelector(label, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	selector, err := execution.ParseExecutionSelector(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if selector.Backend != "" && !execution.IsOllamaBackend(selector.Backend) {
		return fmt.Errorf("%s selects non-offline backend %q", label, selector.Backend)
	}
	return nil
}

func bindEvalCaseWorkspace(session *team.TeamSession, workspace string) (string, error) {
	if err := session.SetCompatibilityWorkspaceScope(workspace); err != nil {
		return "", err
	}
	return session.Scope.SubjectRoot, nil
}

func seedCanonicalContext(ctx context.Context, workspace, projectDir, teamID string, fixtures []ContextItemFixture, seedMemoryPolicy bool, policy agent.MemoryLearningPolicy) (returnErr error) {
	if len(fixtures) == 0 && !seedMemoryPolicy {
		return nil
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return fmt.Errorf("open context store for seed: %w", err)
	}
	defer func() {
		if closeErr := repo.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("close seeded context store: %w", closeErr)
		}
	}()

	if seedMemoryPolicy {
		if policy.Mode == agent.MemoryLearningOff {
			return errors.New("seed memory policy requires a non-off team memory-learning mode")
		}
		revision := "eval-" + policy.PolicyVersion
		snapshot := map[string]any{
			"id": policy.PolicyVersion, "revision_hash": revision, "learning": policy,
			"retrieval": map[string]any{
				"top_k": 20, "minimum_relevance": 0.05,
				"utility_weight": 0.5, "freshness_weight": 1.0,
			},
		}
		raw, marshalErr := json.Marshal(snapshot)
		if marshalErr != nil {
			return fmt.Errorf("marshal active memory policy: %w", marshalErr)
		}
		if err := repo.SaveMemoryPolicyVersion(ctx, policy.PolicyVersion, raw, revision, "active", time.Unix(1, 0).UTC()); err != nil {
			return fmt.Errorf("seed active memory policy: %w", err)
		}
	}

	items := make([]contextstore.ContextItem, 0, len(fixtures))
	for _, fixture := range fixtures {
		items = append(items, contextstore.ContextItem{
			ID:         fixture.ID,
			Kind:       contextstore.ContextPattern,
			Content:    fixture.Content,
			Scope:      contextstore.Scope{ProjectID: projectDir, TeamID: teamID},
			Authority:  contextstore.AuthorityRepository,
			TrustLevel: contextstore.TrustTrusted,
			Priority:   contextstore.PriorityHigh,
			MustKeep:   fixture.MustKeep,
			Confidence: 1,
			Source:     contextstore.SourceRef{Type: "eval_fixture", Ref: fixture.ID},
			Lifecycle:  contextstore.LifecycleConfirmed,
		})
	}
	if len(items) > 0 {
		if err := repo.Append(ctx, items...); err != nil {
			return fmt.Errorf("seed context items: %w", err)
		}
	}
	return nil
}

func assertMemoryAggregates(ctx context.Context, workspace string, expects []MemoryAggregateExpect) []EvalFinding {
	if len(expects) == 0 {
		return nil
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		return []EvalFinding{{Dimension: "memory-aggregates", Expected: "readable context.sqlite learning projection", Actual: err.Error()}}
	}
	defer func() { _ = repo.Close() }()

	var findings []EvalFinding
	for _, expect := range expects {
		aggregate, aggregateErr := repo.ExperienceAggregate(ctx, expect.ContextItemID, expect.PolicyVersion)
		if aggregateErr != nil {
			findings = append(findings, EvalFinding{
				Dimension: "memory-aggregate." + expect.ContextItemID,
				Expected:  "aggregate for policy " + expect.PolicyVersion,
				Actual:    aggregateErr.Error(),
			})
			continue
		}
		prefix := "memory-aggregate." + expect.ContextItemID + "."
		if expect.MinExposureCount != nil && aggregate.ExposureCount < *expect.MinExposureCount {
			findings = append(findings, EvalFinding{Dimension: prefix + "exposure-count", Expected: fmt.Sprintf(">= %d", *expect.MinExposureCount), Actual: fmt.Sprintf("%d", aggregate.ExposureCount)})
		}
		findings = append(findings, compareMemoryAggregateInt(prefix+"consulted-count", expect.ConsultedCount, aggregate.ConsultedCount)...)
		findings = append(findings, compareMemoryAggregateInt(prefix+"applied-count", expect.AppliedCount, aggregate.AppliedCount)...)
		findings = append(findings, compareMemoryAggregateInt(prefix+"rejected-count", expect.RejectedCount, aggregate.RejectedCount)...)
		findings = append(findings, compareMemoryAggregateFloat(prefix+"positive-weight", expect.PositiveWeight, aggregate.PositiveWeight)...)
		findings = append(findings, compareMemoryAggregateFloat(prefix+"negative-weight", expect.NegativeWeight, aggregate.NegativeWeight)...)
	}
	return findings
}

func compareMemoryAggregateInt(dimension string, expected *int, actual int) []EvalFinding {
	if expected == nil || actual == *expected {
		return nil
	}
	return []EvalFinding{{Dimension: dimension, Expected: fmt.Sprintf("%d", *expected), Actual: fmt.Sprintf("%d", actual)}}
}

func compareMemoryAggregateFloat(dimension string, expected *float64, actual float64) []EvalFinding {
	if expected == nil || math.Abs(actual-*expected) <= 1e-9 {
		return nil
	}
	return []EvalFinding{{Dimension: dimension, Expected: fmt.Sprintf("%g", *expected), Actual: fmt.Sprintf("%g", actual)}}
}

// seedPriorRunPolicyAndTasks creates the event-first half of a historical
// checkpoint before SetSessionData installs its matching session projection.
// The provider endpoint participates in the policy fingerprint and is chosen
// dynamically by httptest, so this canonical history cannot be static YAML.
func seedPriorRunPolicyAndTasks(ctx context.Context, workspace string, coordinator *team.Coordinator, restored *team.SessionData, admissionDigests map[string]string) (returnErr error) {
	snapshot := coordinator.ExecutionPolicySnapshot()
	if snapshot == nil {
		return errors.New("seed execution policy snapshot: coordinator has no resolved policy")
	}
	store, err := team.NewEventStore(workspace, "eval-prior-run", "eval-prior-session")
	if err != nil {
		return fmt.Errorf("seed prior-run event store: %w", err)
	}
	defer func() {
		if closeErr := store.Close(); returnErr == nil && closeErr != nil {
			returnErr = fmt.Errorf("close prior-run event store: %w", closeErr)
		}
	}()

	policyPayload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("seed execution policy snapshot: %w", err)
	}
	if _, err := store.AppendPersistedContext(ctx, team.RunEvent{
		Type:    string(team.EventExecutionPolicySnapshot),
		Actor:   "coordinator",
		Payload: policyPayload,
	}); err != nil {
		return fmt.Errorf("seed execution policy snapshot event: %w", err)
	}
	for _, task := range restored.Tasks {
		if task == nil {
			continue
		}
		digest := admissionDigests[task.ID]
		if digest == "" {
			continue
		}
		attempt := task.Retries + 1
		admission := team.DecisionAdmission{
			SchemaVersion:   team.DecisionAdmissionSchemaVersion,
			RunID:           "eval-prior-run",
			TaskID:          task.ID,
			Attempt:         attempt,
			Profile:         team.DecisionProfileOff,
			Source:          team.DecisionProfileSourceDefault,
			TaskInputDigest: digest,
		}
		payload, marshalErr := json.Marshal(admission)
		if marshalErr != nil {
			return fmt.Errorf("seed decision admission for task %s: %w", task.ID, marshalErr)
		}
		if _, appendErr := store.AppendPersistedContext(ctx, team.RunEvent{
			Type: string(team.EventDecisionAdmitted), Actor: "coordinator", TaskID: task.ID, Attempt: attempt, Payload: payload,
		}); appendErr != nil {
			return fmt.Errorf("seed decision admission for task %s event: %w", task.ID, appendErr)
		}
	}
	for _, task := range restored.Tasks {
		if task == nil {
			continue
		}
		payload, payloadErr := legacyTaskCreatedPayload(task)
		if payloadErr != nil {
			return fmt.Errorf("seed legacy task %s: %w", task.ID, payloadErr)
		}
		if _, appendErr := store.AppendPersistedContext(ctx, team.RunEvent{
			Type:    string(team.EventTaskCreated),
			Actor:   task.Agent,
			TaskID:  task.ID,
			Payload: payload,
		}); appendErr != nil {
			return fmt.Errorf("seed legacy task %s event: %w", task.ID, appendErr)
		}
	}
	restored.ExecutionPolicySnapshot = snapshot
	return nil
}

// legacyTaskCreatedPayload converts TodoItem's checkpoint wire names into the
// canonical task-event names used by the reducer. Several compatibility fields
// predate JSON tags and therefore otherwise marshal as Go field names.
func legacyTaskCreatedPayload(task *team.TodoItem) (json.RawMessage, error) {
	data, err := json.Marshal(task)
	if err != nil {
		return nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, err
	}
	for oldName, eventName := range map[string]string{
		"ID":             "id",
		"Agent":          "agent",
		"Desc":           "desc",
		"Status":         "status",
		"Detail":         "detail",
		"Output":         "output",
		"Skills":         "skills",
		"InjectedSkills": "injected_skills",
		"LoadedSkills":   "loaded_skills",
		"Source":         "source",
		"ParentID":       "parent_id",
		"DependsOn":      "depends_on",
		"Verify":         "verify",
		"VerifyMode":     "verify_mode",
		"VerifyResult":   "verify_result",
		"MaxRetries":     "max_retries",
		"Retries":        "retries",
		"OnFailure":      "on_failure",
	} {
		if value, ok := payload[oldName]; ok {
			payload[eventName] = value
			delete(payload, oldName)
		}
	}
	return json.Marshal(payload)
}

// seedWorkspaceFiles writes a case's WorkspaceFiles into its ephemeral
// workspace before the run starts, e.g. a fan_out source manifest a
// scripted tool_call references by workspace-relative path.
func seedWorkspaceFiles(workspace string, files map[string]string) error {
	for _, relPath := range slices.Sorted(maps.Keys(files)) {
		absPath, err := resolveWorkspaceSeedPath(workspace, relPath)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return fmt.Errorf("create directory for workspace file %s: %w", relPath, err)
		}
		if err := os.WriteFile(absPath, []byte(files[relPath]), 0o644); err != nil {
			return fmt.Errorf("write workspace file %s: %w", relPath, err)
		}
	}
	return nil
}

func resolveWorkspaceSeedPath(workspace, relPath string) (string, error) {
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("workspace file %q must be relative", relPath)
	}
	cleaned := filepath.Clean(relPath)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workspace file %q escapes the case workspace", relPath)
	}
	return filepath.Join(workspace, cleaned), nil
}
