# Specification: TUI Navigation & Detail View

## Description
The TUI provides a column-based dashboard to track agent tasks. Users can navigate between columns, select tasks to view details, and switch between various utility views (Memory, Activity Log).

## Behavior Checklist
### Navigation
- [ ] `h` and `l` keys switch between task status columns (Pending, In Progress, etc.).
- [ ] `j` and `k` keys move the task selection cursor within the current column.
- [ ] Column wrapping: Moving right from the last column or left from the first column is prevented (strict boundary).

### View Switching
- [ ] Pressing `Enter` on a selected task opens the **Detail View**.
- [ ] Pressing `Esc` in Detail View returns to the **Columns View**.
- [ ] Pressing `a` toggles the **Activity Log** overlay.
- [ ] Pressing `M` (Shift+M) from Detail View switches to **Memory View**.

### Detail View Content
- [ ] Detail view must display the Task description, Agent name, and injected skills.
- [ ] Logs for the specific task must be visible and scrollable.

### Search Functionality
- [ ] Pressing `/` opens search input.
- [ ] Typing a query and pressing `Enter` highlights matches.
- [ ] Navigation automatically jumps to the first match found.
