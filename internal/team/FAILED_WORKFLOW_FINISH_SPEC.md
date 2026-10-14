# Failed workflow termination

Behavioral checklist:

- [x] A failed workflow refuses successful finish and new delegation.
- [x] Explicit `acknowledge_failed_tasks` permits canonical partial termination
  after workers settle and terminal sessions close.
- [x] Partial termination preserves task failures, failed acceptance, nonzero exit
  status, and cancelled or blocked dispositions; it never claims goal completion.
- [x] The final response uses runtime task evidence rather than unaudited model prose.
- [x] Terminal finalization publishes one canonical `run_finished` event.
- [x] Recovery guidance names only existing verified replacements preserving the
  failed task's contract, otherwise directs the coordinator to partial finish.
- [x] Acknowledgment does not reopen dispatch, self-healing, or rollback.

Regression coverage: `coordinator_partial_finish_test.go` and
`coordinator_failed_workflow_test.go`.
