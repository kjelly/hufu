# Speckit x OpenCode Integration Workflow

This document defines how to combine GitHub's **Spec-kit** (Spec-Driven Development) with the local **OpenCode** implementation planning process.

## 1. The Integrated Lifecycle

| Phase | Tool | Artifact | Description |
|-------|------|----------|-------------|
| **Constitution** | Speckit | `CONSTITUTION.md` | Core principles and architectural standards. |
| **Specify** | Speckit | `*_SPEC.md` | Behavioral requirements and a completion checklist. |
| **Plan** | **OpenCode** | `.opencode/plans/*.md` | Detailed technical implementation: fields, methods, logic, and file changes. |
| **Verify** | Speckit | `*_test.go` | Unit and integration tests derived from the checklist. |

## 2. Workflow Steps

### Step 1: Specify (Speckit)
Define the feature using the Speckit format. Focus on **What** and **Why**.
- Location: `internal/[package]/[feature]_SPEC.md`
- Key output: Behavioral Checklist.

### Step 2: Technical Planning (OpenCode)
Translate the checklist into a detailed implementation plan. Focus on **How**.
- Location: `.opencode/plans/[feature].md`
- Contents: Model changes, state machine transitions, code snippets, and dependencies.
- **Traceability**: Every item in the Speckit checklist must be mapped to a technical change in this plan.

### Step 3: Implementation
Execute the changes described in the OpenCode plan.
- Command: Use `/speckit.implement` or manual execution.

### Step 4: Verification (Speckit-driven Testing)
Generate and run tests based on the Speckit checklist to ensure the implementation matches the spec.
- Location: `internal/[package]/[feature]_test.go`

## 3. Example: TUI Selection & Copy

1.  **Speckit Specify**: Create `internal/tui/SELECTION_SPEC.md` defining that users should be able to select text with 'v' and copy with 'y'.
2.  **OpenCode Plan**: Create `.opencode/plans/vim-selection.md` defining `cursorLine`, `inVisual` fields and the `copySelection()` logic.
3.  **Speckit Verify**: Create `internal/tui/selection_test.go` simulating the keypresses and asserting the visual selection state.
