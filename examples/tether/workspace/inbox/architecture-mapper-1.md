# Task for architecture-mapper (run 1)

**Started:** 2026-04-24T14:22:12Z

## Task

Analyze the architecture of /home/ubuntu/no-changed-github/kit/agent-teams.go. Focus on:
1. Overall module structure — what are the major sections and their responsibilities?
2. Type hierarchy — how do teamConfig, agentDef, agentState, teamSession relate?
3. Entry point (Init function) — what does it register and in what order?
4. How does the extension plug into Kit's lifecycle (which events does it hook)?
5. Concurrency model — what mutexes protect what, and how do goroutines coordinate?
6. File-based communication pattern — inbox/outbox/shared design.

Produce a structured architecture map in Chinese (Traditional).
