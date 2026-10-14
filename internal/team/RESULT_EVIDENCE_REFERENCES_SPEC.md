# Result evidence references

Behavioral checklist:

- [x] Schema-owned JSON pointers select reference entries, source tasks, and
  artifact references without interpreting task prose or team vocabulary.
- [x] References name accepted `evidence_from` results with intact payload hashes;
  optional source predicates and exclusions enforce the declared relationship.
- [x] Each reference appears in both the source and submitting result's
  `files_read`, including an explicitly empty submitting read list.
- [x] Local submission rejection permits correction in the same attempt without
  publishing the rejected result.
- [x] External canonicalization checks normalized, authorized file references
  before publishing a result, independently of `execution.requires-evidence`.
- [x] Replayed completed tasks and `finish` recheck reference bindings without
  requiring the optional context-delivery proof or a final-report template.
- [x] Final-report rendering rechecks bindings before displaying an accepted report.

Regression coverage: `result_contract_references_test.go` exercises admission,
ownership, canonical external reads, replay, failed finish, and preflight.
