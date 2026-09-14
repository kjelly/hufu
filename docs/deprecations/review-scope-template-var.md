# Review scope template-variable compatibility bridge

The bundled `hufu-code-review` team temporarily accepts
`--var review.scope.max_commits=N` as an execution-scope compatibility bridge.
The team default is 10 first-parent commits ending at `HEAD`; `N` must be a
base-10 integer from 1 through 100.

This bridge does not parse natural-language scope. A prompt that says “last 7
commits” does not override the configured value. Until typed run inputs are
enabled, operators must pair any non-default request with the explicit
`--var review.scope.max_commits=N` option and treat the producer's runtime
scope output—not a model-authored heading—as the actual reviewed range.

The template variable will be deprecated as an execution input when the team
migrates to `--input review.scope=...`. Ordinary `--var` prompt and
configuration templating remains supported.
