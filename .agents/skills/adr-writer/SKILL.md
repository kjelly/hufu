---
name: adr-writer
description: Write an Architecture Decision Record (ADR) capturing the context, options considered, decision, and consequences of a technical choice.
---

# ADR Writer Skill

When invoked, write a Markdown ADR using the standard MADR (Markdown Any Decision Record) template.

## Output Path

Write to `docs/adr/NNNN-short-title.md` where NNNN is the next zero-padded sequence number in the existing `docs/adr/` directory. If the directory does not exist, create it.

## Template

```markdown
# NNNN. Short Title

- Status: proposed
- Date: YYYY-MM-DD
- Deciders: <who made this decision>

## Context and Problem Statement

<2-4 sentences describing the situation and the question that needs answering.>

## Decision Drivers

- <driver 1>
- <driver 2>
- <driver N>

## Considered Options

1. <option 1 title>
2. <option 2 title>
3. <option 3 title>

## Decision Outcome

Chosen option: "<X>", because <1-3 sentences explaining the primary reason>.

### Consequences

- Good, because <reason>
- Good, because <reason>
- Bad, because <reason>

### Confirmation

<How will we know this decision worked? What metric or event confirms success?>
```

## Process

1. Identify the decision from the prompt or context. If unclear, ask.
2. List 2-4 alternatives — including "do nothing" if relevant.
3. Pick the recommended option with explicit reasoning.
4. List both positive and negative consequences honestly.
5. State how the decision will be validated later.
6. Use ISO 8601 date (YYYY-MM-DD).

## Style

- Past/present tense is fine; avoid future tense for decisions already made.
- Each consequence is one short sentence; do not pad with explanations.
- Keep the whole ADR under 80 lines.
