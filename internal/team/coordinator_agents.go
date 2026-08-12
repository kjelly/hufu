package team

// Agent resolution and worker context: name lookup/aliases, the agent cache,
// model resolution, and the injected per-worker context sections.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

func agentCacheKey(def *agent.AgentDef, overrideModel string) string {
	if overrideModel != "" {
		return def.Name + "|" + overrideModel
	}
	return def.Name
}

func (c *Coordinator) getOrCreateAgent(ctx context.Context, def *agent.AgentDef, overrideModel string) (fantasy.Agent, []string, error) {
	cacheKey := agentCacheKey(def, overrideModel)
	c.agentCacheMu.RLock()
	if ag, ok := c.agentCache[cacheKey]; ok {
		names := append([]string(nil), c.agentToolNameCache[cacheKey]...)
		c.agentCacheMu.RUnlock()
		return ag, names, nil
	}
	c.agentCacheMu.RUnlock()

	c.agentCacheMu.Lock()
	defer c.agentCacheMu.Unlock()

	if ag, ok := c.agentCache[cacheKey]; ok {
		return ag, append([]string(nil), c.agentToolNameCache[cacheKey]...), nil
	}

	agentDef := def
	if overrideModel != "" {
		overriddenDef := *def
		overriddenDef.Generation.Model = overrideModel
		agentDef = &overriddenDef
	}

	agentDef = c.injectWorkerContext(ctx, agentDef)

	// Inject SSH session manager into context
	ctx = tools.SetSSHSessionManager(ctx, c.sshSessionMgr)

	agentTools := c.selectWorkerTools(agentDef)
	if c.mcpManager != nil {
		agentTools = append(agentTools, c.mcpManager.AsAgentTools()...)

		// Load agent-specific MCP tools if defined
		if len(agentDef.MCPTools) > 0 {
			err := c.mcpManager.LoadAgentMCPServer(agentDef.Name, agentDef.MCPTools, agentDef.Shell)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to load MCP server for agent %s: %w", agentDef.Name, err)
			}
			mcpTools := c.mcpManager.GetAgentMCPTools(agentDef.Name, agentDef.Shell)
			if len(mcpTools) > 0 {
				agentTools = append(agentTools, mcpTools...)
			}
		}
	}
	agentTools = c.filterDeniedWorkerTools(agentTools)

	getAgModelID := c.resolveAgentModel(agentDef, "")
	ag, err := c.createGatedAgent(ctx, c.providerManager.GetProvider(getAgModelID), agent.AgentConfig{
		Def:        agentDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   c.stepBudget(agentDef, agent.DefaultMaxSteps),
	}, agentTools)
	if err != nil {
		return nil, nil, err
	}

	c.agentCache[cacheKey] = ag
	if c.agentToolNameCache == nil {
		c.agentToolNameCache = make(map[string][]string)
	}
	names := agentToolNames(agentTools)
	c.agentToolNameCache[cacheKey] = append([]string(nil), names...)
	return ag, names, nil
}

// resolveAgentName resolves an agent name (exact, case-insensitive, or fuzzy match)
// to its AgentDef.
//
// Thread Safety: c.session is set once in NewCoordinator and never modified.
// c.session.Agents is populated during team loading and remains read-only during
// execution. Therefore, no mutex protection is needed for reading session fields.
// If future changes require runtime mutation of session, add RWMutex protection.
func (c *Coordinator) resolveAgentName(input string) (*agent.AgentDef, string, error) {
	if c.session == nil {
		return nil, "", fmt.Errorf("session not initialized")
	}
	key := strings.ToLower(input)
	if def, ok := c.session.Agents[key]; ok {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			return nil, "", fmt.Errorf("cannot delegate to coordinator agent %q", input)
		}
		return def, key, nil
	}

	var matches []*agent.AgentDef
	seenNames := make(map[string]bool)
	for _, def := range c.session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		if seenNames[def.Name] {
			continue
		}
		// Match the full display name case-insensitively (e.g. "Helper" → "helper")
		if strings.ToLower(def.Name) == key {
			matches = append(matches, def)
			seenNames[def.Name] = true
			continue
		}
		for _, word := range strings.Fields(def.Name) {
			if strings.ToLower(word) == key {
				matches = append(matches, def)
				seenNames[def.Name] = true
				break
			}
		}
		if seenNames[def.Name] {
			continue
		}
		if def.FileAlias != "" {
			for _, seg := range strings.Split(strings.ToLower(def.FileAlias), "-") {
				if seg != "" && seg == key {
					matches = append(matches, def)
					seenNames[def.Name] = true
					break
				}
			}
		}
	}

	if len(matches) == 1 {
		def := matches[0]
		return def, strings.ToLower(def.Name), nil
	}
	if len(matches) > 1 {
		var names []string
		for _, m := range matches {
			names = append(names, m.Name)
		}
		sort.Strings(names)
		return nil, "", fmt.Errorf("ambiguous agent name %q matches multiple agents: %v", input, names)
	}

	var available []string
	seenAvail := make(map[string]bool)
	for _, def := range c.session.Agents {
		if def.Role != "orchestrator" && def.Role != "coordinator" && !seenAvail[def.Name] {
			seenAvail[def.Name] = true
			available = append(available, def.Name)
		}
	}
	sort.Strings(available)
	return nil, "", fmt.Errorf("unknown agent %q (available: %v)", input, available)
}

// PrimaryWorkerName returns the name of the single worker agent available
// for fast-path direct dispatch. It returns "" when the team has zero or
// multiple worker agents — in those cases the fast path falls through to
// the team path (coordinator DAG), since picking an arbitrary specialist
// could misroute a simple task. The default team (coordinator + Helper)
// has exactly one worker, so this returns "helper".
func (c *Coordinator) PrimaryWorkerName() string {
	defs := c.uniqueWorkerDefs()
	if len(defs) != 1 {
		return ""
	}
	return defs[0].Name
}

func (c *Coordinator) uniqueWorkerDefs() []*agent.AgentDef {
	seen := make(map[string]bool)
	var defs []*agent.AgentDef
	for _, def := range c.session.Agents {
		if def.Role == "orchestrator" || def.Role == "coordinator" {
			continue
		}
		if seen[def.Name] {
			continue
		}
		seen[def.Name] = true
		defs = append(defs, def)
	}
	return defs
}

func (c *Coordinator) buildWorkerNamesAndDescs() (names []string, descs []string) {
	for _, def := range c.uniqueWorkerDefs() {
		desc := def.Name
		aliases := c.agentAliasesFor(def)
		if aliases != "" {
			desc += " (aliases: " + aliases + ")"
		}
		if def.Description != "" {
			desc += ": " + def.Description
		}
		if def.Tools != "" {
			desc += fmt.Sprintf(" (tools: %s)", def.Tools)
		}
		names = append(names, def.Name)
		descs = append(descs, desc)
	}
	return names, descs
}

func (c *Coordinator) agentAliasesFor(def *agent.AgentDef) string {
	nameLower := strings.ToLower(def.Name)
	var parts []string
	if def.FileAlias != "" && strings.ToLower(def.FileAlias) != nameLower {
		parts = append(parts, def.FileAlias)
	}
	for _, word := range strings.Fields(def.Name) {
		w := strings.ToLower(word)
		if w != nameLower && !containsPart(parts, w) {
			parts = append(parts, w)
		}
	}
	if def.FileAlias != "" {
		for _, seg := range strings.Split(strings.ToLower(def.FileAlias), "-") {
			if seg != nameLower && seg != "" && !containsPart(parts, seg) {
				parts = append(parts, seg)
			}
		}
	}
	return strings.Join(parts, ", ")
}

func containsPart(parts []string, s string) bool {
	for _, p := range parts {
		if p == s {
			return true
		}
	}
	return false
}

func (c *Coordinator) workerNameList() []string {
	names, _ := c.buildWorkerNamesAndDescs()
	if c != nil && c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		allowed := make(map[string]bool)
		for _, name := range c.phaseWorkflow.activeWorkerNames() {
			allowed[strings.ToLower(name)] = true
		}
		filtered := make([]string, 0, len(names))
		for _, name := range names {
			if allowed[strings.ToLower(name)] {
				filtered = append(filtered, name)
			}
		}
		return filtered
	}
	if c == nil || c.session == nil || len(c.session.Config.Delegation.AllowedWorkers) == 0 {
		return names
	}
	allowed := make(map[string]bool, len(c.session.Config.Delegation.AllowedWorkers))
	for _, name := range c.session.Config.Delegation.AllowedWorkers {
		allowed[strings.ToLower(strings.TrimSpace(name))] = true
	}
	filtered := make([]string, 0, len(names))
	for _, name := range names {
		if allowed[strings.ToLower(name)] {
			filtered = append(filtered, name)
		}
	}
	return filtered
}

const maxAgentsMDSize = 50000

// defaultWorkerContextSize is a token budget, not a character count (spec.md
// item 7): worker-context-size used to mean "truncate AGENTS.md at N
// characters", inconsistent with the token-aware context budgeting used
// everywhere else in Hufu. The numeric default (12000) is unchanged — only
// its unit is — matching the "project context" line item in a typical
// token-budget breakdown.
const defaultWorkerContextSize = 12000

func (c *Coordinator) loadProjectContext() string {
	path := filepath.Join(c.projectDir, "AGENTS.md")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if utf8.RuneCountInString(string(data)) > maxAgentsMDSize {
		runes := []rune(string(data))
		data = []byte(string(runes[:maxAgentsMDSize]) + "\n\n... [AGENTS.md truncated]")
	}
	return string(data)
}

func (c *Coordinator) getWorkerContext(ctx context.Context) string {
	c.workerCtxOnce.Do(func() {
		raw := c.loadProjectContext()
		if raw == "" {
			return
		}
		modelID := c.coordinatorModelID()
		estimator := globalRegistry.GetSpec(modelID).Estimator
		countTokens := func(s string) int {
			n, _ := defaultCounter.CountText(ctx, modelID, s)
			return n
		}
		ctxTokens := c.getWorkerContextSize()
		if s := c.AgentPool().Sidecar(); s != nil && countTokens(raw) > ctxTokens/2 {
			if c.think {
				c.emitThinkSidecar("Compact", "compacting worker context (AGENTS.md)")
			}
			compacted, err := s.Compact(ctx, raw, "Extract the essential project context: tech stack, language, framework, key conventions, and directory structure. Preserve all factual details but remove verbose descriptions.")
			if err == nil && compacted != "" {
				raw = compacted
			}
		}
		c.cachedWorkerContext = TruncateToTokenBudget(raw, estimator, ctxTokens)
	})
	return c.cachedWorkerContext
}

// getWorkerContextSize returns the worker-context-size token budget
// (session.Config.WorkerContextSize, or defaultWorkerContextSize).
func (c *Coordinator) getWorkerContextSize() int {
	if c.session != nil && c.session.Config.WorkerContextSize > 0 {
		return c.session.Config.WorkerContextSize
	}
	return defaultWorkerContextSize
}

func (c *Coordinator) getWorkerSummary(name string) string {
	c.workerSummariesMu.Lock()
	defer c.workerSummariesMu.Unlock()
	if c.workerSummaries == nil {
		return ""
	}
	return c.workerSummaries[name]
}

func (c *Coordinator) computeWorkerSummaries(ctx context.Context) {
	c.workerSummariesOnce.Do(func() {
		c.workerSummaries = make(map[string]string)
		for _, def := range c.uniqueWorkerDefs() {
			if def.System == "" {
				continue
			}
			c.workerSummariesMu.Lock()
			summary := c.summarizeSystem(ctx, def.System)
			c.workerSummaries[def.Name] = summary
			c.workerSummariesMu.Unlock()
		}
	})
}

func (c *Coordinator) summarizeSystem(ctx context.Context, system string) string {
	if s := c.AgentPool().Sidecar(); s != nil {
		if c.think {
			c.emitThinkSidecar("Compact", "summarizing worker system prompt for coordinator")
		}
		compacted, err := s.Compact(ctx, system, "Summarize this agent's role, key behavioral guidelines, and unique instructions in 2-3 concise sentences. Preserve what makes this agent distinct.")
		if err == nil && compacted != "" {
			return compacted
		}
	}
	if utf8.RuneCountInString(system) > 500 {
		runes := []rune(system)
		return string(runes[:500]) + "..."
	}
	return system
}

func (c *Coordinator) buildCoreReminder(def *agent.AgentDef) string {
	if def.System == "" {
		return ""
	}
	lines := strings.Split(def.System, "\n")
	var keyLines []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "---") {
			continue
		}
		keyLines = append(keyLines, trimmed)
		if len(keyLines) >= 5 {
			break
		}
	}
	if len(keyLines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Core Instructions Reminder\n\n")
	fmt.Fprintf(&b, "You are **%s**", def.Name)
	if def.Description != "" {
		fmt.Fprintf(&b, ": %s", def.Description)
	}
	b.WriteString(". Your core directives:\n")
	for _, line := range keyLines {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	b.WriteString("\nFollow these as your primary directive — they define your identity and priorities.\n")
	return b.String()
}

// injectWorkerContext prepends the project context to the agent's system prompt
// if the agent is not an orchestrator/coordinator and a worker context is available.
// It returns a new AgentDef pointer (either the original unchanged or a copy with
// the modified System field) to avoid mutating shared state.
func (c *Coordinator) injectWorkerContext(ctx context.Context, def *agent.AgentDef) *agent.AgentDef {
	if def.Role == "orchestrator" || def.Role == "coordinator" {
		return def
	}
	wc := c.getWorkerContext(ctx)

	var b strings.Builder
	b.WriteString("## Project Context\n\n")
	if wc != "" {
		b.WriteString(wc)
		b.WriteString("\n\n---\n\n")
	}
	wsPath := c.session.Workspace
	sharedPath := filepath.Join(wsPath, sharedDir)
	b.WriteString("## Environment & Rules\n\n")
	fmt.Fprintf(&b, "- Project root (CWD): %s | Control workspace: %s | Shared: %s | Time: %s\n", c.projectDir, wsPath, sharedPath, c.sessionTime.Format(time.RFC3339))
	fmt.Fprintf(&b, "- Modify deliverables under the project root only when the task authorizes it and the active tool policy permits it.\n")
	fmt.Fprintf(&b, "- Put drafts, logs, notes, and other non-deliverable intermediates in the control workspace: %s\n", wsPath)
	fmt.Fprintf(&b, "- Use %s for inter-agent handoff.\n\n", sharedPath)
	b.WriteString("---\n\n")

	injectedDef := *def
	injectedDef.System = injectedDef.System + "\n\n---\n\n" + b.String()

	if reminder := c.buildCoreReminder(def); reminder != "" {
		injectedDef.System += "\n\n" + reminder
	}

	return &injectedDef
}
func (c *Coordinator) resolveCurrentAgentModel(agentName string) string {
	agentDef, _, err := c.AgentPool().ResolveAgentName(agentName)
	if err == nil && agentDef != nil {
		return c.resolveAgentModel(agentDef, "")
	}
	return c.session.Config.Generation.Model
}

func (c *Coordinator) resolveAgentModel(def *agent.AgentDef, overrideModel string) string {
	if overrideModel != "" {
		return overrideModel
	}
	if def.Generation.Model != "" {
		return def.Generation.Model
	}
	return c.session.Config.Generation.Model
}

// resolveAgentMaxOutputTokens returns the max-output-tokens Hufu will
// actually request for def, using the same agent-first/team-fallback
// precedence CreateAgent uses (internal/agent/agent.go). Returns 0 when
// neither the agent nor the team configured a value, so callers can fall
// back to the model-family registry default (spec.md item 2: the context
// budget must reserve however much output the request will really allow,
// not a hardcoded per-family guess).
func (c *Coordinator) resolveAgentMaxOutputTokens(def *agent.AgentDef) int {
	raw := ""
	if def != nil && def.Generation.MaxTokens != "" {
		raw = def.Generation.MaxTokens
	} else if c.session != nil {
		raw = c.session.Config.Generation.MaxTokens
	}
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// providerSemaphore lazily builds (once per provider) and returns the
// concurrency-limiting channel for provider, or nil when that provider has
// no configured max-concurrent — in which case only the team-wide semaphore
// in dagScheduler applies, matching the pre-existing behavior.
func (c *Coordinator) providerSemaphore(provider string) chan struct{} {
	if provider == "" || c.session == nil {
		return nil
	}
	cfg, ok := c.session.Config.Providers[provider]
	if !ok || cfg.MaxConcurrent <= 0 {
		return nil
	}
	c.providerSemMu.Lock()
	defer c.providerSemMu.Unlock()
	if c.providerSem == nil {
		c.providerSem = make(map[string]chan struct{})
	}
	sem, ok := c.providerSem[provider]
	if !ok {
		sem = make(chan struct{}, cfg.MaxConcurrent)
		c.providerSem[provider] = sem
	}
	return sem
}

// coordinatorModelID returns the team default model used by the coordinator for
// model-aware token accounting (compaction records, context budget reporting).
// It falls back to "default" when no team model is configured.
func (c *Coordinator) coordinatorModelID() string {
	if c.session != nil && c.session.Config.Generation.Model != "" {
		return c.session.Config.Generation.Model
	}
	return "default"
}
