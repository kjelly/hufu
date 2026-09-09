package main

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/team"
)

// targetExecutableLookup is injectable so setup validation can be tested
// without starting a backend process. It must only inspect PATH.
type targetExecutableLookup func(string) (string, error)

type preflightExecutionTarget struct {
	role           string
	raw            string
	legacyProvider string
	llm            bool
}

// preflightExecutionTargets validates every statically selected target before
// coordinator construction. In particular it must not open a workspace store,
// launch a process, contact a provider, or perform an authentication handshake.
func preflightExecutionTargets(session *team.TeamSession, cfg *config.Config, roles team.RoleModels, lookup targetExecutableLookup) error {
	if session == nil {
		return fmt.Errorf("execution target preflight requires a team session")
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	if lookup == nil {
		lookup = exec.LookPath
	}

	llmBackends := map[string]struct{}{"local": {}}
	agentBackends := map[string]agent.SubagentProviderConfig{"codex": {
		Type:    "codex-app-server",
		Command: []string{"codex", "app-server"},
	}}
	for name := range session.Config.Providers {
		name = execution.CanonicalBackendName(name)
		if _, exists := agentBackends[name]; exists {
			return fmt.Errorf("backend name %q is defined as both an LLM provider and an agent provider", name)
		}
		llmBackends[name] = struct{}{}
	}
	for name, backend := range session.Config.SubagentProviders {
		name = execution.CanonicalBackendName(name)
		if name == "local" {
			return fmt.Errorf("reserved backend name %q cannot be configured as an agent backend", name)
		}
		if strings.TrimSpace(backend.Type) != "codex-app-server" {
			return fmt.Errorf("legacy agent backend %q has unsupported type %q", name, backend.Type)
		}
		if _, exists := llmBackends[name]; exists {
			return fmt.Errorf("backend name %q is defined as both an LLM provider and an agent provider", name)
		}
		if name == "codex" && backend.Type == "codex-app-server" {
			if len(backend.Command) == 0 {
				backend.Command = []string{"codex", "app-server"}
			}
			agentBackends[name] = backend // specialize built-in process settings
			continue
		}
		agentBackends[name] = backend
	}

	defaultLLM := session.Config.DefaultLLMBackend
	if defaultLLM == "" {
		defaultLLM = cfg.DefaultLLMBackend
	}
	if defaultLLM == "" {
		defaultLLM = "local"
	}
	defaultLLM = execution.CanonicalBackendName(defaultLLM)
	if _, ok := llmBackends[defaultLLM]; !ok {
		return fmt.Errorf("default LLM backend %q is not a registered language-model backend", defaultLLM)
	}

	targets := []preflightExecutionTarget{
		{role: "worker", raw: session.Config.WorkerModel, legacyProvider: session.Config.SubagentProviderDefault},
		{role: "coordinator", raw: session.Config.CoordinatorModel, llm: true},
		{role: "sidecar", raw: roles.Sidecar, llm: true},
		{role: "guard", raw: roles.Guard, llm: true},
		{role: "judge", raw: roles.Judge, llm: true},
		{role: "plan reviewer", raw: roles.PlanReviewer, llm: true},
	}
	// Agent-local generation models and extra-model fan-out leaves are also
	// statically selectable execution targets. They are not necessarily equal
	// to the resolved team worker target, so validate their backend namespace
	// and executable availability before any workspace or lifecycle side
	// effects can begin.
	for name, def := range session.Agents {
		if def == nil || strings.EqualFold(strings.TrimSpace(def.Role), "coordinator") {
			continue
		}
		model := strings.TrimSpace(def.Generation.Model)
		if model == "" {
			model = strings.TrimSpace(session.Config.WorkerModel)
		}
		if model == "" {
			model = strings.TrimSpace(session.Config.Generation.Model)
		}
		legacyProvider := strings.TrimSpace(def.SubagentProvider)
		if legacyProvider == "" {
			legacyProvider = strings.TrimSpace(session.Config.SubagentProviderDefault)
		}
		if model != "" {
			targets = append(targets, preflightExecutionTarget{role: fmt.Sprintf("agent %s", name), raw: model, legacyProvider: legacyProvider})
		}
		for index, model := range def.ExtraModels {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			targets = append(targets, preflightExecutionTarget{role: fmt.Sprintf("agent %s extra model %d", name, index+1), raw: model, legacyProvider: legacyProvider})
		}
	}

	for _, candidate := range targets {
		if strings.TrimSpace(candidate.raw) == "" {
			continue
		}
		effectiveRaw := candidate.raw
		selector, err := execution.ParseExecutionSelector(effectiveRaw)
		if err != nil {
			return fmt.Errorf("invalid %s execution target %q: %w", candidate.role, candidate.raw, err)
		}
		// A qualified selector is already an explicit canonical target. A bare
		// model inherits the legacy provider only for compatibility, matching
		// Coordinator.canonicalizeTaskOccurrence exactly.
		if selector.Backend == "" {
			legacyProvider := execution.CanonicalBackendName(candidate.legacyProvider)
			if legacyProvider != "" && legacyProvider != "hufu-local" {
				effectiveRaw = legacyProvider + "/" + selector.Model
				selector, err = execution.ParseExecutionSelector(effectiveRaw)
				if err != nil {
					return fmt.Errorf("invalid %s execution target %q: %w", candidate.role, candidate.raw, err)
				}
			}
		}
		backend := selector.Backend
		if backend == "" {
			backend = defaultLLM
		}
		if _, ok := llmBackends[backend]; ok {
			continue
		}
		backendConfig, ok := agentBackends[backend]
		if !ok {
			return fmt.Errorf("unknown execution backend %q for %s target %q", backend, candidate.role, candidate.raw)
		}
		if candidate.llm {
			return fmt.Errorf("%s target %q uses agent backend %q, which does not provide a direct language model", candidate.role, candidate.raw, backend)
		}
		if len(backendConfig.Command) == 0 || strings.TrimSpace(backendConfig.Command[0]) == "" {
			return fmt.Errorf("%s target %q: %s backend has no configured executable", candidate.role, candidate.raw, backend)
		}
		if _, err := lookup(backendConfig.Command[0]); err != nil {
			return fmt.Errorf("%s target %q: %s executable %q is unavailable: %w", candidate.role, candidate.raw, backend, backendConfig.Command[0], err)
		}
	}
	return nil
}

// preflightSidecarTarget is the narrow target gate for CLI-owned auxiliary
// sidecar calls that do not execute worker or coordinator roles. It shares the
// canonical backend mapping and static validation path with a normal run.
func preflightSidecarTarget(session *team.TeamSession, cfg *config.Config, model string, lookup targetExecutableLookup) error {
	if err := applyConfiguredBackends(session, cfg); err != nil {
		return err
	}
	return preflightExecutionTargets(session, cfg, team.RoleModels{Sidecar: model}, lookup)
}

// preflightRestoredExecutionTargets checks frozen canonical targets and
// read-only legacy migration plans from a checkpoint before lifecycle code
// can archive, clean, checkpoint, or construct a coordinator. The migration
// plan is never persisted here; the append-only migration reducer remains the
// only component permitted to freeze it durably at dispatch.
func preflightRestoredExecutionTargets(session *team.TeamSession, cfg *config.Config, items []*team.TodoItem, lookup targetExecutableLookup) error {
	return preflightRestoredExecutionTargetsWithEvidence(session, cfg, items, nil, nil, lookup)
}

func preflightRestoredExecutionTargetsWithEvidence(session *team.TeamSession, cfg *config.Config, items []*team.TodoItem, evidence []team.RunEvent, canonicalItems []*team.TodoItem, lookup targetExecutableLookup) error {
	for _, item := range items {
		if item == nil || !resumableTaskStatus(item.Status) {
			continue
		}
		planItem := item
		if item.ExecutionTarget.IsZero() {
			// An event-first migration may already have frozen the canonical
			// target while the checkpoint write was interrupted. Prefer that
			// checked replay projection for preflight; the durable append still
			// remains the runtime migration boundary.
			for _, canonical := range canonicalItems {
				if canonical != nil && canonical.ID == item.ID && !canonical.ExecutionTarget.IsZero() {
					planItem = canonical
					break
				}
			}
		}
		targets, err := team.RestoredExecutionTargetsForPreflight(planItem, evidence)
		if err != nil {
			return fmt.Errorf("restored task %q execution target: %w", item.ID, err)
		}
		seen := make(map[execution.ExecutionTarget]struct{}, len(targets))
		checkTarget := func(label string, target execution.ExecutionTarget) error {
			if target.IsZero() {
				return nil
			}
			if _, exists := seen[target]; exists {
				return nil
			}
			seen[target] = struct{}{}
			if err := target.Validate(); err != nil {
				return fmt.Errorf("restored task %q %s target %q: %w", item.ID, label, target, err)
			}
			copySession := *session
			copySession.Config = session.Config
			copySession.Config.WorkerModel = target.String()
			copySession.Config.SubagentProviderDefault = ""
			// Static configuration was already checked above. Avoid repeating live
			// agent generation/extra-model discovery here; this pass validates only
			// the frozen durable target leaves.
			copySession.Agents = nil
			if err := preflightExecutionTargets(&copySession, cfg, team.RoleModels{}, lookup); err != nil {
				return fmt.Errorf("restored task %q %s target %q: %w", item.ID, label, target, err)
			}
			return nil
		}
		for index, target := range targets {
			label := "primary"
			if index > 0 {
				label = fmt.Sprintf("topology leaf %d", index)
			}
			if err := checkTarget(label, target); err != nil {
				return err
			}
		}
	}
	return nil
}

// preflightRestoredEventLineage performs the read-only crash-window check for
// event-first transitions that may not yet be represented in session.json.
func preflightRestoredEventLineage(session *team.TeamSession, cfg *config.Config, lookup targetExecutableLookup) error {
	items, evidence, err := team.ReadCheckedActiveExecutionTaskEvidence(session.Workspace)
	if err != nil {
		return fmt.Errorf("restored event lineage preflight failed: %w", err)
	}
	if err := preflightRestoredExecutionTargetsWithEvidence(session, cfg, items, evidence, nil, lookup); err != nil {
		return err
	}
	return nil
}

func resumableTaskStatus(status team.TaskStatus) bool {
	switch status {
	case team.TaskPending, team.TaskPlanned, team.TaskInProgress, team.TaskPaused, team.TaskVerifying, team.TaskProtocolIncomplete:
		return true
	default:
		return false
	}
}
