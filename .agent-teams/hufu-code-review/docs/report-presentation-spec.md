# Reader-facing report sources

- [x] Findings and Critic headings use original summary and location, rather than task IDs or payload hashes.
- [x] The main report contains original impact/detail, readable verdict, rationale, missing evidence and next steps without inventing a defect or remediation.
- [x] Full task IDs, hashes, source-to-critic links, artifact IDs, gaps and invariant assessments remain in the audit appendix.
- [x] Multiple sources with reused finding indexes retain their own title/location/verdict association.
- [x] Ordered decisions use the schema-enforced array position, including JSON integer spellings such as 0.0; no numeric string guessing.
- [x] Empty findings/critics are explicit; missing required fields and invalid indexes fail rather than silently substituting another source.
- [x] Payload contracts, canonical hashes, verification and saved historical bodies retain their existing semantics.
