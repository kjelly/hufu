# Saved result identity display

## Behavioral checklist

- [x] A result-ready message displays its result ID, team, and read command in the matching task detail.
- [x] Receiving the message does not change the task status or any coordinator state.
- [x] Updating a result identity does not mutate a previously returned Model's identity map.
- [x] The reference is available in task logs for the existing selection/copy flow.
- [x] Labels are sanitized and wrap within the terminal width.

The identity is supplied after the canonical event commit. The TUI does not
allocate IDs or infer that the task completed from model output.
