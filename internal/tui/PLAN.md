# Technical Plan: TUI Test Implementation

## 1. Test Environment Setup
- Create a `testModel` helper that initializes a `Model` with a fixed width/height (100x40) and populated `tasks`.
- Tasks will cover multiple statuses to ensure all columns have data.

## 2. Navigation Test Strategy
- **Horizontal**: Simulate `l` and `h` keys. Assert `m.col` value and boundary clamping.
- **Vertical**: Simulate `j` and `k` keys. Assert `m.row` value and clamping based on column item count.

## 3. View Switch Test Strategy
- **Detail Transition**: Press `Enter`. Assert `m.inDetail == true` and `m.detailID` matches the selected item.
- **Back Transition**: From Detail, press `Esc`. Assert `m.inDetail == false`.
- **Activity Log**: Press `a`. Assert `m.inActivityLog == true`. Check if `m.vpReady` is handled correctly.

## 4. Search Test Strategy
- **Activation**: Press `/`. Assert `m.inSearch == true` and input focus.
- **Execution**: Input text, press `Enter`. Assert `m.searchQuery` is set and results are populated.
- **Jump**: Check if cursor (`m.col`, `m.row`) jumps to the index of the first matched task.

## 5. Implementation Files
- `internal/tui/speckit_navigation_test.go`: Comprehensive tests derived from `NAVIGATION_SPEC.md`.
