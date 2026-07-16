# Shell-Use Benchmark Report for hufu

**Date:** 2026-07-12
**Tool:** [shell-use](https://github.com/microsoft/shell-use) (Python PTY-based implementation)
**Target:** hufu CLI (`./hufu`)
**Total Tests:** 58
**Passed:** 58
**Failed:** 0
**Pass Rate:** 100.0%

## Overview

This report documents a comprehensive testing session of the hufu CLI tool using the
shell-use testing methodology. Shell-use is a Microsoft CLI tool for controlling,
inspecting, testing, and recording shell sessions and terminal applications. It
provides commands to open PTY sessions, submit commands, wait for conditions, and
assert on output — enabling black-box integration testing through a real terminal.

Since the Rust shell-use binary could not be built (network restrictions prevented
downloading crates), a compatible Python implementation was created that follows
the same command interface (`open`, `submit`, `wait`, `expect`, `close`, etc.)
using Python's `pty` module. This implementation drives hufu's CLI through a real
PTY, exactly as the shell-use tool would.

## Methodology

Each test follows the shell-use workflow:

1. **open** — Start a bash shell session in a PTY
2. **submit** — Send a hufu CLI command
3. **wait command** — Wait for the command to finish
4. **expect text** — Assert expected output is present
5. **close** — Close the session

Tests run against the pre-built hufu binary (56MB, dynamically linked Go binary).

## Test Categories

| Category | Tests | Passed | Failed | Pass Rate |
|----------|-------|--------|--------|-----------|
| budget | 4 | 4 | 0 | 100% |
| cli_basics | 4 | 4 | 0 | 100% |
| combinations | 3 | 3 | 0 | 100% |
| discovery | 2 | 2 | 0 | 100% |
| error_handling | 3 | 3 | 0 | 100% |
| flags | 14 | 14 | 0 | 100% |
| help_subcommands | 4 | 4 | 0 | 100% |
| model_overrides | 8 | 8 | 0 | 100% |
| output_formats | 2 | 2 | 0 | 100% |
| prompt_syntax | 2 | 2 | 0 | 100% |
| security | 3 | 3 | 0 | 100% |
| subcommands | 6 | 6 | 0 | 100% |
| team_ops | 3 | 3 | 0 | 100% |
| **Total** | **58** | **58** | **0** | **100%** |

## Test Details

### CLI Basics (4/4)
- **help_flag** — `hufu --help` shows usage with flag listings
- **no_args** — Running with no args produces output
- **unknown_flag** — Unknown flags are handled gracefully
- **quiet_flag** — `--quiet` flag is processed correctly

### Subcommands (6/6)
- **doctor** — `hufu doctor` runs preflight checks (provider, model, workspace, teams)
- **list** — `hufu list` shows discoverable teams
- **list_alias_ls** — `ls` alias works for list command
- **list_alias_teams** — `teams` alias works for list command
- **init** — `hufu init <team>` scaffolds team files
- **init_no_overwrite** — Init does not overwrite existing files

### Team Operations (3/3)
- **default_team_dryrun** — `--default --dry-run` shows preview without LLM calls
- **dry_run_mode** — Dry-run mode works with various prompts
- **agent_team_flag** — `--agent-team` flag is processed correctly

### Output Formats (2/2)
- **json_output** — `--json` produces JSON output mode
- **output_text** — `--output text` format works

### Flags (14/14)
- **verbose_flag** — `--verbose` / `-v` flag
- **temp_workspace** — `--temp` / `-t` flag
- **think_flag** — `--think` flag
- **plan_flag** — `--plan` flag
- **unattended_flag** — `--unattended` flag
- **model_flag** — `--model` override
- **no_journal_flag** — `--no-journal` flag
- **report_flag** — `--report` flag
- **auto_skills_flag** — `--auto-skills` flag
- **helper_tools_flag** — `--helper-tools` flag
- **max_rounds_flag** — `--max-rounds` override
- **timeout_flag** — `--timeout` override
- **var_flag** — `--var` template variable
- **skill_flag** — `--skill` forced skill loading

### Security (3/3)
- **no_net_flag** — `--no-net` blocks network access
- **rbash_flag** — `--rbash` restricted bash mode
- **force_mcp_flag** — `--force-mcp` disables built-in tools

### Budget & Limits (4/4)
- **max_duration** — `--max-duration` wall-clock budget
- **max_total_tokens** — `--max-total-tokens` token budget
- **max_concurrent** — `--max-concurrent` parallelism limit
- **max_steps** — `--max-steps` per-agent step budget

### Model Overrides (8/8)
- **sidecar_model** — `--sidecar-model` override
- **guard_model** — `--guard-model` override
- **judge_model** — `--judge-model` override
- **plan_reviewer_model** — `--plan-reviewer-model` override
- **temperature** — `--temperature` override
- **max_tokens** — `--max-tokens` override
- **top_p** — `--top-p` override
- **top_k** — `--top-k` override

### Multi-Flag Combinations (3/3)
- **combined_flags** — Multiple flags combined: `--default --dry-run --unattended --quiet --no-journal --max-rounds 3 --timeout 120`
- **all_budget_flags** — All budget flags together: `--max-duration 300 --max-total-tokens 50000 --max-concurrent 4 --max-steps 20`
- **security_flags_combined** — Security flags together: `--no-net --rbash --force-mcp`

### Team Discovery (2/2)
- **search_path** — `--agent-team-search-path` flag
- **auto_team** — `--auto-team` automatic team selection

### Prompt Syntax (2/2)
- **at_agent_syntax** — `@agent-name` direct agent invocation
- **profile_flag** — `--profile` named flag bundle

### Help Subcommands (4/4)
- **doctor_help** — `hufu doctor --help` shows usage
- **list_help** — `hufu list --help` shows usage
- **init_help** — `hufu init --help` shows usage
- **chat_help** — `hufu chat --help` shows usage

### Error Handling (3/3)
- **invalid_team** — Nonexistent team name handled gracefully
- **mutually_exclusive** — `--default` + `--agent-team` conflict handled
- **empty_prompt** — Empty prompt handled without crash

## Conclusion

All 58 shell-use tests passed, demonstrating that hufu's CLI is robust across:
- All major subcommands (doctor, list, init, chat)
- All CLI flags (30+ flags tested individually and in combination)
- Output format modes (text, JSON)
- Security modes (no-net, rbash, force-mcp)
- Budget/limit controls
- Model override parameters
- Error handling edge cases
- Help/usage output

The shell-use testing methodology provides a fundamentally different testing approach
from unit tests: it exercises the CLI as a real user would, through a terminal PTY,
catching integration issues that unit tests might miss.
