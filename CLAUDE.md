# CLAUDE.md (AI Agent Development Guide)

This file contains quick development commands and style rules for AI coding assistants. For full details on package structure, architecture, CLI flags, and TUI system, see [AGENTS.md](file:///home/ubuntu/nfs/github/hufu/AGENTS.md).

## Quick Commands

- **Build binary**: `go build ./cmd/hufu`
- **Run hufu**: `go run ./cmd/hufu [prompt]`
- **Run tests**: `go test ./...`
- **Lint**: `go vet ./...`

## Code Style & Architecture Guidelines

1. **File Size Limit**: Keep individual source files small (ideally **< 800 lines**). If a file exceeds this limit, decompose it into smaller files with single responsibilities.
2. **Error Handling**: Always wrap errors with context using `fmt.Errorf("doing X: %w", err)`. Do not return raw errors from external dependencies.
3. **TUI State Machine**: The Bubble Tea TUI `Model.Update(msg)` must be a pure function `(Model, Msg) -> (Model, Cmd)`. Do not perform any direct I/O, channel operations, or global state mutations inside `Update()`.
4. **Table-Driven Tests**: Write test cases as slices of structs to verify correctness across multiple inputs. Use standard Go assertions.
5. **Lint Rules**: Follow `.golangci.yml` rules. Run `go vet ./...` before finalizing changes.
