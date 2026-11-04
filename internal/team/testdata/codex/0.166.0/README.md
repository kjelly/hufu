# Codex 0.166.0 UserInput protocol fixture

Generated offline with `codex-cli 0.166.0` using
`codex app-server generate-json-schema --out <private-task-directory>/schema`.
No account, model call, or running app-server inference session is required.

Source: [official App Server message schema](https://developers.openai.com/codex/app-server/).
The complete generated `v2/TurnStartParams.json` SHA-256 is
`49e53282f327c25e7b39d97afacd038ef10ad5c4fb0431608b354d3a7bf6bcea`.

`user-input.schema.json` keeps the upstream `UserInput` definition and its
transitive `ByteRange`, `TextElement`, and `ImageDetail` definitions unchanged.
It replaces the unrelated turn envelope with a self-contained root reference.
To reproduce, select those definitions, sort their names, and write UTF-8 JSON
with two-space indentation and a final newline; retain upstream number values.

The compact, recursively sorted-key JSON SHA-256 of the upstream `UserInput`
definition alone is
`b082a38701b9ec3eb1ac56572e9d5d55c9016c1428ff6ddff647e24c5b9e139b`.
The tests verify this upstream digest before using the schema.

The fixture demonstrates protocol shapes, including `localImage` with required
`type`/`path` and optional `detail`. It does not establish model vision support
or enable image input in Hufu. Existing text-only producer inputs must continue
to validate; native image transport is delivered in a later implementation stage.
