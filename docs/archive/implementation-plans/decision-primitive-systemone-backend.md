# hufu DecisionPrimitive System One backend implementation specification

> Status: implemented — archived 2026-10-01; implemented on branch feat/decisionrt-systemone (see Implementation record)
> Authority: reference (implementation record; current behavior is defined by [DecisionPrimitive](../../architecture/decision-primitive.md) §7.3, §18A, §29, §42.2, §46, §47, and §52)
> Verified-Commit: 2026-10-01
> Supersedes: —
> Superseded-By: [DecisionPrimitive](../../architecture/decision-primitive.md)
> Target repository: github.com/kjelly/hufu
> Baseline commit: f2d7fc35d6a39d00d9dbe420adba0605f744c7d4
> Date: 2026-10-01
> Scope: explicit System One backend for the standalone DecisionPrimitive API and hufu decisionrt CLI, a shared canonical-context helper, and raised per-attempt timeouts

## Implementation record

Implemented on feat/decisionrt-systemone, one commit per phase of Section 10. The plan was first gitignored scratch material in docs/tmp/, then a draft in docs/architecture/ during implementation. It moved here once every phase had landed.

| Commit | Phase | Content |
| --- | --- | --- |
| `0ab7afc` | plan | This plan, v2, as a tracked draft. |
| `e887f81` | step 1 | Golden sidecar prompt-byte tests covering every accepted context type; a CLI test that the default backend is rule with no fallback. |
| `b0720d3` | step 2 | decisionrt.CanonicalContextValues; sidecar uses it in place of its three private helpers; a golden digest test. |
| `50387fe` | step 3 | decisionrt.DefaultTimeout (5s) and MaxTimeout (30s), used by NewRuntime and the CLI. |
| `0b22bdf` | step 4 | The internal/decisionrt/backend/systemone adapter and its tests. |
| `95e46f9` | step 5 | Registry row, Resolve, and the --systemone-* flags with the HUFU_SYSTEMONE_API_KEY fallback. |
| `b851548` | step 6 | End-to-end CLI tests through the real registry against httptest.Server. |
| `3a7b51b` | step 4 follow-up | Concurrent-use test under the race detector, so the canonical document can list systemone in §28. |
| `0c03bb8` | step 7 | decision-primitive.md (new §18A and the sections listed in Section 8), both README files, and docs/README.md. |

Baseline at f2d7fc3: go build, go vet, go test ./... (46 packages), and golangci-lint run all passed. bin/check-docs already failed because docs/reference/performance-gate.md has no lifecycle header (since af6205c). This change leaves that file alone; every other header and all 213 relative links passed.

After implementation: go build ./..., go vet ./..., go test ./... (47 packages), and golangci-lint run (0 issues) all pass. internal/team has no changes.

A live smoke test ran the built binary against Ollama 0.35.0 with nimble on a LAN GPU host. It is not an acceptance dependency.

| Request | Result | Exit |
| --- | --- | --- |
| backends | systemone available | — |
| choice (support ticket) | billing, raw 0.991, receipt with backend systemone, model nimble, fallback_used false | 0 |
| boolean | true, 0.9987 | 0 |
| integer 0..4 | 4, 0.974 | 0 |
| boolean with --min-confidence 0.999 | abstained, low_confidence | 3 |
| single-value integer range | rejected before HTTP | 4 |
| unknown model | unavailable (HTTP 404) | 5 |

### Deviations from the plan

- **Availability reasons.** The registry derives them by calling systemone.New, so it does not repeat the adapter's validation. To make that work, the adapter package exports ErrMissingModel, ErrInvalidModel, ErrInvalidEndpoint, and ErrInvalidAPIKey. These sit in the adapter package, not the core API, so the three exported core additions of Section 1 are unchanged. Resolve also maps an invalid API key to invalid_systemone_api_key internally; List never reports it, because listing does not read the key.
- **Default URL.** The CLI derives the default systemone URL from config.DefaultLocalProviderURL + "/systemone". The value is the planned http://127.0.0.1:11434/v1/systemone.
- **Endpoint validation** also rejects a URL whose host has a port but no hostname, an empty query ("?"), and an empty fragment ("#").
- **Redirects.** The cloned client returns http.ErrUseLastResponse. A 3xx response is therefore classified as ErrorBackendFailure, the plan's "rejected redirect" row.
- **Transport errors** drop the URL that *url.Error adds, so an error never carries the endpoint path.
- **Extra tests.** Beyond Section 9:
  - a golden digest test;
  - a dependency-boundary test that the adapter imports only the standard library and internal/decisionrt;
  - a concurrent-use test;
  - a runtime-level test that NewRuntime accepts every mapped result.

### Not done

- The hosted TypeSafe System One API was not verified (no account or key), and docs.system-one.dev was unreachable.
- systemone is not connected to team execution. The plan excludes it, and DecisionPrimitive still has no caller outside cmd/hufu.

## 0. Revision 2 decisions

v1 was checked against the baseline code and against a live Ollama 0.35.0 `/v1/systemone` endpoint serving `nimble` (Appendix A). The following decisions are binding and override any v1 wording:

| ID | Decision |
| --- | --- |
| D1 | A single-value integer range (Min == Max) is not sent to the provider. Ollama rejects a one-criterion choice with HTTP 400. systemone rejects the request locally before HTTP with ErrorBackendFailure. It never decides locally and never pads the domain. |
| D2 | The per-attempt timeouts are raised for every backend. RuntimeConfig.Timeout == 0 and the CLI --timeout default become 5s. The maximum in NewRuntime and the CLI becomes 30s. |
| D3 | The 64 KiB request bound is a hufu-local bound, not a claim about provider documentation. An oversized encoded request is ErrorBackendFailure before HTTP, the same kind sidecar uses for its prompt-size limit. |
| D4 | Adding an external inference adapter is approved. It reverses the exclusion in the canonical decision-primitive.md (§23, §46, §36 and related sections). Section 8 lists the required documentation edits. |
| D5 | Context canonicalization for transport moves into one exported decisionrt helper. sidecar and systemone both use it, so no backend keeps a private copy. sidecar prompt bytes must remain byte-identical. |

## 1. Outcome and boundaries

Add a backend named systemone. It sends one bounded DecisionPrimitive request to an exact POST /v1/systemone endpoint. The existing rule and sidecar backends remain available. The default CLI backend remains rule. The same decisionrt.Request can be passed explicitly to sidecar or systemone.

The backend is a generic DecisionPrimitive protocol adapter. It must not add fields to decisionrt.Request, BackendResult, Receipt, or the decisionrt.Backend interface. It must not modify internal/team or the higher-level DecisionEngine. It must not automatically connect System One to team execution, routing, retry, guards, or authorization. A model result is evidence for a typed decision; it never authorizes a side effect.

This change adds exactly three exported items to package decisionrt: the canonical-context helper (Section 4.1) and the DefaultTimeout and MaxTimeout constants (Section 2.1). It adds nothing else to the core API.

The only supported invocation paths in this change are an explicit Go caller using the new backend and an explicit hufu decisionrt --backend systemone command. Out of scope: automatic migration, backend selection, calibration, batching, ordinal score kind, model discovery, model warm-up, and shadow evaluation.

The protocol references for the wire format are:

- https://ollama.com/blog/ollama-now-supports-jev-style-decision-models (verified live against Ollama 0.35.0, see Appendix A)
- https://docs.system-one.dev/en/docs/api (unreachable from the review environment; not verified)
- https://docs.system-one.dev/en/docs/primitives (unreachable from the review environment; not verified)

The adapter targets the native HTTP response shape that Ollama 0.35 returns: a complete choice distribution or a noul probability. A provider response lacking these fields is invalid output. The implementation does not claim that every upstream adapter, the hosted TypeSafe API, or a rounded Jev response is accepted. Section 5 defines the exact accepted subset.

## 2. Existing contracts to preserve

The current DecisionPrimitive types and policies remain authoritative:

~~~go
type Backend interface {
    Name() string
    Decide(context.Context, Request) (BackendResult, error)
}
~~~

The decision kinds are choice, boolean, and integer_range. The runtime does the following:

- validates requests and results;
- applies one acceptance policy to each attempted backend;
- owns the per-attempt timeout;
- produces receipts and metrics;
- decides whether to invoke a configured fallback.

Existing sidecar results have ConfidenceNone. In the CLI, existing rule behavior is AlwaysAbstain.

The current CLI fallback behavior must remain:

| Primary | CLI fallback | Result |
| --- | --- | --- |
| rule, including the default | none | existing rule behavior |
| sidecar | rule AlwaysAbstain | existing behavior |
| sidecar with --no-fallback | none | existing behavior |
| systemone | none | new explicit behavior |

For systemone, --no-fallback is accepted but has no additional effect. Do not add --fallback-backend, and do not configure a systemone fallback. In particular, do not promise that sidecar can rescue a systemone result rejected by --min-confidence or --require-calibrated. sidecar reports ConfidenceNone, and the existing runtime applies the same policy to both attempts.

### 2.1 Per-attempt timeout (D2)

| Setting | Baseline | v2 |
| --- | --- | --- |
| RuntimeConfig.Timeout == 0 | 2s | 5s |
| CLI --timeout default | 2s | 5s |
| Maximum accepted by NewRuntime and the CLI | 10s | 30s |

Export the two values from package decisionrt as DefaultTimeout and MaxTimeout. Replace the unexported defaultDecisionTimeout and maximumDecisionTimeout constants in runtime.go with these. In cmd/hufu/decisionrtcmd.go, the flag default, the range check, and the error message must use the exported constants instead of the hard-coded 2*time.Second and 10*time.Second. Derive the message from MaxTimeout, which yields "--timeout must be within (0,30s]". The rules that values below zero or above the maximum are ErrorConfiguration (runtime) or ErrorInvalidRequest (CLI) without clamping stay unchanged.

Why these values (measurements in Appendix A):

- The slowest warm request measured on a GPU host took 2.1s: 21 candidates with no prompt cache. That exceeds the old 2s default, and 5s gives more than 2x margin.
- A cold model load took 12.3s, which exceeds the old 10s maximum. 30s lets an explicit --timeout absorb it with more than 2x margin.

The default intentionally does not cover a cold load. Against a hung server, failing fast remains the default behavior. The README documents raising --timeout or pre-warming instead.

This applies to every backend. A sidecar attempt that times out now waits up to 5s before the sidecar-to-rule fallback, instead of 2s. A fallback attempt still gets its own new attempt timeout bounded by the caller context. This change is a timeout policy change, not a latency claim. The backend must honor the context it receives.

## 3. Package ownership and public construction

Add internal/decisionrt/backend/systemone/. It may use a few private files for protocol, mapping, validation, and transport. The exact file count is not part of the contract. The package may import only the standard library and internal/decisionrt.

Expose one constructor and no provider-specific core API:

~~~go
type Config struct {
    Endpoint   string
    APIKey     string
    Model      string
    HTTPClient *http.Client
}

func New(Config) (decisionrt.Backend, error)
~~~

New validates configuration without network I/O. A nil HTTPClient uses an internal default. A supplied client is cloned before redirect behavior is set, so the caller's client is not mutated. The backend must reject redirects even when a supplied client would normally follow them. Request cancellation comes from the context supplied to Decide.

Configuration rules:

- **Endpoint** is an exact URL. The CLI defaults it to http://127.0.0.1:11434/v1/systemone.
  - Accept only http or https with a nonempty host.
  - Reject userinfo, query, and fragment.
  - Preserve the specified path, and never append /v1/systemone to an arbitrary URL.
- **Model** is required.
  - Reject leading or trailing whitespace, invalid UTF-8, and control characters.
  - Reject more than 128 Unicode code points or more than 256 UTF-8 bytes. The byte bound also satisfies the current DecisionPrimitive result-model validation.
- **APIKey** is optional. Reject CR, LF, and other control characters before building a header. Do not trim or log it.
- Constructor failures are decisionrt.ErrorConfiguration. The CLI registry may convert them to decisionrt.ErrorBackendUnavailable, but the public error message must stay machine-safe.

Backend.Name returns systemone.

## 4. Native request mapping

Each Decide call first validates decisionrt.Request. It then applies the local limits below. Only after both steps does it send exactly one native question under the fixed key decision.

The request has three fields:

- **model**: the configured model.
- **state**: always a JSON object, `{}` for nil or empty context. It holds only the canonical context from Section 4.1. Do not send Purpose, Spec.ID, Spec.Version, the request digest, fallback data, or receipt metadata.
- **questions**: an object whose only key is decision.

Spec.Question becomes question.instructions. No native question key is derived from user text or metadata.

The native criteria field is a JSON object that maps each opaque key to its description. The keys are zero-padded so that sorted map encoding reproduces the declared order. The encoder must emit the keys in declared order, whether or not it relies on that property.

For choice:

- Emit native type choice.
- Enumerate Spec.Options in order with opaque keys o00 through o20.
- Each criterion description is the option ID. When Description is nonempty, append a colon, a space, and Description.
- Keep a private mapping from each opaque key to its original option ID.

For boolean, emit native type noul with no criteria.

For integer_range with at least two values:

- Emit native type choice.
- Enumerate the permitted integers in ascending order with keys i00 through i20. Each criterion description is the canonical decimal integer.
- Keep a private mapping back to int64.
- Do not use native score. A score may be fractional, while KindIntegerRange is a discrete decision.

Local limits, checked before any HTTP request is sent:

- **Single-value integer range (D1).** Min == Max is ErrorBackendFailure with a fixed message such as "systemone requires at least two candidates". Ollama 0.35 rejects a one-criterion choice with HTTP 400 "criteria must contain 2–26 candidates". Two shortcuts are forbidden:
  - Deciding locally would produce a receipt that names the systemone backend and model and reports a raw confidence the model never produced.
  - Padding the domain with a synthetic criterion would change the decision.

  Choice already requires at least two options, and boolean maps to noul, so this is the only kind affected.
- **Request size (D3).** An encoded request body larger than 64 KiB is ErrorBackendFailure. This is hufu's own bound for this adapter, enforced for every configured endpoint. It is not derived from provider documentation. The hosted documentation could not be checked, and one third-party summary describes a token-based limit of about 32k tokens instead.

  This error kind matches sidecar's prompt-size limit. A Go caller's configured fallback can therefore still serve a request that is valid for DecisionPrimitive but too large for this adapter. The CLI configures no systemone fallback, so the CLI result is exit 4.

The hufu domain caps of 21 options and 21 integer values fall within Ollama's 2–26 criteria limit.

Examples:

~~~json
{
  "model": "nimble",
  "state": {"failure_class": "transient"},
  "questions": {
    "decision": {
      "type": "noul",
      "instructions": "Should this task be retried?"
    }
  }
}
~~~

~~~json
{
  "model": "nimble",
  "state": {},
  "questions": {
    "decision": {
      "type": "choice",
      "instructions": "Select the execution class",
      "criteria": {"o00": "small: Small model", "o01": "large: Large model"}
    }
  }
}
~~~

### 4.1 Shared canonical context helper (D5)

Add one exported helper to package decisionrt, for example in validate.go or a new small file:

~~~go
// CanonicalContextValues returns a new map with the same keys as values,
// where every value is a plain string, bool, or json.Number holding the
// canonical form used by Digest.
func CanonicalContextValues(values map[string]any) (map[string]any, error)
~~~

Rules:

- Build it on the existing canonicalizeContext and canonicalContextValue. Validation, digest, and transport must share one implementation. The "string" type maps to a string, "bool" to a bool, and "number" to json.Number.
- It returns a non-nil map, which is empty for nil or empty input. It returns the same ErrorInvalidRequest errors that Request.Validate returns for an invalid context.
- It must not change Request.Validate or Digest results.
- sidecar replaces its private canonicalPromptContext, canonicalPromptJSONNumber, and canonicalPromptFloat with this helper. systemone uses it to build state. No backend may keep a private copy of numeric canonicalization.
- The name may change if lint or naming review requires it. There must still be exactly one exported helper.

sidecar prompt bytes must remain byte-identical. For every context accepted by Request.Validate, the current sidecar output already equals the helper output:

- Signed integers use FormatInt.
- Unsigned values are bounded by MaxInt64, so FormatUint and FormatInt agree.
- Floats and json.Number use the same rules: a value that is integral and within int64 range is written as an integer, and other values use 'g' with precision -1.

A golden byte test (Section 9) must prove the equivalence before the private helpers are deleted.

## 5. Accepted native response and result mapping

Read at most 64 KiB of the response body; any additional byte is ErrorInvalidBackendOutput. Then validate the body:

- Accept a single JSON document only, and reject trailing JSON data.
- Reject invalid UTF-8 before decoding.
- Reject duplicate JSON object keys at every nesting level, including escaped duplicate names.
- Reject wrong JSON types, malformed numbers, and NaN or Inf.
- Do not include the raw body in errors.

The top-level object must contain a non-null answers object. That object must have exactly one key, decision, whose value is a non-null answer object. Unknown extension fields such as model and usage may be ignored. Check each required field for presence separately from its numeric value, because zero is a valid confidence or noul value. Typed structs with plain float64 fields alone do not prove that a required field was present.

Choice answer requirements:

- type is choice;
- choice is one declared opaque key;
- probabilities is an object containing every declared key exactly once and no extra keys;
- each probability is finite and in [0,1];
- the sum differs from 1 by no more than 1e-6;
- provider confidence is present, finite, and in [0,1]. It is not used as hufu confidence: Appendix A shows it differs substantially from the selected probability.

Map a choice answer as follows:

- Status is StatusDecided, with the original option ID or integer value.
- Candidates is a complete slice in declared order.
- Confidence is the probability of the selected key, and ConfidenceSemantics is ConfidenceRaw.
- If the provider selects a key that is not the maximum-probability key, preserve the provider's selection. Current DecisionPrimitive validation does not impose argmax.

Noul answer requirements:

- type is noul;
- noul is present, finite, and in [0,1].

Let p be noul, the provider's P(true). Map a noul answer as follows:

- Select true only if p > 0.5. The exact tie selects false.
- Return Candidates in the order false with 1-p, then true with p.
- Set Confidence to p for true or 1-p for false, and ConfidenceSemantics to ConfidenceRaw.
- Do not invent a native noul confidence field.

BackendResult.Model is the configured invocation model, not a provider-returned alias. Never mark a System One result ConfidenceCalibrated, and never use the provider's choice confidence as hufu confidence.

The existing runtime validation remains authoritative after mapping. Its candidate-sum tolerance is 1e-6, and the adapter must not silently renormalize provider probabilities to bypass that contract. A rounded response whose distribution falls outside this tolerance is invalid output; this is an explicit compatibility limit. Ollama 0.35 returns full-precision probabilities: the sum error measured at most 2.2e-16 (Appendix A).

## 6. HTTP and errors

Send POST to the exact configured Endpoint with Content-Type: application/json and Accept: application/json. If APIKey is nonempty, send Authorization: Bearer followed by that value; otherwise omit Authorization. Within the adapter, do not stream, retry, probe models, add provider extension fields such as keep_alive, or parse provider error text.

Classify outcomes as follows. Non-2xx responses are classified by status code only.

| Condition | decisionrt kind |
| --- | --- |
| Local limit before HTTP (single-value integer range, encoded request > 64 KiB); no request is sent | ErrorBackendFailure |
| Network or DNS error, connection reset, HTTP 408, HTTP 429, HTTP 5xx, rejected redirect, other unexpected non-2xx | ErrorBackendFailure |
| HTTP 400, 401, 403, 404, 405, 415, 422 | ErrorBackendUnavailable |
| 2xx with malformed JSON, missing fields, out-of-domain values, or an oversized response | ErrorInvalidBackendOutput |

The existing runtime normalizes context cancellation and timeout. The following must never enter normal errors, receipts, or metrics: a provider response body, URL credentials, the Authorization header, the API key, or raw state. Do not place question text, context values, option descriptions, or URLs in metric labels.

The runtime currently falls back only for ErrorBackendFailure and ErrorInvalidBackendOutput. The CLI configures no systemone fallback in this change. The classification above therefore affects exit codes and diagnostics, not an implicit recovery path. A Go caller that configures its own fallback does get fallback for the local limits.

## 7. Registry and CLI

Extend RegistryOptions in cmd/hufu/decisionrt_registry.go with SystemOneModel, SystemOneURL, and SystemOneAPIKey.

- Resolve("systemone") constructs the adapter after validating configuration.
- List returns rule, sidecar, systemone, in that order. The systemone Type label is decision-native.
- Availability depends only on local configuration, with the stable reasons missing_systemone_model, invalid_systemone_model, and invalid_systemone_url. When several apply, report the first in that order.
- Listing performs no network I/O and does not need an API key.

Add these flags to every decisionrt execution subcommand (choice, boolean, integer, run):

- --systemone-model, default empty;
- --systemone-url, default http://127.0.0.1:11434/v1/systemone;
- --systemone-api-key, default empty. HUFU_SYSTEMONE_API_KEY is the fallback, used only when the flag value is empty.

Other flag rules:

- Add --systemone-model and --systemone-url to decisionrt backends for configuration inspection. backends does not read the API key.
- Keep --provider-url, --provider-api-key, --sidecar-model, and HUFU_PROVIDER_API_KEY exclusively on their existing sidecar path.
- Keep the --backend default of rule, and update the --backend help to list all three backends.
- --timeout follows Section 2.1.
- Preserve all current sidecar flag meanings and prompt behavior.

Example:

~~~sh
hufu decisionrt choice \
  --backend systemone \
  --systemone-model nimble \
  --id route --version v1 --purpose model-route@v1 \
  --question "Select the execution class" \
  --option small="Small model" \
  --option large="Large model"
~~~

The same request fields remain valid with --backend sidecar and --sidecar-model. Backends are selected explicitly; the CLI has no auto-routing.

Outcomes with systemone:

- Existing --min-confidence applies to System One's raw selected probability. A result below the threshold abstains with reason low_confidence.
- Existing --require-calibrated always rejects a System One result, because the result is raw. With no fallback configured for systemone, the result abstains.
- An invalid native response, a transport failure, or a local pre-HTTP limit returns an error according to the existing exit-code handling.
- A missing or invalid model is a configuration failure before HTTP.

## 8. Documentation changes during implementation (D4)

Update README.md, README.tw.md, docs/README.md, and docs/architecture/decision-primitive.md. Edit the canonical document in place, and add new sections without renumbering existing ones (for example, a new §18A).

decision-primitive.md:

| Section | Required change |
| --- | --- |
| Header (Scope, Verified-Commit) | Scope includes the systemone adapter; update Verified-Commit to the implementing commit. Keep the non-goal of reimplementing a third-party logits technique: systemone calls a provider and reimplements nothing. |
| §1, §2.1 | Three backends; add systemone to the layering diagram. |
| §2.2 item 5 | Keep. The core API stays provider-neutral, and systemone lives behind Backend only. |
| §4 | Add backend/systemone/. Replace "本規格不建立其他 backend 目錄" with a statement that only the explicit systemone adapter exists, using net/http and no Python or other service dependency. |
| §7.3 | Name the exported canonical-context helper as the single implementation used by Digest, sidecar, and systemone. |
| §12, §29 | Timeout values per Section 2.1, plus the exported DefaultTimeout and MaxTimeout. |
| New §18A | The systemone contract: request mapping, local limits, accepted response, raw-confidence mapping, error classification, and no CLI fallback. |
| §19 | The registry registers rule, sidecar, and systemone. |
| §23 | External inference backends remain excluded, with the explicit systemone adapter as the only exception. Still no logits, llama.cpp, or Python service. |
| §24 | systemone returns ConfidenceRaw and is never ConfidenceCalibrated. Calibration remains excluded. |
| §30, §54 | Add the systemone, helper, and timeout tests from Section 9. |
| §36 | Allow stating that systemone targets the Ollama 0.35 /v1/systemone native shape. Do not claim hosted TypeSafe compatibility, calibration, or coordinator integration. |
| §37, §56, §57 | Replace "two backends" acceptance wording. |
| §42.2 | --backend rule\|sidecar\|systemone; --timeout default 5s, range (0,30s]; the three systemone flags and the environment fallback. |
| §46 | Three valid names; List returns three rows in order; the systemone Type and reasons. |
| §47 | Add the systemone row with no fallback. |
| §52 | backends example and JSON with the systemone row; the accepted flags and reason list. |

README.md and README.tw.md, in the standalone bounded decisions section:

- three backends, and the systemone flags with the HUFU_SYSTEMONE_API_KEY fallback;
- raw confidence: --min-confidence is meaningful, while --require-calibrated always abstains;
- no systemone fallback, and single-value integer ranges are unsupported by systemone;
- requires Ollama 0.35 or later with a decision model such as nimble;
- the timeout default of 5s and maximum of 30s.

The README must also state that local decision-model latency depends on hardware, and that a GPU host is recommended. While Ollama loads a model, the first call can exceed the default timeout; the user can raise --timeout (up to 30s), pre-warm the model, or raise OLLAMA_KEEP_ALIVE on the server. Do not publish measured latency numbers in the README.

docs/README.md: the DecisionPrimitive index line mentions the systemone adapter.

Do not modify docs/architecture/decision-runtime.md or add team-specific routing rules. Do not link active documentation or code comments to this implementation plan.

## 9. Required automated tests

All tests are local and deterministic. Use httptest.Server and fixtures built from the published or verified wire shape (Appendix A). Acceptance does not require a running Ollama process, a hosted account, an API key, purchased credits, a human label set, or a measured benchmark.

Shared helper and timeout tests:

- **Golden sidecar prompt bytes.** Add these tests before the refactor. They must pass unchanged after sidecar switches to CanonicalContextValues. Cover string, bool, signed and unsigned integers, int-valued and fractional floats, and json.Number forms: "3", "3.0", "1e2", and "-0.0".
- CanonicalContextValues equals the expected map for the same inputs, returns `{}` for nil and empty input, and returns ErrorInvalidRequest for invalid context.
- Digest results are unchanged for the existing digest fixtures.
- NewRuntime accepts 30s and rejects 30s+1ns and negative values. Replace the existing 11s rejection case.
- Timeout == 0 gives an attempt context deadline of about DefaultTimeout. Assert it on the context deadline the backend observes; do not sleep.
- The CLI --timeout default equals decisionrt.DefaultTimeout. 30s is accepted, and 31s exits 2 with the derived message.

Adapter and mapping tests:

- choice with 2 and 21 options: original option order, opaque keys, criteria object, descriptions, selected value, complete probabilities, and provider confidence that differs from the selected probability;
- boolean with p = 0, 0.1, 0.5, 0.9, and 1: false/true candidate order and tie behavior;
- integer ranges -2..2 and 21 values: verify the native type is choice, never score;
- a single-value integer range returns ErrorBackendFailure, and the httptest server receives zero requests;
- empty state `{}`, context-only state, numeric normalization through the shared helper, and absence of runtime metadata;
- invalid responses:
  - wrong answer type;
  - missing or null answers or decision;
  - missing or null noul;
  - missing or null provider confidence;
  - missing, extra, or unknown probability key;
  - invalid sum;
  - out-of-range or non-finite probability;
  - duplicate keys, including escaped and nested duplicates;
  - malformed UTF-8;
  - trailing JSON;
- transport and limits:
  - exact POST path and headers, and optional Authorization;
  - rejected redirects and cancellation;
  - a request over 64 KiB returns ErrorBackendFailure with zero requests sent; the 64 KiB response limit;
  - every HTTP status classification, including 404 for an unknown model and 400 for a validation error;
  - body closure;
  - errors free of secrets and raw state;
- constructor validation, and no network call during construction or registry listing.

CLI and runtime tests:

- unchanged default rule behavior and existing sidecar behavior, including sidecar-to-rule fallback and --no-fallback;
- systemone registry availability and selection, missing or invalid configuration, and absence of a systemone fallback;
- accepted raw probability, --min-confidence abstention, --require-calibrated abstention, and error exit codes, including exit 4 for a single-value integer range;
- the existing JSON result and receipt shape with Backend=systemone, the configured Model, and FallbackUsed=false;
- sidecar prompt fixture bytes remain unchanged.

Tests must not require a real model to produce a particular choice or response time. Published examples and Appendix A may seed synthetic fixtures. They are not evidence of measured hufu accuracy or latency.

## 10. Coding-agent execution order and completion

1. Add or update regression tests for rule, sidecar, registry, CLI defaults, and sidecar fallback. Include the golden sidecar prompt-byte tests.
2. Add CanonicalContextValues and switch sidecar to it. The golden tests must pass unchanged. Delete the private sidecar canonicalization helpers.
3. Export DefaultTimeout and MaxTimeout with the Section 2.1 values. Use them in runtime.go and decisionrtcmd.go, and update the timeout tests.
4. Implement the isolated systemone adapter: request mapping, local limits, strict response validation, and bounded HTTP transport.
5. Add constructor and registry support, then the CLI flags and the environment fallback.
6. Add end-to-end CLI tests using httptest.Server.
7. Update the canonical architecture document, both README files, and docs/README.md per Section 8.
8. Inspect git diff -- cmd/hufu internal for unintended team-specific policy or internal/team changes.
9. Run go test ./..., go vet ./..., and golangci-lint run. Fix failures and rerun.

The change is complete when all of the following hold:

- all three backends remain explicitly selectable;
- the request, result, and core Backend contracts and DecisionEngine behavior are unchanged, apart from the three exported additions in Section 1;
- the systemone choice, boolean, and integer mapping, the local limits, and the raw-confidence policy pass the local tests;
- sidecar prompt bytes are unchanged;
- the timeouts follow Section 2.1;
- the documentation matches the code;
- all validation commands pass.

## Appendix A. Live wire verification (2026-10-01)

These facts are informative and not a test dependency. They were observed with Ollama 0.35.0 and the nimble 9B model on two machines: a LAN GPU host, and the CPU-only (6-core) development machine. The requests used the exact shapes from Section 4.

Response shapes:

- The top-level response has model, answers, and usage.
- A choice answer has type, choice, probabilities, and confidence. Probabilities are full float precision. The sum error was at most 2.2e-16 for 2, 3, and 21 candidates.
- A noul answer has only type and noul; it has no confidence field.

Provider confidence differs from the selected probability, which is why Section 5 never uses it:

| Candidates | Selected probability | Provider confidence |
| --- | --- | --- |
| 2 (0.507 / 0.493) | 0.507 | 0.00014 |
| 21 | 0.539 | 0.532 |

Opaque keys versus semantic keys: one support-ticket example selected the same answer in both cases, with selected probability 0.9909 for o00, o01, o02 and 0.9902 for billing, technical, other. This is one sample, not a benchmark.

Errors:

- An unknown model returns HTTP 404 with `{"error":"model \"…\" not found, try pulling it first"}`.
- An unknown question type, missing questions, and a one-criterion choice return HTTP 400 with `{"error":"…"}`, for example "criteria must contain 2–26 candidates" and "questions must contain 1–64 fields".

Latency is not a contract:

| Host | Condition | Latency |
| --- | --- | --- |
| GPU | warm, noul or 2–3 candidates | 0.2–0.9s |
| GPU | warm, 21 candidates, no prompt cache | 1.8–2.1s |
| GPU | cold model load | 12.3s |
| CPU-only | warm | 17–45s |
| CPU-only | cold | several minutes; Ollama's own 300s load timeout fired once |

Ollama honors a keep_alive request field on this endpoint, but that field is an Ollama extension and not part of the decision protocol, so the adapter must not send it (Section 6).

For comparison, the existing sidecar backend with gemma4:e4b on the same GPU host decided in 0.5–1.0s when warm, and it always abstained under any --min-confidence because its results carry ConfidenceNone.
