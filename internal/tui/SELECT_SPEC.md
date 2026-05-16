# Specification: TUI Text Selection & Copy

## Description
This feature enables users to interact with task logs in the Detail View using Vim-like text selection. Users can enter a Visual Mode, define a text range, and copy that range to the system clipboard using OSC52 escape sequences.

## Behavior Checklist
- [ ] **Visual Mode Activation**: Pressing `v` in the Detail View enters **VISUAL mode**, marking the current cursor position as the selection start.
- [ ] **Visual Mode Indicator**: The footer or status bar must clearly indicate that the user is in `-- VISUAL --` mode.
- [ ] **Navigation & Extension**: Using `h`, `j`, `k`, `l` (Vim keys) or arrow keys while in VISUAL mode extends the selection from the start point to the current cursor position.
- [ ] **Visual Highlighting**: The selected text range must be visually distinguished (e.g., different background color) in the viewport.
- [ ] **Copy to Clipboard**: Pressing `y` (yank) while in VISUAL mode copies the selected text to the clipboard and returns the view to NORMAL mode.
- [ ] **Cancellation**: Pressing `Esc` in VISUAL mode cancels the selection and returns to NORMAL mode without copying.
- [ ] **Multi-line Support**: Selection must correctly handle ranges spanning multiple lines of log content.
