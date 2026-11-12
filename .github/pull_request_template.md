Describe the problem and resulting behavior. For a non-documentation change,
fill the evidence block below using actual test/artifact references. The
contract is in `docs/reference/fix-evidence.md` in the repository.

Choose `maintenance` for non-behavior maintenance, with `regressions: []` and
`cause_status: not_applicable` when appropriate; do not invent an RCA or test.
For a bug fix, use `partial` until the cause and required source checks are
supported. A probe never justifies `workflow_complete`.

```hufu-fix-evidence
{
  "kind": "bugfix",
  "acceptance": "",
  "cause_status": "unresolved",
  "cause": "",
  "cause_evidence": [],
  "regressions": [
    {"path": "", "scenario": "", "before": "", "after": ""}
  ],
  "checks": [
    {"command": "", "result": "not_run", "evidence": ""}
  ],
  "workflow": {
    "level": "offline",
    "claim": "partial",
    "scope": "",
    "evidence": [],
    "limitations": ""
  }
}
```
