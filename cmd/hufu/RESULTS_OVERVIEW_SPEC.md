# Saved run overview

- [x] `results runs --team <team>` lists saved final run responses, not worker attempts or verification receipts.
- [x] Overview text omits workspace/index/internal task constraints, and explains the worker-list and older-session commands.
- [x] Each response has a timestamp, team, outcome, bounded literal excerpt and exact reading command.
- [x] The preview is at most three nonempty lines of 140 characters, explicitly labeled as an excerpt.
- [x] Empty history explains that worker results can exist before a saved final response.
- [x] JSON preserves the canonical response, coverage and identity without trimming saved content.
- [x] Explicit `list` retains the existing worker-result contract.
- [x] Overview pagination only includes final responses, using the existing scope/head-bound cursor.
- [x] Reading commands preserve explicit workspace and branch scope; registry defaults omit redundant storage arguments.
- [x] Worker-only flags require explicit `list`; no natural-language meaning is guessed and no LLM is called.
