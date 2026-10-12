# Chat-first TUI

> Status: active
> Authority: normative
> Verified-Commit: `feat/chat-first-tui` (P3)
> Supersedes: [chat-first TUI P0–P3 implementation plan](../archive/implementation-plans/chat-first-tui-p0-p3.md)
> Superseded-By: —

`hufu chat --ui chat` is a full-screen conversation with one loaded team. It
keeps the team, its MCP servers, and its workspace lease for the whole
session; every message is one turn of the same coordinator conversation.
The legacy line REPL (`--ui plain`, the default) and the task dashboard
(`--ui dashboard`, alias `--tui`) are unchanged.

## Front ends

| `--ui` | What it is | Requires a TTY |
|---|---|---|
| `plain` (default) | line REPL with readline completion | no |
| `dashboard` (`--tui`) | task dashboard with an "additional prompt" dialog | yes |
| `chat` | chat-first TUI described here | yes; otherwise the command fails with a hint to use `--ui plain` |

`--tui` together with `--ui chat` or `--ui plain` is a usage error. The chat
command also accepts `--theme`, `--display-preset`, and `--no-spinner`.

## Architecture

```text
cmd/hufu/chat.go          runChat → selectChatUIMode → chatSession.run{Plain,Dashboard,ChatUI}
cmd/hufu/chat_ui.go       tea.Program(ChatModel) ⇄ conversation.Controller
cmd/hufu/chat_runtime_adapter.go   RuntimePort over the loaded teamContext
cmd/hufu/chat_turn.go     runChatTurn: the single turn implementation (plain REPL and chat TUI)
internal/conversation     Controller: admission, bounded queue, per-turn context, events
internal/tui/chat_*.go    ChatModel: composer, transcript, progress cards, overlays
```

Status events follow the existing path (`newCoordDisplay` → `makeTUIReporter`
→ `tea.Program.Send`) and reach the chat view through the embedded task
dashboard. The controller never handles status events.

### Turn runner

`runChatTurn` prepares the prompt (`#file` injection, project context), binds
a fresh invocation ID, calls `Coordinator.Run` for the first turn of the
process and `ContinueWithPrompt` afterwards, classifies exploratory delivery,
persists the session after a successful turn, and reports the run identities.
A cancelled turn counts as a turn (it established coordinator history); a turn
that failed for another reason does not. `/reset` and `/team` restart the
sequence.

### Controller

`conversation.Controller` admits commands without blocking and answers each
with exactly one `Ack`:

| Command | Admitted when | Otherwise |
|---|---|---|
| `submit_prompt` | idle → `accepted`; running → `queued` (capacity 16, max 64) | `busy` when the queue is full; `rejected` for empty, invalid UTF-8, or > 256 KiB |
| `cancel_turn`, `wrap_up` | a turn is running | `rejected: no active turn` |
| `reset_context`, `switch_team` | idle with an empty queue | `rejected: busy …` |

Resubmitting a command ID returns the original `Ack`; admission is in-memory
and not durable. Each turn runs under its own `context.WithCancel` of the
session context, so cancelling a turn never ends the session. Events
(`TurnStarted`, `TurnSettled`, `ControlDone`, `QueueDropped`, `Closed`) are
emitted in order; the actor is released before a result is announced, so a
command sent in reaction to the event is admitted as idle. `Close` drops queued
prompts (reported, never run), cancels the active turn, and waits for it up to
the graceful wrap-up timeout; on timeout the caller runs emergency
finalization.

## Data authority

| Shown | Source |
|---|---|
| Final answer | return value of `Run` / `ContinueWithPrompt` after exploratory-delivery classification |
| Outcome label | the `RunResult` this turn published (`LastRunResult()` changed); otherwise `no result` — an earlier turn's result is never reused |
| Progress cards | the canonical task projection (`TasksUpdatedMsg`): tasks new or changed since the turn started, one row each with agent, status, execution target, task ID, the task description, and its latest activity; frozen when the turn settles |
| Elapsed time and activity | `TurnStarted.StartedAt` and `TurnResult.Elapsed` from the controller clock; the header shows `running <elapsed>` (refreshed by a per-turn tick: 1 s, 15 s on e-paper), the status line the latest runtime status (`⟳ reviewer step 3`), and the final answer title the turn's duration |
| Turn summary | after a turn settles, one line at the bottom from the frozen card: outcome, status counts, and up to three failed tasks by ID, contract (or description), and agent — visible even when a long answer pushes the card out of view |
| Sidebar models | the runtime's precedence: the coordinator runs on the resolved coordinator target; a worker on its own model, else the team route, else the resolved worker default |
| Tool activity | `TaskLogMsg` lines built from already-redacted `StatusEvent`s, sanitized again before rendering |
| Restored history | `session.json` entries loaded by the normal resume path, marked `restored` |

The view never writes task state, never synthesizes "verified", and never runs
recovery: an unresolved previous run is reported with a pointer to
`hufu session status` / `hufu resume`.

## Keys

| Where | Key | Action |
|---|---|---|
| Composer | `Enter` | send (the draft stays until the controller admits it) |
| Composer | `Ctrl+J`, `Alt+Enter`, `Shift+Enter`* | newline |
| Composer | `Tab` | complete `/commands` (first token) and `@names` |
| Composer | `Esc` | focus the transcript |
| Transcript | `j`/`k`, arrows, `PgUp`/`PgDn`, `Ctrl+U`/`Ctrl+D` | scroll (bubbles viewport key map) |
| Transcript | `g`/`G` | top / bottom (bottom resumes following new content) |
| Transcript | `n`/`p`, `Enter`, `Esc` | select a progress-card row, open its task detail, clear the selection |
| Transcript | `i`, `a` | back to the composer |
| Transcript | `T` | cycle the theme |
| Anywhere | `Ctrl+T` | chat ↔ tasks |
| Anywhere | `Ctrl+P` | command palette (inserts, never runs) |
| Running turn | `Ctrl+C` | cancel the turn; again while cancelling: confirm quit |
| Idle | `Ctrl+C` | clear the draft; on an empty composer: confirm quit |
| ask_user | dialog keys | the dialog owns every key until answered |

\* `Shift+Enter` needs a terminal with key disambiguation; Bubble Tea v2
requests it by default.

Slash commands: a first token shaped `/word` is a command (`/help`, `/agents`,
`/skills`, `/config`, `/status`, `/tasks`, `/cost`, `/theme [mode]`,
`/save <path>`, `/cancel`, `/wrap`, `/reset`, `/team [name]`, `/exit`,
`/quit`). An unknown `/word` is refused with a hint; `//text` sends `/text`
literally; anything else (for example `/home/x is broken`) is a prompt.

## Layout and display

From 110 columns a sidebar lists agents and task counts; from 40 columns a
header and a hint line are shown; below that only the transcript, the
composer, and one status line remain. The composer grows with its content up
to 6, 3, or 1 rows depending on terminal height. Messages are plain text:
prose is word-wrapped, fenced code is only hard-wrapped, and all control
sequences are removed. Themes reuse the dashboard's palette and detection; the
`epaper` preset renders at 1 FPS with no spinner and no cursor blinking.

## Limitations

- The final answer is shown when the turn settles; it is not streamed token
  by token.
- Markdown is not rendered.
- Command admission is not durable across process restarts; there is no
  remote client.
