# Historical result presentation

- [x] Plain final stdout preserves saved content; stderr identifies team, completion time, invocation, run and source event.
- [x] JSON, quiet and payload-only output preserve their machine-readable contracts.
- [x] show uses a CLI formatter with complete details, findings/severity, decisions/reasons and artifact references; it does not call FormatForContext.
- [x] show --payload prints only the complete validated payload JSON, including payloads larger than 16 KiB; absent payloads fail clearly.
- [x] Payload lookup retains all caller scope filters, redaction and canonical source provenance, and does not read artifact paths.
- [x] Every individual task has a final-response reading command; the CLI never infers a receipt role from an agent name.
- [x] --render markdown is optional; plain is the default. Pipes, JSON, payloads, plain display and no-color modes remain ANSI-free.
- [x] Explicit Markdown rendering uses terminal width and the existing light/dark/mono presentation choices, with controlled input budgets.
- [x] The team report starts with conclusion, concise findings and next actions, and retains all original evidence, gaps and critic decisions in an audit section.
- [x] Critic results remain distinct from original proposals; presentation does not promote rejected/unverified findings or evidence gaps into accepted defects.
