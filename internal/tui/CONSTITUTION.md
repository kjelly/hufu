# Hufu TUI Project Constitution

## Overview
This document defines the core principles and architectural standards for the Hufu TUI (Terminal User Interface). All implementation and testing must adhere to these rules.

## Core Principles
1.  **Vim-Style Navigation**: Navigation must always support `hjkl` for directionals (h:left, j:down, k:up, l:right) in all major views.
2.  **Pure State Machine**: The TUI logic (Update) must be a pure function of (State, Message) -> (NewState, Cmd).
3.  **Responsive Layout**: The UI must handle `WindowSizeMsg` gracefully, adjusting column widths and viewport heights dynamically.
4.  **Defensive Rendering**: `View()` methods must never panic and should handle uninitialized state (e.g., zero width/height) by showing placeholders or safe defaults.

## Architectural Standards
- **Separation of Concerns**: UI state (Model) should be decoupled from the Coordinator logic.
- **Testable Viewports**: Viewports must be initialized before use and their content should be verifiable via string inspection.
- **Consistent Keys**: Global keys (like `Ctrl+C` for quit, `Esc` for back) must behave consistently across all sub-views.
