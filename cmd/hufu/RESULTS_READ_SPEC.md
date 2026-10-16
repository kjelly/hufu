# Reprint the complete final response

- [x] `result` is an explicit Cobra alias of `results`, including normal team/workspace resolution.
- [x] With no subcommand, text output is the latest selected run's complete saved response body and one CLI newline.
- [x] No metadata, preview, history instructions, worker receipts, old response, summary or model synthesis is added to that body.
- [x] Long multiline bodies remain complete; terminal controls and existing redaction remain protected.
- [x] Explicit `final [result-id]` reprints the selected final response body as well.
- [x] Missing saved content fails clearly without printing unrelated worker output or a diagnostic reason as the answer.
- [x] JSON keeps the historical envelope with the complete selected run_response and provenance.
- [x] `runs` exposes the paginated overview previously assigned to the no-subcommand path.
- [x] Worker and pagination filters on the default reader require their appropriate explicit subcommands.
- [x] Team-specific response bodies are not claimed to reproduce multi-team aggregation, CLI headings or execution statistics that were not saved.
