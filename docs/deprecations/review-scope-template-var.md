# Review scope template-variable deprecation

> Status: deprecated
> Authority: reference
> Verified-Commit: `0a68f3e1547fbb25c4ae78a2fd9f091934a13b11`
> Supersedes: —
> Superseded-By: typed `review.scope` run input

The bundled `hufu-code-review` team now uses the typed `review.scope` run input.
Its default is the last 10 first-parent commits ending at `HEAD`.

Use one of these explicit inputs when automation must pin the scope:

```bash
hufu --agent-team hufu-code-review \
  --input 'review.scope={"kind":"last_n","count":3,"history":"first_parent","head":"HEAD"}' \
  'Review the selected changes'

hufu --profile hufu-code-review-ci 'Review the selected changes'
```

`--var review.scope.max_commits=N` is deprecated and no longer controls the
producer payload. Supplying it emits a warning on stderr. Remove it or replace
it with `--input review.scope=...`; ordinary `--var` prompt/configuration
templating remains supported.

When the prompt also contains a scope expression, it must agree with an
explicit typed input. A conflict fails before coordinator or worker model
execution. Reports and acceptance use the frozen input snapshot and the
producer's runtime-owned scope attestation, not model-authored prose.
