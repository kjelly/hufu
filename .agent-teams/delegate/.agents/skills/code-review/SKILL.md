---
name: code-review
description: Review code for bugs, security issues, performance problems, and style violations. Provides structured feedback categorized by severity.
allowed-tools: read, bash, grep, find
---

# Code Review Skill

## When to Use This Skill

Use this skill when asked to review, audit, or check code quality.

## Review Checklist

### 1. Correctness
- Logic errors and off-by-one mistakes
- Unhandled edge cases (nil pointers, empty slices, etc.)
- Incorrect API usage
- Race conditions in concurrent code

### 2. Security
- SQL injection vulnerabilities
- Hardcoded secrets or credentials
- Insecure cryptographic practices
- Missing input validation
- Path traversal risks

### 3. Performance
- N+1 query patterns
- Unnecessary allocations in hot paths
- Missing or inefficient caching
- Large memory copies when references would suffice

### 4. Style & Readability
- Function length (prefer <50 lines)
- Meaningful variable names
- Consistent error handling patterns
- Appropriate use of comments (explain why, not what)

## Output Format

Present findings in this structure:

```
### Critical (must fix)
- [issue description] at [file:line]

### Warning (should fix)  
- [issue description] at [file:line]

### Suggestion (nice to have)
- [issue description] at [file:line]
```

## Important Rules

- Always read the actual code using `read` tool before making claims
- Use `grep` and `find` to check for patterns across the codebase
- Verify claims by running `bash` commands when uncertain
- Never suggest fixes without confirming the issue actually exists