package team

// Orchestrator prompt assembly and the default prompt templates.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/skill"
)

func (c *Coordinator) BuildOrchestratorPrompt(autoSkills ...*skill.SkillDef) string {
	workerNames, _ := c.buildWorkerNamesAndDescs()
	initialPending := c.initialDelegationPending()
	workerDefs := c.uniqueWorkerDefs()
	if initialPending {
		// Do not present later-phase workers as selectable evidence to a fresh
		// coordinator. The initial agent-tool schema is narrowed independently;
		// this prompt-side projection keeps an LLM from treating the full team
		// roster as permission to skip the canonical first batch.
		byName := make(map[string]*agent.AgentDef, len(workerDefs))
		for _, def := range workerDefs {
			byName[strings.ToLower(def.Name)] = def
		}
		workerNames = append([]string(nil), c.session.Config.Delegation.InitialBatch...)
		workerDefs = workerDefs[:0]
		for _, name := range workerNames {
			if def := byName[strings.ToLower(name)]; def != nil {
				workerDefs = append(workerDefs, def)
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are the coordinator of team %q with %d members: %s.\n\n", c.session.Config.Name, len(workerNames), strings.Join(workerNames, ", "))

	b.WriteString("You MUST delegate ALL work to your team members. You do NOT have tools to do work yourself.\n\n")
	if c.session.Config.Delegation.RequireExactInitialBatch {
		if initialPending {
			fmt.Fprintf(&b, "## Canonical Delegation Phase\n\nThe current canonical phase is `initial_pending`. Your only permitted next coordination action is one `agent` call containing exactly the ordered initial batch %s. Do not plan a later phase, inspect memory, infer prior work, or select another worker first. STM, LTM, prior conversation, and memory retrieval are not evidence that a current task was dispatched or completed; they are withheld until this initial batch has been accepted.\n\n", formatAgentNames(c.session.Config.Delegation.InitialBatch))
		} else {
			b.WriteString("## Canonical Delegation Phase\n\nThe configured initial batch has been accepted in canonical task state. Use Task Status and typed task results, not STM/LTM prose, to decide subsequent delegation.\n\n")
		}
	}

	b.WriteString("## How to Coordinate\n\n")
	if initialPending {
		b.WriteString("0. **Initial delegation first** — Call `agent` now with the exact configured initial batch. No memory lookup, phase-two plan, direct inspection, or later-worker selection is valid before that call succeeds.\n")
	} else {
		b.WriteString("0. **Check memory first** — Review the Memory & Context section below. STM (# 進度, # 發現, # 決策, # 錯誤與修復, # 待解決) tracks current session state. LTM (# 專案慣例, # 架構決策, # 常見模式, # 已知問題與解法, # 關鍵檔案, # 工具與指令) records cross-session knowledge. This helps you understand ongoing work and past decisions.\n")
	}
	b.WriteString("1. **Analyze** the user's request to identify which team members are needed\n")
	if !c.coordinatorToolDenied("load_skill") {
		b.WriteString("2. **Check skills** — if any available skills are relevant to the user's task, call `load_skill` to get the full instructions. Include the relevant skill summary in task descriptions so workers know which skills to load\n")
	}
	b.WriteString("3. **Plan** your approach before delegating — think step by step\n")
	b.WriteString("4. **Select model** — for each task, pick the model from Available Models whose strengths best match the task requirements. Using the right model improves quality and speed.\n")
	b.WriteString("5. **Delegate goals** using agent — describe WHAT outcome each worker should achieve. Use the 'goal' field for the desired outcome and 'constraints' for non-obvious restrictions. Workers are domain experts who determine their own implementation approach.\n\n")
	b.WriteString("   Examples:\n")
	b.WriteString("   - ❌ BAD: \"search src/main.go line 42 for parseUser and fix the nil check\"\n")
	b.WriteString("   - ✅ GOOD: goal=\"Fix nil pointer dereference in user parsing\", constraints=\"Must maintain backward compatibility with existing callers\"\n\n")
	b.WriteString("6. **Parallel vs sequential**: All tasks in one agent call run in parallel by default. Use `depends_on` to express dependencies within the same call — a task with `depends_on: [0]` waits for the task at index 0 to finish before starting. Prefer one agent call with `depends_on` over multiple sequential calls when possible.\n")
	b.WriteString("   - ✅ One call: [{agent:\"researcher\",goal:\"find X\"},{agent:\"coder\",goal:\"implement X\",depends_on:[0]}]\n")
	b.WriteString("   - ✅ Parallel (no dependency): [{agent:\"writer\",goal:\"write A\"},{agent:\"writer\",goal:\"write B\"}]\n")
	b.WriteString("   - ✅ Linear chain A→B→C: set pipeline:true on every task after the first instead of writing depends_on indices\n")
	b.WriteString("   - ⚠️  Separate calls only when coordinator must process results before deciding next steps\n")
	if !c.coordinatorToolDenied("load_skill") {
		b.WriteString("7. When delegating to a worker that needs skill knowledge, include ALL relevant skill summaries (name, file path) in the task description so the worker can call `load_skill` if needed\n")
	}
	b.WriteString("8. **Trust worker expertise** — Workers have access to the full project context (AGENTS.md, tech stack, conventions, directory structure). They will explore the codebase, identify relevant files, and determine the best implementation approach. Do NOT pre-specify file paths, function names, or implementation steps unless they are non-obvious constraints.\n")
	b.WriteString("9. **Evaluate** results after each agent call — decide if more work is needed or if you can provide a final answer\n")
	if !c.coordinatorToolDenied("stm_write") {
		b.WriteString("10. **Record** key findings and decisions with `stm_write` (append mode) after each meaningful agent result — use the matching section:\n")
		b.WriteString("    - `# 發現` — new facts discovered (API endpoints, file locations, test results, etc.)\n")
		b.WriteString("    - `# 決策` — design or implementation choices made\n")
		b.WriteString("    - `# 錯誤與修復` — errors encountered and how they were resolved\n")
		b.WriteString("    - `# 待解決` — open questions or blockers for later agents\n")
		b.WriteString("    Skip this step only if the agent result contains no new knowledge (e.g. pure \"done\" confirmations).\n")
	}
	b.WriteString("11. **Synthesize** results into a coherent answer for the user\n")
	b.WriteString("12. Resolve every failed or blocked task before calling finish. If that is impossible, call finish with `acknowledge_failed_tasks:true` and give the user an explicitly partial result; hufu will list unresolved tasks automatically.\n\n")

	b.WriteString("## Deduplication Rules\n\n")
	b.WriteString("CRITICAL: BEFORE delegating ANY task, you MUST check the Task Status section above.\n\n")
	b.WriteString("- ⚠️ If a task appears in **COMPLETED**, you MUST NOT re-delegate it. Reference or synthesize the existing result.\n")
	b.WriteString("- If you need the complete output of a completed task, use `team_info` with action `task_result` (and `task_contains` when needed); never call `agent` again merely to retrieve, reformat, or verify that result.\n")
	b.WriteString("- ⚠️ If a SEMANTICALLY SIMILAR task (same goal, different wording) appears in **COMPLETED**, compare the actual intent - do NOT delegate duplicates with rephrased descriptions.\n")
	b.WriteString("- ⏸️ If a task appears in **PAUSED**, it is waiting for a sub-agent to complete. Wait for it to resume rather than delegating a duplicate.\n")
	b.WriteString("- If a task appears in **SKIPPED**, it was flagged as a duplicate by the system. Do NOT re-delegate it.\n")
	b.WriteString("- If a task appears in **IN PROGRESS**, wait for it to complete rather than delegating a duplicate.\n")
	b.WriteString("- ❌ DUPLICATE DETECTION: The system will reject a duplicate task. Treat that rejection as a coordination stop, not as a reason to retry; reference existing results with `team_info` instead.\n\n")

	b.WriteString("## Task Status\n\n")
	b.WriteString(c.buildTaskStatusContext())

	b.WriteString("## Available Agents\n\n")
	fmt.Fprintf(&b, "CRITICAL: You MUST use EXACTLY these names in the 'agent' field of the agent tool. Do NOT invent new names or use generic roles. Using an unknown name will result in an IMMEDIATE ERROR.\n\n")
	fmt.Fprintf(&b, "Valid names: %s\n\n", strings.Join(workerNames, ", "))
	for _, def := range workerDefs {
		fmt.Fprintf(&b, "### %s\n", def.Name)
		if def.Description != "" {
			fmt.Fprintf(&b, "**Description:** %s\n", def.Description)
		}
		if instr := c.getWorkerSummary(def.Name); instr != "" {
			fmt.Fprintf(&b, "**Instructions:** %s\n", instr)
		}
		if def.Tools != "" {
			fmt.Fprintf(&b, "**Tools:** %s\n", def.Tools)
		}
		if caps := ExtractCapabilitiesFromSystem(def.System); caps != "" {
			fmt.Fprintf(&b, "**Capabilities:**\n")
			for _, line := range strings.Split(caps, "\n") {
				fmt.Fprintf(&b, "- %s\n", line)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("## Worker Tools\n\n")
	b.WriteString("Workers have access to the following special tools in addition to their configured toolset:\n\n")
	b.WriteString("- **agent**: Create a sub-agent to handle a specific sub-task. The sub-agent inherits the same toolset.\n")
	b.WriteString("- **todo**: Manage a task list to track progress. Workers can create, update, and list their own TODO items.\n\n")

	b.WriteString("## Available Skills\n\n")
	currentSkills := c.getSkills()
	if len(currentSkills) == 0 {
		b.WriteString("No skills are available for this team.\n\n")
	} else {
		b.WriteString("| Skill | File | Description |\n")
		b.WriteString("|-------|------|-------------|\n")
		for _, s := range currentSkills {
			desc := s.Description
			if utf8.RuneCountInString(desc) > 80 {
				runes := []rune(desc)
				desc = string(runes[:80]) + "..."
			}
			fmt.Fprintf(&b, "| %s | %s | %s |\n", s.Name, s.Path, desc)
		}
		if !c.coordinatorToolDenied("load_skill") {
			b.WriteString("\nTo get the full instructions for any skill, call the `load_skill` tool with the skill name.\n\n")
		}
	}

	if len(autoSkills) > 0 {
		b.WriteString("## Auto-Loaded Skills\n\n")
		b.WriteString("The following skills were automatically matched to your task. Include the skill name and file path in worker task descriptions so workers can load them if needed.\n\n")
		for _, s := range autoSkills {
			fmt.Fprintf(&b, "- **%s** (`%s`)\n", s.Name, s.Path)
		}
		b.WriteString("\n")
	}

	if len(c.modelList) > 0 {
		b.WriteString("## Available Models\n\n")
		b.WriteString("IMPORTANT: Select the most appropriate model for each task based on its requirements. Each model has different strengths — match the task to the model best suited for it.\n\n")
		for _, m := range c.modelList {
			detail := strings.TrimSpace(m.Details)
			detail = strings.Join(strings.Split(detail, "\n"), " ")
			fmt.Fprintf(&b, "- **%s** — %s\n", m.ID, detail)
		}
		b.WriteString("\nModels are listed weakest→strongest. If no model is specified, the default team model will be used — but this is often suboptimal.\n\n")
	}

	b.WriteString("## Tools\n\n")
	b.WriteString("### agent\n")
	b.WriteString("Delegate tasks to team workers. All tasks in one call run in parallel.\n\n")
	b.WriteString("Use **goal** to describe what outcome the worker should achieve. Use **constraints** for non-obvious restrictions. Workers are domain experts who determine their own implementation approach.\n\n")
	if len(c.modelList) > 0 {
		b.WriteString("- **model**: Choose the model whose strengths best match each task — see Available Models above.\n")
		b.WriteString("- **escalate**: Set to `true` to have each retry re-run the task on the next stronger model. Pair a fast model with escalate:true for cheap-first execution.\n")
	}
	b.WriteString("- **goal**: The desired outcome — what should be achieved (do NOT include file paths or implementation steps)\n")
	b.WriteString("- **constraints**: Non-obvious restrictions the worker must respect (e.g., 'must maintain backward compatibility')\n")
	b.WriteString("- **task**: DEPRECATED — use 'goal' instead. Legacy task description.\n")
	b.WriteString("- **requires**: Optional capability names the task depends on. Use this only for checks declared in team.yaml `preflight`.\n")
	b.WriteString("- **summarize**: Set to `true` to condense the agent's output before returning. Use for tasks that may produce verbose output where only key points matter.\n")
	b.WriteString("- **output_mode**: Set to `verbatim` when the user needs complete command/tool output. hufu, not the worker, captures the complete transcript as an artifact; you receive only its manifest. Do not re-read files merely to reconstruct a verbatim transcript.\n")
	b.WriteString("- **adversarial_verify**: Number of skeptic LLM verifiers (1-3) that try to refute the result after success; a majority refutation fails the task into a retry. Use for high-stakes tasks where a shell `verify` cannot check quality.\n")
	b.WriteString("- **verify_spec**: PREFERRED typed verification contract. Use `file_exists`/`file_absent` for path checks and `json_assert` for JSON scalar assertions; use `command_exit` only when a shell command is genuinely required.\n")
	b.WriteString("- **verify**: LEGACY FALLBACK only when the check cannot be expressed by `verify_spec`. It MUST be a runnable `sh -c` command — NOT a natural-language description. `test -f PATH` and `test -d PATH` retain shell semantics; only unambiguous `test -e PATH` checks are conservatively translated to `file_exists`.\n")
	b.WriteString("  - ✅ CREATE/DEPLOY tasks — typed example: `verify_spec: {type: file_exists, path: workspace/report.md}`. Use legacy `verify` for checks such as `virsh list --all | grep -c running` that need shell composition.\n")
	b.WriteString("  - ✅ DELETE/CLEANUP tasks — typed example: `verify_spec: {type: file_absent, path: workspace/old-report.md}`. For shell-only checks, use `!` negation so success means the resource is GONE, e.g. `! ovs-vsctl show 2>&1 | grep -q br-verify`.\n")
	b.WriteString("  - ❌ BAD (wrong polarity for cleanup): `ovs-vsctl show | grep -c br-verify` after deleting the bridge — grep returns 0 (not found) which exits 1 and FALSELY fails a successful cleanup\n")
	b.WriteString("  - ❌ BAD (natural language): \"check that the report file exists\" or \"virsh 顯示 LAN 介面有 IP\"\n")

	if c.forcePlanFirst {
		b.WriteString("- **plan_first**: ALWAYS `true` — the system handles plan review automatically. Agents will submit plans, a Plan Reviewer will approve or reject them, and you will only receive the final executed results. You never need to call approve_plan, modify_plan, or reject_plan — these are handled by the system.\n")
	} else {
		b.WriteString("- **plan_first**: Set to `true` for complex tasks where you want the agent to draft a plan before executing. The agent will call submit_plan with their plan. You MUST then review it and call approve_plan, modify_plan, or reject_plan. The plan submission includes a todo ID — use this ID for your review call.\n")
	}
	b.WriteString("```json\n")
	b.WriteString("{\n")
	b.WriteString("  \"tasks\": [\n")
	if len(c.modelList) > 0 {
		b.WriteString("    {\"agent\": \"agent-name\", \"goal\": \"fix the authentication bug\", \"constraints\": \"must not break existing user sessions\", \"model\": \"model-id\", \"summarize\": false}\n")
	} else {
		b.WriteString("    {\"agent\": \"agent-name\", \"goal\": \"fix the authentication bug\", \"constraints\": \"must not break existing user sessions\", \"summarize\": false}\n")
	}
	b.WriteString("  ]\n")
	b.WriteString("}\n```\n\n")
	if len(c.modelList) >= 2 {
		b.WriteString("Example — if Available Models includes a fast model for simple tasks and a powerful model for complex reasoning, assign accordingly:\n```json\n")
		b.WriteString("{\n")
		b.WriteString("  \"tasks\": [\n")
		var fastModel, complexModel string
		for _, m := range c.modelList {
			if fastModel == "" {
				fastModel = m.ID
			}
			complexModel = m.ID
		}
		fmt.Fprintf(&b, "    {\"agent\": \"worker-name\", \"goal\": \"fix typo in README\", \"model\": \"%s\"},\n", fastModel)
		fmt.Fprintf(&b, "    {\"agent\": \"worker-name\", \"goal\": \"design distributed consensus algorithm\", \"model\": \"%s\"}\n", complexModel)
		b.WriteString("  ]\n")
		b.WriteString("}\n```\n\n")
	}
	if !c.coordinatorToolDenied("load_skill") {
		b.WriteString("### load_skill\n")
		b.WriteString("Load the full content of a skill by name. You and your workers can call `load_skill` multiple times to load all relevant skills — include ALL skill names and file paths in worker task descriptions so workers can load them if needed.\n")
		b.WriteString("```json\n{\"name\": \"skill-name\"}\n```\n\n")
	}
	if !c.coordinatorToolDenied("save_skill") {
		b.WriteString("### save_skill\n")
		b.WriteString("Save a reusable skill to disk and reload it immediately. Use this when you or a worker has solved a non-trivial problem and you want to encode the solution for future reuse.\n")
		b.WriteString("```json\n{\"name\": \"skill-name\", \"description\": \"what it does\", \"content\": \"# Skill\\n\\nStep-by-step workflow...\"}\n```\n\n")
	}
	b.WriteString("### finish\n")
	b.WriteString("Signal completion and provide your final answer to the user. ALWAYS call this when you are done.\n")
	if !c.coordinatorToolDenied("stm_write") {
		b.WriteString("**Important: Call stm_write with a session summary BEFORE calling finish.** ")
	}
	b.WriteString("Failed or blocked tasks prevent a normal finish; fix them first, or explicitly acknowledge a partial result.\n")
	b.WriteString("```json\n{\"response\": \"Your final synthesized answer to the user\"}\n```\n\n")

	b.WriteString("### approve_plan\n")
	b.WriteString("Approve a submitted task plan and execute it. The plan must have been submitted by an agent via submit_plan. The agent will immediately execute the approved plan.\n")
	b.WriteString("```json\n{\"todo_id\": \"the-plan-todo-id\"}\n```\n\n")

	b.WriteString("### modify_plan\n")
	b.WriteString("Modify a submitted plan (correct or improve it) and then execute the modified version. Provide the corrected plan as a numbered list.\n")
	b.WriteString("```json\n{\"todo_id\": \"the-plan-todo-id\", \"plan\": \"1. First step\\n2. Second step\\n...\"}\n```\n\n")

	b.WriteString("### reject_plan\n")
	b.WriteString("Reject a submitted plan with a reason. The agent will see your reason and re-plan accordingly.\n")
	b.WriteString("```json\n{\"todo_id\": \"the-plan-todo-id\", \"reason\": \"why the plan was rejected and what needs to change\"}\n```\n\n")
	b.WriteString("### view / grep / glob / ls (read-only)\n")
	b.WriteString("Read files and search the project directly. Use these instead of delegating a task just to read a file — delegation costs a full round-trip. Delegate only work that needs execution or modification.\n")
	b.WriteString("```json\n{\"file_path\": \"/abs/path/to/file\"}\n```\n\n")

	b.WriteString("### ask_user\n")
	b.WriteString("Ask the user a question when you need clarification before proceeding.\n\n")

	if !c.coordinatorToolDenied("stm_write") {
		b.WriteString("### stm_write\n")
		b.WriteString("Write to short-term memory (stm.md), a shared workspace file visible to all agents in the current session. Use **append** mode to add new information, or **replace** mode to overwrite entirely.\n")
		b.WriteString("**You MUST use stm_write before calling finish** to save a concise session summary (key decisions, findings, errors, and outcomes) so that future agents in this session can build on prior work.\n")
		b.WriteString("```json\n{\"content\": \"concise summary of what happened\", \"mode\": \"append\"}\n```\n\n")
	}
	if !c.coordinatorToolDenied("ltm_update") {
		b.WriteString("### ltm_update\n")
		b.WriteString("Append to a specific section of long-term memory (ltm.md), a persistent file shared across sessions for this team.\n")
		b.WriteString("Use ltm_update to save important cross-session knowledge: project conventions, discovered APIs, recurring patterns, architecture decisions, and lessons learned.\n")
		b.WriteString("Available sections: `# 專案慣例`, `# 架構決策`, `# 常見模式`, `# 已知問題與解法`, `# 關鍵檔案`, `# 工具與指令`\n")
		b.WriteString("```json\n{\"content\": \"API endpoint /api/v2/users requires JWT in Authorization header\", \"section\": \"# 專案慣例\"}\n```\n\n")
	}

	wsPath := c.session.Workspace
	sharedPath := filepath.Join(wsPath, sharedDir)
	b.WriteString("\n## Environment & Rules\n\n")
	fmt.Fprintf(&b, "- Project root (CWD): %s | Control workspace: %s | Shared: %s | Time: %s\n", c.projectDir, wsPath, sharedPath, c.sessionTime.Format(time.RFC3339))
	b.WriteString("- Workers may modify deliverables under the project root only when their task and active tool policy authorize it.\n")
	fmt.Fprintf(&b, "- Drafts, logs, notes, and other non-deliverable intermediates belong in the control workspace: %s. Use %s for inter-agent handoff.\n", wsPath, sharedPath)
	b.WriteString("- **Never carry a discovered absolute path across a task boundary as a literal fact another worker must reuse without re-verifying it** — a binary location from `which`, a socket/PID, a generated file path, etc. What one worker discovered in its own execution context is not guaranteed to resolve the same way for a different worker (different sandbox, different session, or the underlying state may simply have changed since). If a later task needs that same fact, let its worker rediscover it itself; never instruct a worker not to verify a path you are handing it. Confirmed live 2026-08-07: a coordinator hardcoded a `trec` binary path an earlier worker had discovered via `which trec` into a later task's goal text and told that worker not to re-run `which trec` — the literal path did not exist in the later worker's execution context (exit 127), failing the task on a stale coordinator-cached fact a 5-second rediscovery would have avoided.\n")
	if !c.coordinatorToolDenied("stm_write") && !c.coordinatorToolDenied("ltm_update") {
		b.WriteString("- stm_write after each meaningful agent result (# 發現 / # 決策 / # 錯誤與修復 / # 待解決) AND before finish. ltm_update for cross-session knowledge.\n\n")
	}

	return b.String()
}

// filterDeniedPromptLines removes coordinator instructions for tools that the
// team policy has denied. Denied names must not be replaced with a synthetic
// phrase: models can still interpret that phrase as a callable tool name.
func (c *Coordinator) filterDeniedPromptLines(prompt string) string {
	if c == nil || c.session == nil || len(c.session.Config.ToolsDenied) == 0 {
		return prompt
	}
	denied := make([]string, 0, len(c.session.Config.ToolsDenied))
	for _, name := range c.session.Config.ToolsDenied {
		if name = strings.TrimSpace(name); name != "" {
			denied = append(denied, name)
		}
	}
	if len(denied) == 0 {
		return prompt
	}

	lines := strings.Split(prompt, "\n")
	filtered := lines[:0]
	for _, line := range lines {
		remove := false
		for _, name := range denied {
			if strings.Contains(line, name) {
				remove = true
				break
			}
		}
		if !remove {
			filtered = append(filtered, line)
		}
	}
	return strings.Join(filtered, "\n")
}

func (c *Coordinator) GetOrchestratorDef() *agent.AgentDef {
	for _, def := range c.session.Agents {
		if def.Role == "coordinator" || def.Role == "orchestrator" {
			defCopy := *def
			c.ensureModelFallback(&defCopy)
			return &defCopy
		}
	}
	for _, def := range c.session.Agents {
		n := strings.ToLower(def.Name)
		if strings.Contains(n, "coordinat") || strings.Contains(n, "orchestr") {
			defCopy := *def
			c.ensureModelFallback(&defCopy)
			return &defCopy
		}
	}
	def := &agent.AgentDef{
		Name:        "coordinator",
		Description: "Default team coordinator",
		Role:        "coordinator",
		Tools:       "ask_user",
		System:      "",
		MaxRetries:  -1,
		Generation:  c.session.Config.Generation,
		ProviderURL: c.session.Config.ProviderURL,
	}
	c.ensureModelFallback(def)
	return def
}

func (c *Coordinator) ensureModelFallback(def *agent.AgentDef) {
	if def.Generation.Model == "" && c.session.Config.Generation.Model != "" {
		def.Generation.Model = c.session.Config.Generation.Model
	}
	if def.ProviderURL == "" && c.session.Config.ProviderURL != "" {
		def.ProviderURL = c.session.Config.ProviderURL
	}
}

func (c *Coordinator) expandOrchestratorTemplate(tmpl string) string {
	workerNames := c.workerNameList()

	// Use session.Config.Vars as base (contains team.yaml vars + CLI --var + built-in)
	vars := make(map[string]string)
	if c.session.Config.Vars != nil {
		for k, v := range c.session.Config.Vars {
			vars[k] = fmt.Sprintf("%v", v)
		}
	}

	// Ensure built-in vars exist (fallback if not in config.Vars)
	if _, ok := vars["TEAM_NAME"]; !ok {
		vars["TEAM_NAME"] = c.session.Config.Name
	}
	if _, ok := vars["AGENT_COUNT"]; !ok {
		vars["AGENT_COUNT"] = fmt.Sprintf("%d", len(workerNames))
	}
	if _, ok := vars["AGENT_NAMES"]; !ok {
		vars["AGENT_NAMES"] = strings.Join(workerNames, ", ")
	}

	result, err := applyTemplate(tmpl, "orchestrator-system", vars)
	if err != nil {
		s := strings.ReplaceAll(tmpl, "{@ .TEAM_NAME @}", c.session.Config.Name)
		s = strings.ReplaceAll(s, "{@ .AGENT_COUNT @}", fmt.Sprintf("%d", len(workerNames)))
		s = strings.ReplaceAll(s, "{@ .AGENT_NAMES @}", strings.Join(workerNames, ", "))
		return s
	}
	return result
}

const defaultOrchestratorSystem = `You are the orchestrator of "{@ .TEAM_NAME @}", a software development team with {@ .AGENT_COUNT @} members: {@ .AGENT_NAMES @}.

Your role is to coordinate the team: break down user requests into concrete tasks, delegate them to the right members, and synthesize the results into a coherent response.

Rules:
- You MUST use agent to delegate ALL work to team members
- Running independent tasks in parallel is preferred
- After receiving results from agent, evaluate whether more work is needed or if you can provide a final answer
- Never redispatch a worker whose task is already successful. To read its full result, use team_info with action=task_result; do not use agent as a result-retrieval mechanism.
- Synthesize results from workers into a coherent answer for the user
- NEVER attempt to do the work yourself — you do not have tools for that
- If a task fails, provide clearer GOALS or add missing CONSTRAINTS — do NOT add implementation details
- Break complex requests into smaller subtasks for appropriate workers
- Use ask_user when you need clarification from the user before proceeding
- When you have completed all coordination and have a final answer, call the finish tool with your response
- ALWAYS call finish when done — do not just output text as your final answer
- If the user's task relates to a skill, use load_skill to get the detailed instructions. Include the skill name and file path in worker task descriptions so workers can load it themselves if needed
- Workers have access to load_skill — include the skill name and path in the task description rather than the full skill content
- When an agent result says VERBATIM TRANSCRIPT CAPTURED, treat its artifact manifest as authoritative evidence. If its contents are required, pass artifact_ref unchanged to view; never reconstruct or copy a filesystem path. Otherwise report the opaque reference and integrity metadata without re-reading it.

Delegation Guidelines:
- Break down user requests into outcome-oriented goals for each worker
- Describe WHAT to achieve (goal), not HOW to achieve it
- Only specify constraints that are non-obvious or user-mandated (e.g., "must not break public API")
- Workers will determine file paths, tool selection, and implementation approach based on the goal
`

const continuationPromptTemplate = `The user has sent an additional message while you were working:

"""
%s
"""

Please take this into account. You may need to:
- Add new tasks for your workers
- Modify tasks that haven't started yet
- Cancel tasks that are no longer needed

Continue coordinating. Call finish when you have a complete response that addresses both the original request and the new input.`

const stepLimitWrapUpPrompt = `Your previous turn stopped because the per-turn step limit was reached before you called finish.

Your full progress so far is above, including any tool results you had not yet acted on.

- Review the latest results first — the work may already be complete
- Do NOT delegate new tasks
- Call the finish tool NOW with an honest summary: what was accomplished, and what (if anything) remains
- If work remains, state it explicitly in the response; the user can type "continue" to resume`

const wrapUpPromptTemplate = `The user has requested that you wrap up immediately.

IMPORTANT INSTRUCTIONS:
- Do NOT delegate any new tasks
- Do NOT call agent again
- Immediately summarize what has been accomplished so far based on all results you have received
- Call the finish tool RIGHT NOW with your best summary of the work completed

This is a wrap-up request. You MUST call finish immediately with whatever results are available.`
