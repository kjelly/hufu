package team

import (
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// validateActionCatalog reports the catalog's structural findings and then
// checks each entry against the loaded team. It is pure: it does not modify
// the session, and every call on the same session returns the same findings.
func validateActionCatalog(session *TeamSession) []ContractFinding {
	if session == nil {
		return nil
	}
	findings := append([]ContractFinding(nil), session.actionCatalogFindings...)
	if session.ActionCatalog == nil {
		return findings
	}
	reachable := make(map[*agent.AgentDef]bool)
	for _, def := range reachableWorkers(session) {
		reachable[def] = true
	}
	for _, entry := range session.ActionCatalog.Entries {
		findings = append(findings, validateActionCatalogProvider(session, entry)...)
		findings = append(findings, validateActionCatalogAgents(session, entry, reachable)...)
		findings = append(findings, validateActionCatalogProposers(session, entry, reachable)...)
		findings = append(findings, validateActionCatalogPhase(session, entry)...)
	}
	findings = append(findings, validateActionCatalogToolSequences(session)...)
	return findings
}

func validateActionCatalogProvider(session *TeamSession, entry ActionCatalogEntry) []ContractFinding {
	if session.ProviderRegistry != nil && session.ProviderRegistry.Has(entry.Capability) {
		return nil
	}
	if _, ok := configuredActionProvider(session.Config.ActionProviders, entry.Capability); ok {
		return nil
	}
	return []ContractFinding{errorFinding(actionCatalogField(entry.ID, "capability"), FindingActionProviderMissing,
		fmt.Sprintf("action %q capability %q has no configured action provider", entry.ID, entry.Capability))}
}

// validateActionCatalogAgents checks the entry's executing agent and every
// discover/propose role.
func validateActionCatalogAgents(session *TeamSession, entry ActionCatalogEntry, reachable map[*agent.AgentDef]bool) []ContractFinding {
	var findings []ContractFinding
	executor := sessionAgentDef(session, entry.Agent)
	switch {
	case executor == nil:
		findings = append(findings, errorFinding(actionCatalogField(entry.ID, "agent"), FindingActionCatalogAgentUnknown,
			fmt.Sprintf("action %q agent %q is not an agent of this team", entry.ID, entry.Agent)))
	case isCoordinatorRole(executor) || normalizedName(executor.Name) == "helper" || !reachable[executor]:
		findings = append(findings, errorFinding(actionCatalogField(entry.ID, "agent"), FindingActionCatalogAgentUnreachable,
			fmt.Sprintf("action %q agent %q must be a team worker the coordinator can dispatch (not the coordinator, helper, or a worker outside allowed-workers)", entry.ID, entry.Agent)))
	default:
		if len(executor.ExtraModels) > 0 {
			findings = append(findings, errorFinding(actionCatalogField(entry.ID, "agent"), FindingActionCatalogAgentUnsupported,
				fmt.Sprintf("action %q agent %q declares extra-models, which catalog actions do not support", entry.ID, entry.Agent)))
		}
		if entry.SideEffect != SideEffectNone && workerWorkspaceSpecFor(session, executor).Isolated() {
			findings = append(findings, errorFinding(actionCatalogField(entry.ID, "agent"), FindingActionCatalogAgentUnsupported,
				fmt.Sprintf("action %q agent %q uses an isolated worker workspace, so the entry must be side-effect: none", entry.ID, entry.Agent)))
		}
	}
	for _, list := range []struct {
		field string
		names []string
	}{{"access.discover", entry.Discover}, {"access.propose", entry.Propose}} {
		for _, name := range list.names {
			def := sessionAgentDef(session, name)
			if def == nil {
				findings = append(findings, errorFinding(actionCatalogField(entry.ID, list.field), FindingActionCatalogAgentUnknown,
					fmt.Sprintf("action %q %s lists %q, which is not an agent of this team", entry.ID, list.field, name)))
				continue
			}
			if len(def.ExtraModels) > 0 {
				findings = append(findings, errorFinding(actionCatalogField(entry.ID, list.field), FindingActionCatalogAgentUnsupported,
					fmt.Sprintf("action %q %s lists %q, which declares extra-models", entry.ID, list.field, name)))
			}
		}
	}
	return findings
}

// validateActionCatalogProposers rejects a require-proposal entry that no
// worker can actually record a proposal for.
func validateActionCatalogProposers(session *TeamSession, entry ActionCatalogEntry, reachable map[*agent.AgentDef]bool) []ContractFinding {
	if !entry.RequireProposal {
		return nil
	}
	reasons := actionCatalogProposerExclusions(session, entry, reachable, func(def *agent.AgentDef) bool {
		return agentUsesExternalBackend(session, def)
	})
	if len(reasons) < len(entry.Propose) {
		return nil
	}
	return []ContractFinding{errorFinding(actionCatalogField(entry.ID, "invocation.require-proposal"), FindingActionCatalogProposerUnreachable,
		actionCatalogProposerMessage(entry, reasons))}
}

// actionCatalogProposerExclusions returns, for every propose role that cannot
// receive team_action_propose, why not. A role absent from the result is an
// effective proposer. externalBackend decides whether a role runs on a
// backend that does not receive Hufu tools.
func actionCatalogProposerExclusions(session *TeamSession, entry ActionCatalogEntry, reachable map[*agent.AgentDef]bool, externalBackend func(*agent.AgentDef) bool) map[string]string {
	proposeDenied := slices.ContainsFunc(session.Config.ToolsDenied, func(name string) bool {
		return normalizedName(name) == teamActionProposeToolName
	})
	reasons := make(map[string]string)
	for _, name := range entry.Propose {
		def := sessionAgentDef(session, name)
		switch {
		case def == nil:
			reasons[name] = "not an agent of this team"
		case !reachable[def]:
			reasons[name] = "not a worker the coordinator can dispatch"
		case proposeDenied:
			reasons[name] = "team tools-denied removes " + teamActionProposeToolName
		case externalBackend(def):
			reasons[name] = "runs on an external agent backend that does not receive Hufu tools"
		}
	}
	return reasons
}

func actionCatalogProposerMessage(entry ActionCatalogEntry, reasons map[string]string) string {
	if len(entry.Propose) == 0 {
		return fmt.Sprintf("action %q requires a proposal but access.propose is empty, so it can never be dispatched", entry.ID)
	}
	details := make([]string, 0, len(entry.Propose))
	for _, name := range entry.Propose {
		details = append(details, fmt.Sprintf("%s: %s", name, reasons[name]))
	}
	return fmt.Sprintf("action %q requires a proposal but no propose role can record one (%s)", entry.ID, strings.Join(details, "; "))
}

// validateActionCatalogPhase checks that a workflow team has a phase where
// the entry may be dispatched.
func validateActionCatalogPhase(session *TeamSession, entry ActionCatalogEntry) []ContractFinding {
	if len(session.Config.Workflow.Phases) == 0 {
		return nil
	}
	phases, err := normalizeWorkflowPhases(session.Config.Workflow.Phases)
	if err != nil {
		return nil
	}
	hasExecute := slices.Contains(phases, PhaseExecute)
	hasPrepare := slices.Contains(phases, PhasePrepare)
	if hasExecute || entry.SideEffect == SideEffectNone && hasPrepare {
		return nil
	}
	allowed := "an execute phase"
	if entry.SideEffect == SideEffectNone {
		allowed = "a prepare or execute phase"
	}
	return []ContractFinding{errorFinding(actionCatalogField(entry.ID, "side-effect"), FindingActionCatalogPhaseUnreachable,
		fmt.Sprintf("action %q (%s) needs %s, which this workflow does not declare", entry.ID, entry.SideEffect, allowed))}
}

// validateActionCatalogToolSequences rejects a closed tool sequence that
// names a catalog protocol tool: those tools are exposed per agent and are
// not part of a static contract.
func validateActionCatalogToolSequences(session *TeamSession) []ContractFinding {
	var findings []ContractFinding
	for index, task := range session.ContractTasks {
		for _, tool := range task.Execution.ToolSequence {
			if slices.Contains(teamActionToolNames, normalizedName(tool)) {
				findings = append(findings, errorFinding(fmt.Sprintf("tasks[%d].execution.tool-sequence", index), FindingActionCatalogToolSequenceUnsupported,
					fmt.Sprintf("tool-sequence cannot list %q; action catalog tools are not part of a closed tool sequence", tool)))
			}
		}
	}
	return findings
}

func isCoordinatorRole(def *agent.AgentDef) bool {
	role := normalizedName(def.Role)
	return role == "coordinator" || role == "orchestrator"
}

// pruneInvalidActionCatalogEntries removes every entry with an error finding
// from a lint-mode snapshot, so later lint checks see only valid entries.
func pruneInvalidActionCatalogEntries(session *TeamSession, findings []ContractFinding) error {
	if session == nil || session.ActionCatalog == nil {
		return nil
	}
	invalid := make(map[string]bool)
	for _, finding := range findings {
		if finding.Severity != FindingSeverityError {
			continue
		}
		if id, ok := actionCatalogFieldID(finding.Field); ok {
			invalid[id] = true
		}
	}
	if len(invalid) == 0 {
		return nil
	}
	kept := session.ActionCatalog.Entries[:0:0]
	for _, entry := range session.ActionCatalog.Entries {
		if !invalid[entry.ID] {
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		session.ActionCatalog = nil
		return nil
	}
	session.ActionCatalog = &ActionCatalogSnapshot{Version: session.ActionCatalog.Version, Entries: kept}
	return session.ActionCatalog.rehash()
}

// ValidateActionCatalogProposers repeats the proposer check once the
// coordinator has resolved every worker's execution target, which is when
// --worker-model and model selectors such as codex/... take effect. It runs
// before any provider is contacted.
func (c *Coordinator) ValidateActionCatalogProposers() error {
	if c == nil || c.session == nil || c.session.ActionCatalog == nil {
		return nil
	}
	reachable := make(map[*agent.AgentDef]bool)
	for _, def := range reachableWorkers(c.session) {
		reachable[def] = true
	}
	for _, entry := range c.session.ActionCatalog.Entries {
		if !entry.RequireProposal {
			continue
		}
		reasons := actionCatalogProposerExclusions(c.session, entry, reachable, c.workerUsesAgentBackend)
		if len(reasons) == len(entry.Propose) {
			return fmt.Errorf("%s: %s", FindingActionCatalogProposerUnreachable, actionCatalogProposerMessage(entry, reasons))
		}
	}
	return nil
}

// workerUsesAgentBackend reports whether def's primary execution target is an
// external agent backend, resolved the way task admission resolves it.
func (c *Coordinator) workerUsesAgentBackend(def *agent.AgentDef) bool {
	model, err := c.ModelRuntime().ResolveTaskModel(def, TaskDef{Agent: def.Name})
	if err != nil {
		return agentUsesExternalBackend(c.session, def)
	}
	legacyProvider := strings.TrimSpace(def.SubagentProvider)
	if legacyProvider == "" {
		legacyProvider = strings.TrimSpace(c.session.Config.SubagentProviderDefault)
	}
	target, err := c.resolveCanonicalTaskTarget(model, legacyProvider)
	if err != nil || target.IsZero() {
		return agentUsesExternalBackend(c.session, def)
	}
	backend, err := c.ExecutionRegistry().ResolveBackend(target.Backend)
	if err != nil {
		return agentUsesExternalBackend(c.session, def)
	}
	return backend.Kind() == execution.BackendKindAgent
}
