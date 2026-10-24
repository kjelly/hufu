# Dashboard resize regression contract

These existing behaviors implement the Responsive Layout and Defensive Rendering
principles in [CONSTITUTION.md](CONSTITUTION.md).

## Behavioral checklist

- [x] WindowSizeMsg updates both dimensions and initializes the viewport.
- [x] Supported narrow widths show the terminal-width warning.
- [x] Returning to a wide terminal restores the prompt and removes the warning.
- [x] Tiny dimensions and successive size changes render without panicking.
- [x] A real PTY delivers resize events after provider and screen readiness;
  the child remains responsive to SIGINT and leaves the alternate screen.

The dimension matrix lives in `resize_contract_test.go`; the Linux PTY lifecycle
lives in `cmd/hufu/pty_e2e_test.go`. See the [validation record](../../docs/archive/analyses/pty-tests-2026-10-22.md).
