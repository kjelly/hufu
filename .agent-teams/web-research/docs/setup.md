# Web research prerequisites

The browser MCP servers are pinned to `@playwright/mcp@0.0.84`. Install the
matching browser before starting research; an older cached Chromium revision
does not satisfy the MCP package's executable requirement.

Full team runs also require `OLLAMA_API_KEY` in the launching environment for
the hosted Ollama `web_search` service.

```bash
task_dir=$(mktemp -d /tmp/codex-task.XXXXXX)
trap 'rm -rf "$task_dir"' EXIT
TMPDIR="$task_dir" PLAYWRIGHT_SKIP_BROWSER_GC=1 \
  npx -y @playwright/mcp@0.0.84 install-browser chrome-for-testing
TMPDIR="$task_dir" python3 .agent-teams/web-research/tests/browser_smoke.py
```

The manual smoke test starts each of the three isolated MCP contexts, navigates
to `https://example.com/`, and reads a separate `browser_snapshot` with `{}`.
It verifies both the observed URL and the actual documentation-example paragraph
inside the DOM snapshot, then closes the client and browser processes.
It needs network access and installed browser
dependencies, and is separate from the offline Go tests.

Researcher, countercheck, and verifier use `result-transport: runtime`: they
return their complete JSON work report, and the runtime collects and validates
typed claims without repeating research. The collector's provider response
function carries data only; it grants no executable tools. Its exact arguments
still undergo schema, evidence, receipt, and acceptance validation. An endpoint
that ignores the response-function request must still return valid JSON or use
the bounded format-repair budget; prose is never extracted as accepted claims.

The synthesizer deliberately uses `result-transport: tool` and the closed
`[submit_result]` sequence. This preserves its existing result-only tool boundary
without granting browsing, file access, or delegation.

Browser installation fixes future page access. Result-only repair preserves
old observations and cannot acquire pages that the failed run never opened.
Changed team instructions are policy changes: start a fresh session or assess
supported contract recovery rather than editing saved receipts or checkpoints.
