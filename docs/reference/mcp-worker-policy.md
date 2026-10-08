# MCP worker observation policy

A team maintainer can authorize a trusted external MCP tool for an unbound,
read-only worker by declaring `toolPolicies` on the owning server. The worker
must also name the exact `{server}__{tool}` in its literal tool grant. An empty
or `all` grant does not confer this exception.

```yaml
mcp-servers:
  documents:
    type: local
    command: [document-server]
    allowedTools: [read_document]
    toolPolicies:
      read_document:
        sideEffect: none
        filesystem: none
        inputSchema:
          type: object
          additionalProperties: false
          required: [key]
          properties:
            key: {type: string, enum: [public_record]}
```

The agent declares `tools: documents__read_document`. Only `sideEffect: none`
and `filesystem: none` are supported. `inputSchema` must be a closed object
schema (`type: object`, `additionalProperties: false`), at most 64 KiB, using
Draft 2020-12 without external resource loading. Arguments are bounded to
64 KiB, reject duplicate keys and excessive nesting, and must satisfy this
additional schema before transport. The server's native argument contract
continues to apply. Direct calls and the dynamic gateway enforce the same
worker permission boundary. Policy-enabled tools require a runtime authorizer;
the legacy manager execution entry point cannot bypass it.

The declaration is a maintainer trust decision, not an MCP annotation or an OS
sandbox. `filesystem: none` asserts that permitted arguments do not expose
model-controlled local filesystem operations. Servers may maintain their own
internal state or cache. Input restrictions must exclude path/export parameters
or constrain them appropriately. The maintainer remains responsible for server
implementation, network access, redirects and effects of admitted operations.
No declaration permits unsupported filesystem access or external mutations.

Authentic manager adapters alone receive the worker exception. Team denies,
session decisions, phase/sequence gates, unattended permissions and `no-net`
remain effective. Bound workset workers still require artifact path enforcement
and cannot use this exception. Reserved MCP runtime actions remain hidden.
Missing configured tools fail coordinator startup before a model call.

Server launch identity, environment digests and tool policy declarations are
pinned in the execution-policy snapshot. Each occurrence also freezes the live
tool descriptor, including its policy. Changing a declaration, server config or
schema requires a new session; changing a descriptor makes the frozen target
unavailable. Legacy snapshots that cannot pin policy fail closed. Teams without
these declarations retain their previous snapshot and descriptor fingerprints.

## Binding text responses

`x-hufu-tool-evidence` accepts `output_format: text` to match runner-owned MCP
text. Existing JSON bindings remain unchanged. Declare exactly one of `tool`
or `tools`, and exactly one of `input_pointer` or `target_pattern`.

```json
{
  "groups_pointer": "/findings",
  "items_pointer": "/sources",
  "tool": "documents__read_document",
  "input_pointer": "/key",
  "value_pointer": "/key",
  "output_format": "text",
  "text_pattern": "(?ms)^Content:\\n(.*?)\\nEnd$",
  "quote_pointer": "/quote",
  "status_pointer": "/state",
  "call_id_pointer": "/tool_call_id",
  "verified_status": "checked",
  "unverified_status": "unavailable",
  "group_fallback": {"/verdict": "unverified"}
}
```

`text_pattern` optionally isolates source text from metadata. With no pattern,
the whole text is eligible. For tools that return the actual target in their
response instead of accepting it as an argument, use `target_pattern` instead
of `input_pointer`. Patterns use Go/RE2 syntax, are limited to 4096 bytes, and
must contain exactly one capture group. Exactly one match is required per
response; missing, ambiguous or truncated sections fail closed. `output_pointer`
is only for JSON output and cannot appear with text output.

The source identity and quote must match the same successful logical tool call
in the submitting worker's current task/run/attempt transcript. Tool errors,
unpaired results, other tools and other workers cannot supply evidence. Runtime
overrides model-provided call IDs and downgrades mismatches using the owning
schema's fallback and diagnostic vocabulary. Text matching verifies what the
server returned; it does not establish the truth of a source's assertion.
When target identity exists only in output, an error with no target metadata
cannot be attributed to a specific source and is reported as absent.
