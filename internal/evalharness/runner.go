package evalharness

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

	coordinator, err := team.NewCoordinator(
		session,
		server.URL+"/v1", "eval-harness-key",
		nil,               // mcpManager
		nil,               // memoryStore
		nil,               // modelList
		team.RoleModels{}, // roleModels
		1,                 // maxConcurrent
		false,             // verbose
		false,             // think
		false,             // direnv
		nil,               // allowedPaths
		nil,               // pathConsent
		nil,               // hookRegistry
		false,             // rbashMode
		"",                // restrictedPath
		false,             // noNet
		false,             // forceMCP
		nil,               // forcedSkillNames
		false,             // planMode
		false,             // autoSkillsMode
	)
	if err != nil {
		return EvalCaseResult{}, fmt.Errorf("construct coordinator: %w", err)
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
