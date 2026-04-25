# Task for logic-analyzer (run 1)

**Started:** 2026-04-24T14:22:12Z

## Task

Deep-dive into the core execution flow of /home/ubuntu/no-changed-github/kit/agent-teams.go. Trace these key paths:

1. **Team activation flow**: How does a team get loaded? Trace from /team command, AGENT_TEAM env var, and //team: prefix — what are the differences?

2. **run_agents tool execution**: Trace the full path from LLM calling run_agents → parallel goroutine spawning → runSingleAgent → kit subprocess → result collection. How does retry logic work? How are timeouts handled?

3. **Streaming UI updates**: How does the real-time detail column work? Trace the pipe reader goroutine, ticker goroutine, and how they update agentState.

4. **Orchestrator prompt injection**: How does OnBeforeAgentStart work? What does buildOrchestratorPrompt compose?

5. **Error handling**: What happens when an agent fails? Trace the OnToolResult handler and the abort mechanism.

Produce a detailed execution flow analysis in Chinese (Traditional).
