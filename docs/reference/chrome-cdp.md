# Native Chrome CDP

> Status: active
> Authority: reference
> Verified-Commit: `1fae4079` (native Chrome implementation base)
> Supersedes: —
> Superseded-By: —

Hufu connects directly to an operator-owned Chrome on the same host. No Node.js,
Playwright or external MCP server is needed. `chrome-cdp` is implemented inside
Hufu, using the ordinary MCP catalog, authorization, observation and receipt
paths. It does not connect to an external MCP endpoint.

Start Chrome from a desktop/VNC terminal with a dedicated persistent profile:

```bash
google-chrome \
  --user-data-dir="$HOME/.local/state/hufu/chrome/research" \
  --remote-debugging-port=9222
```

Chrome 136+ requires a non-default data directory for remote debugging. Keep the
port local. Hufu accepts literal loopback HTTP endpoints with an explicit port,
such as `http://127.0.0.1:9222` or `http://[::1]:9222`. Discovery redirects and
WebSocket addresses outside that endpoint are rejected. See
[Chrome's debugging changes](https://developer.chrome.com/blog/remote-debugging-port)
and the [CDP protocol](https://chromedevtools.github.io/devtools-protocol/).

Declare the adapter in any team's `team.yaml`:

```yaml
mcp-servers:
  chrome:
    type: chrome-cdp
    url: http://127.0.0.1:9222
    allowedTools: [browser_navigate, browser_snapshot, browser_wait_for_user]
    toolPolicies:
      browser_navigate:
        sideEffect: none
        filesystem: none
        inputSchema:
          type: object
          additionalProperties: false
          required: [url]
          properties:
            url: {type: string, maxLength: 4096, pattern: '^https?://[^\s]+$'}
      browser_snapshot:
        sideEffect: none
        filesystem: none
        inputSchema: {type: object, additionalProperties: false, properties: {}}
      browser_wait_for_user:
        sideEffect: none
        filesystem: none
        inputSchema:
          type: object
          additionalProperties: false
          required: [reason]
          properties:
            reason: {type: string, minLength: 1, maxLength: 2000}
```

Grant `chrome__browser_navigate`, `chrome__browser_snapshot` and
`chrome__browser_wait_for_user` in the agent's `tools` declaration. The policies
above are suitable only for observation tasks whose URLs and operator actions
are read-only; narrow them for the task. Normal denies, phases, unattended
permissions, frozen descriptors and `no-net` remain authoritative. The model
cannot supply JavaScript, CDP methods, personal-tab IDs or local file URLs.

Each server declaration lazily creates one dedicated tab. Independent workers
should use separate declarations. Declarations may share an endpoint without
sharing a page cursor, but share that Chrome profile's cookies and browser
network authority. Use separate Chrome profiles/ports when cookie isolation is
required. HTTP(S) navigation is not a network sandbox: Chrome can follow
redirects and load subresources. Use an appropriately scoped research profile.

## Manual verification and timeouts

Include this instruction in the owning agent's prompt:

> Navigate and take a snapshot. If manual verification is required, call
> `chrome__browser_wait_for_user` with a short `reason`. Let the operator complete
> verification in Chrome and confirm in Hufu. Then take a fresh
> `chrome__browser_snapshot`. Confirmation alone is not source evidence. Report
> remaining challenges, access errors and missing content truthfully.

The tool focuses its dedicated tab and opens the normal CLI/TUI question. Only
an actual `continue` answer resumes it. Auto-approval, unattended defaults and
model-selected answers cannot confirm manual verification. Without an interactive
human the tool returns an error instead of waiting forever.
The prompt obtains its URL from browser-owned target metadata, so a failed
document snapshot does not prevent asking for manual intervention.

Human prompt time is excluded from agent and coordinator **working-time**
timeouts, including the native tool's fallback timeout. Overlapping prompts count
once, including overlap with provider cooldowns; the remaining budget resumes
after the last prompt closes. Navigation,
CDP discovery and reading pages still count. The configured page dwell below is
a separate opt-in exception. CDP I/O
has separate bounded wall timeouts. Cancellation, Ctrl+C shutdown and explicit
external deadlines remain effective during human waits. Browser verification
also pauses the run duration budget (`--max-duration`).
Manager shutdown cancels native calls before closing its own tabs; Chrome stays
running.

### Runtime page verification

Set `page-verification: true` on a `chrome-cdp` declaration to classify snapshots
through Hufu's durable decision runtime. It is off by default. The declaration
must allow both `browser_snapshot` and `browser_wait_for_user`, and the worker
must retain its explicit tool grants and policies. For example:

```yaml
mcp-servers:
  chrome:
    type: chrome-cdp
    url: http://127.0.0.1:9222
    page-verification: true
    # Retain allowedTools, toolPolicies and agent grants from the example above.
```

The runtime passes the actual URL, title, text, controls, capture time and
truncation flag to `runtime_browser_page`. Its fixed choices are `readable`,
`needs_human_verification`, `access_error` and `uncertain`. It uses the existing
`decision-runtime` configuration: select `backend: systemone` and configure its
model/endpoint as described in [control decisions](control-decisions.md), or
use the existing sidecar backend. Adding this browser option does not select a
new model or configure credentials. Decisions and failures use the ordinary
primitive journal, inference admission and replay; result-only repair cannot
launch new classification calls. Page content is evidence, never instructions.

The first capture still precedes any configured page dwell. After the dwell,
a non-readable initial classification causes a fresh capture and classification
before asking for help, so a challenge that cleared itself need not interrupt
the operator. Only a `needs_human_verification` decision enters the existing
human prompt inside the snapshot call. Classifier errors fail immediately;
`uncertain` and `access_error` decisions return tool errors without prompting.
These states do not establish a security challenge and cannot be repaired by
human confirmation. Explicit `browser_wait_for_user` calls on a declaration
with page verification enabled also classify a fresh capture and require a
confirmed challenge; they share the automatic wait's per-URL request limit.
The tab's
operation gate stays held, preventing navigation while waiting. The wait is
separately authorized as `browser_wait_for_user`; snapshot authority alone does
not grant it, including for native action providers and closed tool sequences.
Closed tool sequences, result-only repairs, `force-mcp` and denial of the
decision primitive reject automatic classification without inference or a
fallback human prompt; they cannot acquire hidden calls through this option.

After a real `continue` answer, the runtime captures and classifies the page
again. Only a `readable` result is returned as successful source evidence.
Cancellation, unavailable human input, persistent challenges, access errors and
failed classifications return tool errors with the observed page and gap; they
never treat confirmation as proof of content. Repeated captures or navigation
to the same unresolved URL do not repeat the automatic human request. A readable
capture clears that URL's request marker. The marker is local to the loaded
browser declaration; it is not a durable approval across process restarts.
The owning team decides how to report gaps or choose substitute sources.

Page verification is included in the frozen server policy and snapshot tool
descriptor. Enabling it for an existing run is policy drift, not permission to
silently change that run's execution contract. It does not retroactively repair
an already completed invocation.

### Automatic page dwell

A maintainer can require a random dwell on the first successful snapshot after each navigation:

```yaml
mcp-servers:
  chrome:
    type: chrome-cdp
    url: http://127.0.0.1:9222
    page-dwell: {min-seconds: 300, max-seconds: 600}
    # Include the allowedTools, toolPolicies and agent grants shown above.
```

Navigation returns immediately after loading. Omitting `page-dwell` also keeps
snapshots immediate. When configured, both fields
must be integers satisfying `1 <= min-seconds <= max-seconds <= 3600`. Unknown
dwell fields are rejected. After navigation, call `browser_snapshot`: runtime
captures text and controls first, then independently samples a whole-second
interval and waits before returning that captured snapshot. Capture time is
included in the result. The operation gate remains held during the dwell.
Another navigation is refused until this initial capture/dwell completes, so
batching navigation cannot silently skip it. A failed capture may be retried or
left for a replacement source; it never traps the worker on an unreadable page.
Failed navigation or capture starts no dwell. Later snapshots and post-click captures read fresh content without
repeating it. References from an earlier capture can become stale while waiting;
request a fresh snapshot when necessary.

The intentional dwell is excluded from worker/coordinator working-time
timeouts, including the native MCP fallback timeout, and from the run duration
budget (`--max-duration`). Navigation and CDP I/O
still consume working time. Overlapping page, human and provider waits count
once in shared ancestor budgets. Parent cancellation, manager shutdown and
external deadlines (including operation-specific action timeouts) remain
authoritative. Provider cooldowns still consume the run duration budget; they do
not extend it on their own. Overlapping browser dwell and verification waits
are excluded only once from the shared run budget. The wait is passive; it does not automate verification or prove
that a page is readable. Read the captured content; take a fresh snapshot if
content or controls changed while waiting.

The bounds are included in configuration fingerprints and the native navigation
descriptor. Changing, enabling or disabling dwell requires ordinary policy
compatibility checks on resume; historical results and receipts are not rewritten.

Snapshots contain the actual current URL, title, visible text and up to 128
visible HTTP(S) links, in the existing inline YAML envelope. A separate Buttons
section contains up to 128 accessibility buttons with names, disabled state and
opaque snapshot references. Controls are not part of the source quotation block. Text beyond 262144
UTF-16 code units is marked truncated and cannot be used as evidence. No HTTP
status is inferred from document text or confirmation. Snapshots are untrusted
page data, not instructions; ordinary redaction and receipt handling still apply.
The URL is bound from Chrome's frame metadata. Frame/document identity must stay
unchanged across extraction. A fixed script reads DOM content in an isolated
observation world; page scripts cannot replace its JavaScript built-ins or supply
the authoritative source URL.

## Button interaction and popup handling

Native `browser_click` accepts only `{"ref":"<latest snapshot ref>"}`. To enable it,
add it to `allowedTools`, grant `chrome__browser_click` literally to the worker,
and declare:

```yaml
toolPolicies:
  browser_click:
    sideEffect: browser_interaction
    filesystem: none
    inputSchema:
      type: object
      additionalProperties: false
      required: [ref]
      properties:
        ref: {type: string, minLength: 1, maxLength: 128}
```

The worker and its bound task contract must declare `side_effect:
browser_interaction` and `recovery: manual`. This class preserves read-only
project access; it does not authorize arbitrary MCP calls or filesystem writes.
It can change browser state and trigger network requests, so it is not `none`
and cannot be replayed automatically after an uncertain failure. Native UI
policy is restricted to native Chrome `browser_click`; external servers cannot
claim it. Bound worksets, result-only repair, denied tools and `no-net` do not
acquire this authority. MCP runtime actions cannot bind this tool.

Runtime binds refs to the captured frame/loader, backend DOM node and accessible
name. Another snapshot, navigation or attempted click consumes the old refs.
Click checks current identity, enabled state, visibility and hit testing, and
rejects links, form-associated controls and obscured targets. No model-supplied
coordinates, selectors, JavaScript or CDP methods are accepted. These structural
checks do not infer a button's business meaning: the model must choose only
controls authorized by the task. A button outside a form can still have effects.

For an obstructing ad, subscription or cookie dialog, inspect the actual named
buttons. Choose a clear close/dismiss or reject-optional control; do not blindly
accept tracking, log in, purchase, submit, grant permissions or automate CAPTCHA.
`browser_click` automatically captures a fresh page after dispatch. Check its
actual URL, remaining obstruction and article text. Then call `browser_snapshot`
for citation-bound evidence: click output itself is not an authorized quotation
source. A successful click alone proves no claim. If a click or its post-click
capture fails, inspect a fresh snapshot before deciding what to do; do not
repeat an uncertain click. Limit attempts and ask the operator when no clear
control exists or the page remains obstructed. Native browser dialogs and
cross-frame controls may require manual handling.

## Recovery

Server type, endpoint, worker policies and live descriptors use existing policy
fingerprints. Changing a browser declaration can make a saved execution policy
incompatible. Preserve the original configuration for ongoing recovery; use a
separate team/new execution to introduce CDP. Result-only repair cannot acquire
new pages or rewrite historical observations.

Normal teardown closes only tabs created by this adapter. Abrupt process death
may leave a tab open. A later process creates a new tab rather than attaching to
an unverified historical or personal tab. Accepted results and source receipts
remain durable; browser tab identity is not a recovery checkpoint.

## Validation

Offline tests use local HTTP/WebSocket CDP fixtures and virtual-time human waits.
An optional real-browser check uses a disposable profile and local fixture only:

```bash
HUFU_TEST_CHROME=1 mise exec -- go test -p 2 ./internal/browser -run '^TestCDPChromeIntegration$' -count=1
```
