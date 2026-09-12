package evalharness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// defaultCaseTimeout bounds a single case's wall-clock time so a stuck
// scripted run (e.g. a fixture that never triggers the coordinator's
// natural stop) cannot hang CI indefinitely (§13 "per-case timeout"). A
// package-level var, not a const, so tests can shrink it instead of
// waiting out the real default -- see TestEvalTimeout.
var defaultCaseTimeout = 60 * time.Second

// ErrCaseNotFound is wrapped into RunSuite's error when caseID is set but no
// case in the fixture matches it. A caller filtering by case across several
// suite fixtures should treat this as "not in this one" rather than fatal,
// and only fail once no fixture matched at all.
var ErrCaseNotFound = errors.New("case not found in suite")

// RunSuite executes every case in a suite fixture, or -- when caseID is
// non-empty -- only that one case, and returns their canonical results.
func RunSuite(ctx context.Context, fixture *SuiteFixture, caseID string) (EvalSuiteResult, error) {
	suiteResult := EvalSuiteResult{SuiteName: fixture.Name}
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
	workspace, err := os.MkdirTemp("", "hufu-eval-*")
	if err != nil {
		return EvalCaseResult{}, fmt.Errorf("create case workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	session.Workspace = workspace
	if err := seedWorkspaceFiles(workspace, c.WorkspaceFiles); err != nil {
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
		false, // noNet
		false, // forceMCP
		nil,   // forcedSkillNames
		false, // planMode
		false, // autoSkillsMode
	)
	if err != nil {
		return EvalCaseResult{}, fmt.Errorf("construct coordinator: %w", err)
	}
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

	findings := assertRun(c.Expect, runResult, events, tasks)
	durableEvents, durableEventsErr := coordinator.EventJournal().ReadEvents(context.WithoutCancel(caseCtx))
	if durableEventsErr != nil {
		findings = append(findings, EvalFinding{
			Dimension: "durable-events",
			Expected:  "readable append-only event journal",
			Actual:    durableEventsErr.Error(),
		})
	} else {
		findings = append(findings, assertDurableEvents(c.Expect.DurableEvents, durableEvents)...)
	}
	if errors.Is(caseCtx.Err(), context.DeadlineExceeded) {
		findings = append(findings, EvalFinding{
			Dimension: "timeout",
			Expected:  fmt.Sprintf("run to finish within %s", defaultCaseTimeout),
			Actual:    "deadline exceeded",
		})
	}
	if runErr != nil && runResult == nil {
		// Run() returning an error alongside a populated RunResult is a
		// normal outcome path (e.g. unresolved tasks); only a nil RunResult
		// means the harness has nothing to assert against.
		findings = append(findings, EvalFinding{
			Dimension: "run-error",
			Expected:  "a RunResult even on error",
			Actual:    runErr.Error(),
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
	runID := ""
	if runResult != nil {
		outcome = string(runResult.Outcome)
		runID = normalizeOpaqueID(runResult.RunID)
	}
	return EvalCaseResult{
		CaseID:     c.ID,
		Passed:     len(findings) == 0,
		RunOutcome: outcome,
		Findings:   findings,
		Metrics:    EvalMetrics{Duration: time.Since(started), RunID: runID},
	}, nil
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
	for relPath, content := range files {
		absPath := filepath.Join(workspace, relPath)
		if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
			return fmt.Errorf("create directory for workspace file %s: %w", relPath, err)
		}
		if err := os.WriteFile(absPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write workspace file %s: %w", relPath, err)
		}
	}
	return nil
}
