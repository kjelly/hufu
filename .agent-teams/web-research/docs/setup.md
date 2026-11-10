# Web research prerequisites

Use a Hufu build with native `chrome-cdp`, button interaction and capture-before-dwell support. Start a visible Chrome from
this Ubuntu host's desktop/VNC terminal before running research:

```bash
google-chrome \
  --user-data-dir="$HOME/.local/state/hufu/chrome/web-research" \
  --remote-debugging-port=9222
```

The three declarations connect to `http://127.0.0.1:9222`; each owns a dedicated
tab. Cookies are shared within this research profile. Use public sources only;
do not attach a personal authenticated profile. Chrome must keep running while
the team works. Hufu focuses the relevant tab when manual verification is needed;
match the URL shown in the CLI/TUI question to the correct Chrome tab, complete
verification there, and answer the prompt. Parallel workers may change which tab
is in front. Human prompt time does not
consume worker/coordinator working-time timeout or the run duration budget
(`--max-duration`). Ctrl+C and external operation deadlines still apply. Unattended execution cannot confirm verification.

Each of the three browser declarations enables
`page-dwell: {min-seconds: 300, max-seconds: 600}`. After a successful navigation,
navigation returns after loading; the first `browser_snapshot` captures text
and controls immediately, then keeps the page open for a newly sampled 5–10
minutes before returning that captured content. Later snapshots read fresh
content without another dwell. This passive dwell does
not consume agent/coordinator working-time timeout or the run duration budget
(`--max-duration`). The model cannot shorten
it through tool arguments or add a second reading wait. Cancellation and
external operation deadlines still apply. A completed dwell is not source evidence.

Full research runs still require `OLLAMA_API_KEY` for hosted Ollama `web_search`.
The browser itself uses native Go CDP; Node.js and Playwright are not required.
The `playwright_<role>` logical identifiers are retained for evidence contract
compatibility and are not external MCP subprocesses.

Offline role-admission checks load the real native tool catalogs without opening
Chrome. An optional driver smoke test launches a disposable headless Chrome and
a local fixture, reads real snapshots and checks external DOM changes. It does
not use the research profile, model credentials or public research websites:

```bash
task_dir=$(mktemp -d /tmp/codex-task.XXXXXX)
trap 'rm -rf "$task_dir"' EXIT
TMPDIR="$task_dir" HUFU_TEST_CHROME=1 mise exec -- go test -p 2 ./internal/browser -run '^TestCDPChromeIntegration$' -count=1
```

For full runtime configuration and timeout semantics, see
[native Chrome CDP](../../../docs/reference/chrome-cdp.md).

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

The Chrome CDP configuration applies to new executions. Result-only repair preserves
old observations and cannot acquire pages that the failed run never opened.
Changed team instructions are policy changes: start a fresh session or assess
supported contract recovery rather than editing saved receipts or checkpoints.

Browser workers and their static contracts use `side_effect: browser_interaction`
and `recovery: manual`; synthesis remains read-only. Named button refs permit
bounded popup dismissal through `browser_click`, which returns a fresh post-click
capture. Workers must inspect it and obtain `browser_snapshot` citation evidence.
References expire after another snapshot, navigation or click. No arbitrary
selectors, scripts, links or form submission are admitted. Semantic button choice
belongs to the worker, with manual intervention for ambiguous controls. These
changed grants/contracts are frozen policy: do not apply them by editing a live
historical execution or its checkpoints.
