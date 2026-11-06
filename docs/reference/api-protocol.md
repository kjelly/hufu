# Provider API protocol

> Status: active
> Authority: reference
> Verified-Commit: f95d67db
> Supersedes: —
> Superseded-By: —
> Validation: [offline implementation evidence](responses-api-validation.md)

`api-protocol` explicitly selects the request format for one LLM provider.
It applies to workers, coordinator and auxiliary models routed through that
provider. Within that provider, a wire model name, URL, prompt, or provider
error never switches the format. Selecting another backend uses that backend's
configured protocol.

| Setting | Effective protocol | Selection source |
|---|---|---|
| Omitted or empty | `chat-completions` | `legacy` |
| `chat-completions` | `chat-completions` | `explicit` |
| `responses` | `responses` | `explicit` |

Values are trimmed and case-normalized. `auto`, aliases and unknown values are
rejected. This is an LLM setting; agent backends continue to use their separate
`protocol` setting, such as `app-server-v2`.

## Configuration

Canonical configuration in `hufu.yaml` or `team.yaml`:

```yaml
backends:
  remote:
    kind: llm
    type: openai-compatible
    base-url: https://provider.example/v1
    api-protocol: responses
```

The legacy provider form remains supported:

```yaml
providers:
  remote:
    provider-url: https://provider.example/v1
    api-protocol: responses
```

These examples illustrate syntax; retain your own configured endpoint,
credential source and model targets. A target such as `remote/gpt-5` uses the
`remote` provider's protocol. Do not define the same backend through both forms.

Canonical backend entries replace the corresponding global entry at the team
overlay, so include its required fields. Configuration-file legacy provider
merges inherit nonempty fields; CLI backend setup preserves its existing whole
entry override rules. After merging, a remaining empty protocol value retains
legacy behavior. An empty value in a field-inheriting layer does not clear an
inherited nonempty protocol.

## Supported content and options

Responses supports text and local function call/result history. Images, audio,
files, hosted tools, provider-executed references and server-managed response
history are rejected before generation. `store: true` and nonempty
`previous_response_id` are not supported.

Fantasy is pinned to v0.45.5. Its Responses adapter does not replay reasoning
items or encrypted reasoning content. An explicit reasoning effort is accepted
only when the pinned adapter actually encodes it for that model name; this is
an SDK encoding rule, not a statement of upstream model support. For example,
`gpt-6` and generic custom aliases with explicit effort fail before generation.
`none` is an explicit effort too. Omit effort when it is not needed, or use a
model/effort combination that both the adapter and your provider support.

Temperature and top-p are ignored by the pinned Responses adapter and produce
bounded warnings. Other provider/model rejections remain errors; a 400 or 404
does not trigger a switch to Chat Completions.

## Admission, usage and resume

Responses admission uses a conservative estimate of the escaped UTF-8 JSON
inventory, including history, tool schemas, structured output and options.
Its byte floor and framing allowances can reject a request earlier than the
Chat estimator. The existing output reservation and safety margin still apply;
this is not an exact tokenizer or an upstream token-count claim.

Explicit zero usage remains known zero. Missing or invalid usage remains
unknown. Cached input is recorded separately; reasoning tokens already included
in output are not added again. Streaming function calls become executable only
after a completed terminal response has been verified.

Explicit protocol and binding version are part of frozen execution identity.
Changing protocol, including changing legacy Chat to explicit Chat, rejects
resume before generation. Restore the original setting to resume that session;
use the existing `--new` workflow when intentionally starting a new session with
a different protocol.

## Diagnostics

`hufu doctor --json` and `hufu team check <team> --output json` report requested
and effective protocol, source and binding version. Requested `legacy` is a
diagnostic label for an omitted value, not a valid configuration value.

The added protocol check reads configuration only. Doctor retains its existing
metadata checks; team check remains offline unless `--online` is requested.
Neither performs a new generation probe, and a reported selection does not
prove endpoint compatibility.

This implementation is verified with fake HTTP and offline integration tests.
Real OpenAI endpoint verification is outside its acceptance criteria and awaits
separate user authorization. No lower cost, latency or improved answer quality
is claimed.
