---
name: test-review
description: Review a set of tests for quality, coverage, and anti-patterns. Use when asked to review, audit, or critique tests.
---

# Test Review Skill

When invoked with a set of test files, evaluate them against the following checklist and return a structured report.

## Checklist

### Correctness
- [ ] Each test asserts exactly one behavior (or one logical group with `t.Run` sub-tests).
- [ ] Assertions are specific (`assert.Equal(t, 42, got)` not `assert.NotNil(t, got)`).
- [ ] The test name describes the expected behavior, not the implementation.

### Coverage
- [ ] Happy path is covered.
- [ ] At least one error path is covered for each function that can fail.
- [ ] Boundary values are tested (0, 1, max, min, empty, nil, large).
- [ ] Concurrency: race conditions are tested if the code is concurrent.

### Quality
- [ ] No test depends on another test's state (no order dependence).
- [ ] No test sleeps for arbitrary durations (use polling or `require.Eventually`).
- [ ] No test reads from the network unless explicitly mocked.
- [ ] No test writes to a real database or filesystem unless using a tempdir or in-memory store.

### Style
- [ ] Test names follow the project convention (`TestFunctionName_Scenario` or `Test FunctionName Scenario`).
- [ ] Table-driven tests group related cases under a single `for _, tt := range tests { t.Run(tt.name, ...) }`.
- [ ] Setup helpers (e.g. `newTestHarness`) are reused, not copy-pasted.
- [ ] Tests use the project's existing test framework (e.g. `testing` + `testify`).

## Output Format

Return a markdown report:

```markdown
# Test Review: <scope>

## Summary
<one paragraph: overall quality, key concerns>

## Critical Issues
- file:line — <issue>

## Improvements
- file:line — <suggestion>

## Strengths
- <what is done well, to preserve in future changes>
```

## Process

1. Discover test files via the project's test directory conventions.
2. Read each test file in full before commenting.
3. Cross-reference against the production code it covers.
4. Group findings by severity: Critical (test is wrong) → Improvement (could be better) → Strength.
5. Be specific: file:line citations, not vague generalizations.
