# Saved final run responses

- [x] `results final --team <team>` reads the newest canonical run response without a run-id or model call.
- [x] `results final <result-id>` follows the selected task's workspace, team, session, run and branch identities.
- [x] An unfinished run does not fall back to another run, session, workspace, team or sibling branch's response.
- [x] Ambiguous cross-branch responses require explicit branch selection.
- [x] `show` retains the exact worker output and labels it as an individual agent task result, with a final-response locator.
- [x] JSON retains canonical result identity and distinguishes task from run_response.
- [x] Explicit caller filters cannot be broadened by an anchor; worker-only filters and pagination are refused for final.
- [x] A run_response locator remains an exact lookup; response content and outcome are not rewritten or synthesized.
