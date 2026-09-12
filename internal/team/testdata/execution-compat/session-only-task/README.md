# session-only-task

Producer: fixed legacy fixture builder v1 (pre canonical execution identity).

Case 13: the task exists only in the active session snapshot. A single
self-contained migration event must restore it without a synthetic legacy
task event.
