package utils

import (
	"bytes"
	"encoding/json"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// SecretRedactor is the process-memory redaction boundary used by
// persistence helpers. Implementations must never serialize their exact
// secret values.
type SecretRedactor interface {
	RedactText(string) string
	RedactJSON([]byte) ([]byte, error)
}

var processRedactors struct {
	sync.RWMutex
	items []SecretRedactor
}

// RegisterSecretRedactor adds a process-local redactor. Redactors are
// additive so concurrently active coordinators cannot remove one another's
// protections from shared persistence paths.
func RegisterSecretRedactor(redactor SecretRedactor) {
	if redactor == nil {
		return
	}
	processRedactors.Lock()
	defer processRedactors.Unlock()
	value := reflect.ValueOf(redactor)
	if value.Kind() == reflect.Pointer {
		for _, existing := range processRedactors.items {
			other := reflect.ValueOf(existing)
			if other.Kind() == reflect.Pointer && other.Pointer() == value.Pointer() {
				return
			}
		}
	}
	processRedactors.items = append(processRedactors.items, redactor)
}

const redactedSecret = "[REDACTED]"

var (
	// Matches YAML and JSON key/value pairs for the credential names commonly
	// passed through agent prompts. The value is deliberately limited to one
	// line: multiline configuration bodies are not credentials by themselves.
	secretKeyValueRe      = regexp.MustCompile(`(?im)(\b(?:[a-z0-9_.-]*(?:password|passwd|secret|token|credential|api[_-]?key|access[_-]?key|private[_-]?key)[a-z0-9_.-]*)\b\s*[:=]\s*)(\\"(?:\\.|[^"\\])*\\"|"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,}\]"']+)`)
	secretJSONRe          = regexp.MustCompile(`(?i)("[a-z0-9_.-]*(?:password|passwd|secret|token|credential|api[_-]?key|access[_-]?key|private[_-]?key)[a-z0-9_.-]*"\s*:\s*)("(?:\\.|[^"\\])*")`)
	secretAuthorizationRe = regexp.MustCompile(`(?im)(\bauthorization\s*:\s*(?:bearer|basic)\s+)([^\s]+)`)
	privateKeyBlockRe     = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// Environment assignments may use all-caps names and optional export.
	secretEnvRe     = regexp.MustCompile(`(?m)(\b(?:export\s+)?[A-Z][A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|CREDENTIAL|API_KEY|ACCESS_KEY|PRIVATE_KEY)[A-Z0-9_]*=)([^\s]+)`)
	secretKeyNameRe = regexp.MustCompile(`(?i)(?:password|passwd|secret|token|credential|api[_-]?key|access[_-]?key|private[_-]?key)`)
	// Prefix-only credential formats. Each pattern is anchored to a known
	// vendor prefix and a strict character class so it never matches
	// ordinary identifiers, git SHAs, UUIDs, or the [REDACTED] marker.
	// openAISecretRe matches `sk-` followed by exactly 48 alphanumeric
	// characters (total length 51). The body class forbids `-` and `_` so a
	// trailing `sk-proj-…` token is left for openAIProjSecretRe instead.
	openAISecretRe = regexp.MustCompile(`\bsk-[A-Za-z0-9]{48}\b`)
	// openAIProjSecretRe matches `sk-proj-` followed by ≥20 characters drawn
	// from `[A-Za-z0-9_-]`. The first body character must be alphanumeric or
	// `_` so a string of dashes cannot match.
	openAIProjSecretRe = regexp.MustCompile(`\bsk-proj-[A-Za-z0-9_][A-Za-z0-9_-]{19,}`)
	// githubPATSecretRe matches `ghp_` followed by exactly 36 alphanumeric
	// characters.
	githubPATSecretRe = regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`)
	// awsAccessKeyIDRe matches `AKIA` followed by exactly 16 uppercase
	// alphanumeric characters.
	awsAccessKeyIDRe = regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`)
	// slackTokenRe matches `xoxb-` followed by at least two `-`-separated
	// segments; the body-length floor is enforced in code.
	slackTokenRe = regexp.MustCompile(`(?i)\bxoxb-[A-Za-z0-9]+(?:-[A-Za-z0-9]+)+`)
)

// secretKeyNameLiterals spells out every lowercase string secretKeyNameRe
// accepts. Each credential pattern above requires one of them, so text that
// contains none cannot match and the patterns can be skipped.
var secretKeyNameLiterals = []string{
	"password", "passwd", "secret", "token", "credential",
	"apikey", "api_key", "api-key",
	"accesskey", "access_key", "access-key",
	"privatekey", "private_key", "private-key",
}

// credentialPrefixLiterals spells out every vendor prefix the prefix-only
// credential patterns above are anchored to. None of these literals appears
// in the redactedSecret marker, so text that contains only already-redacted
// content cannot match and the patterns can be skipped.
var credentialPrefixLiterals = []string{
	"sk-",
	"sk-proj-",
	"ghp_",
	"akia",
	"xoxb-",
}

// mayContainCredentialPrefix is the cheap literal gate that decides whether
// the prefix-only credential patterns need to run. Each pattern above is
// strictly anchored to one of these prefixes; ordinary text that contains
// none cannot match, and running five regexes over multi-megabyte session
// state on every checkpoint would dwarf the cost of a substring scan.
func mayContainCredentialPrefix(content string) bool {
	lower := strings.ToLower(content)
	for _, literal := range credentialPrefixLiterals {
		if strings.Contains(lower, literal) {
			return true
		}
	}
	return false
}

// redactSlackTokenLengthGate replaces a Slack token match only when the
// body is at least 16 characters long. The slackTokenRe regex itself
// enforces the multi-segment shape; the body-length floor distinguishes a
// real token from a short, dash-separated identifier that happens to share
// the prefix.
func redactSlackTokenLengthGate(match string) string {
	body := match[len("xoxb-"):]
	if len(body) >= 16 {
		return redactedSecret
	}
	return match
}

// containsSecretKeyName reports whether secretKeyNameRe matches content. The
// case-insensitive alternation has no literal prefix, so RE2 walks it at every
// input position; persisting session state runs it over megabytes of task
// output on every checkpoint. A substring search rejects most text first, and
// the regex decides the rest.
func containsSecretKeyName(content string) bool {
	return mayContainSecretKeyName(content) && secretKeyNameRe.MatchString(content)
}

// mayContainSecretKeyName is false only when secretKeyNameRe cannot match.
// RE2's (?i) folds ASCII case, U+212A KELVIN SIGN (to k), and U+017F LATIN
// SMALL LETTER LONG S (to s) onto the key-name letters. strings.ToLower maps
// the Kelvin sign to k but leaves the long s alone, so input containing it is
// passed on. ToLower also maps a few runes the regex does not fold, such as
// U+0130 to i; those only pass extra input on to the regex.
func mayContainSecretKeyName(content string) bool {
	if strings.ContainsRune(content, 'ſ') {
		return true
	}
	lower := strings.ToLower(content)
	for _, literal := range secretKeyNameLiterals {
		if strings.Contains(lower, literal) {
			return true
		}
	}
	return false
}

// Numeric token counters are telemetry, not credentials. Keep this exception
// explicit: every other scalar below a secret-looking key is redacted,
// regardless of its JSON type.
var numericTelemetryKeys = map[string]struct{}{
	"max_tokens":                      {},
	"requested_tokens":                {},
	"available_tokens":                {},
	"reserved_tokens":                 {},
	"safety_tokens":                   {},
	"window_tokens":                   {},
	"diagnostic_max_tokens":           {},
	"diagnostic_max_lines":            {},
	"tokens_used":                     {},
	"used_tokens":                     {},
	"used_duration_ms":                {},
	"tokens_since_progress":           {},
	"tokens_since_criterion_progress": {},
	"max_tokens_without_progress":     {},
	"tokens_before":                   {},
	"tokens_after":                    {},
	"last_requested_tokens":           {},
	"last_available_tokens":           {},
	// Execution-event usage counters (team.ExecutionUsage). Debug bundles
	// redact JSONL, so without these entries every usage count became
	// "[REDACTED]" and cost analysis from a bundle was impossible.
	"input_tokens":          {},
	"output_tokens":         {},
	"total_tokens":          {},
	"progress_tokens":       {},
	"cache_read_tokens":     {},
	"cache_creation_tokens": {},
	// Cost reservation bounds are typed numeric telemetry. Keeping these
	// numbers intact is required for durable replay; arbitrary values under
	// other token-named keys remain redacted.
	"estimated_input_tokens": {},
	"reserved_output_tokens": {},
	// token_count is the per-item token estimate in a memory injection
	// manifest. It matches the secret-key regex ("token") but is numeric
	// telemetry, not a credential; redacting it corrupts session.json and
	// makes the manifest unparseable.
	"token_count": {},
	"tokens":      {},
	// Universal decision-runtime limit frozen into correctness events. This is
	// a numeric budget, not a credential value.
	"total_decision_tokens":      {},
	"total-decision-tokens":      {},
	"max-evidence-input-tokens":  {},
	"per-call-max-output-tokens": {},
}

// Structured telemetry keys contain a bounded metadata object rather than a
// scalar token count. They are produced by secret-free DTOs and must remain
// structured so durable telemetry can be decoded after redaction.
var structuredTelemetryKeys = map[string]struct{}{
	"max_output_tokens": {},
}

var numericTelemetrySources = map[string]struct{}{
	"operator":          {},
	"provider_runtime":  {},
	"provider_metadata": {},
	"model_config":      {},
	"catalog":           {},
	"provider_observed": {},
	"fallback":          {},
}

var numericTelemetryConfidences = map[string]struct{}{
	"high":      {},
	"medium":    {},
	"estimated": {},
	"observed":  {},
}

// Safe policy metadata describes how a caller handles credentials; it is not
// itself a credential. Keep the exception deliberately narrow and
// value-constrained so a secret placed under one of these keys is still
// redacted. Tool schemas commonly expose this metadata to let an agent choose
// an environment reference instead of a literal value. Redacting the policy
// makes the safety boundary self-defeating.
var safeSecretMetadataValues = map[string]map[string]struct{}{
	"secret_handling": {
		"none":                  {},
		"value_env_recommended": {},
		"value_env_required":    {},
	},
}

// learnedSecrets remembers credential values that were already recognized
// beside their key. The patterns above can only redact a value that still
// carries its key, but an agent legitimately reads a credential and the value
// then travels alone: `grep '^admin_password:' vault | sed 's/.*: //'` returns
// the bare secret, a TUI echoes it a character at a time, a generated script
// embeds it as a positional argument. None of those shapes carry a key, so a
// value recognized once must stay redacted everywhere it appears afterwards.
var learnedSecrets struct {
	sync.RWMutex
	values map[string]*regexp.Regexp
	order  []string
}

const (
	// Short values are far more likely to be a shared word, an enum, or a
	// version than a credential, and redacting one poisons every log that
	// happens to contain it.
	minLearnedSecretLen = 8
	// Bound the set so a long run cannot grow it without limit; the oldest
	// entries are evicted first.
	maxLearnedSecrets = 256
	// Longest value for which prefix matching is built at all. A value this
	// long is not something a human types into an echoing prompt, so only the
	// exact value is matched.
	maxLearnedSecretPrefixLen = 64
	// How many trailing characters a match may be missing. Interactive TUIs
	// echo a typed secret one character at a time, so a log ends up holding
	// near-complete prefixes, and one character short of a password is still
	// effectively the password.
	learnedSecretPrefixSlack = 2
	// Floor on the mandatory part of a prefix match. Without it a credential
	// that happens to begin with an ordinary word rewrites unrelated text:
	// learning "protocol-secret" with an 8-character floor redacted the word
	// "protocol" everywhere it appeared, including out of a run ID. Corrupting
	// evidence is the failure mode this whole file exists to avoid.
	minLearnedSecretPrefixLen = 12
)

// learnedSecretPattern matches a credential value, or a prefix of it that is
// within learnedSecretPrefixSlack characters of the whole value. The optional
// tail is greedy, so the fullest occurrence present is what gets replaced.
func learnedSecretPattern(value string) *regexp.Regexp {
	exact := regexp.MustCompile(regexp.QuoteMeta(value))
	runes := []rune(value)
	if len(runes) > maxLearnedSecretPrefixLen {
		return exact
	}
	mandatory := len(runes) - learnedSecretPrefixSlack
	if mandatory < minLearnedSecretPrefixLen {
		// Too short for a prefix match to be distinguishable from ordinary
		// text; the exact value is all that can be matched safely.
		return exact
	}
	tail := ""
	for i := len(runes) - 1; i >= mandatory; i-- {
		tail = "(?:" + regexp.QuoteMeta(string(runes[i])) + tail + ")?"
	}
	compiled, err := regexp.Compile(regexp.QuoteMeta(string(runes[:mandatory])) + tail)
	if err != nil {
		return exact
	}
	return compiled
}

// learnSecretValue records a credential value for value-based redaction.
// It is deliberately conservative: a false positive here silently corrupts
// unrelated diagnostics, which is worse than missing one bare occurrence.
func learnSecretValue(raw string) {
	// Model prose quotes code with Markdown backticks, so `install.go` or
	// `1048576` arrives with a backtick stuck to one end. No credential
	// starts or ends with one, and leaving it on hides the value's shape.
	value := strings.Trim(unquoteSecretValue(raw), "`")
	if !isLearnableSecret(value) {
		return
	}
	learnedSecrets.Lock()
	defer learnedSecrets.Unlock()
	if _, exists := learnedSecrets.values[value]; exists {
		return
	}
	if learnedSecrets.values == nil {
		learnedSecrets.values = make(map[string]*regexp.Regexp)
	}
	learnedSecrets.values[value] = learnedSecretPattern(value)
	learnedSecrets.order = append(learnedSecrets.order, value)
	for len(learnedSecrets.order) > maxLearnedSecrets {
		delete(learnedSecrets.values, learnedSecrets.order[0])
		learnedSecrets.order = learnedSecrets.order[1:]
	}
}

func unquoteSecretValue(raw string) string {
	value := strings.TrimSpace(raw)
	for _, quote := range []string{`\"`, `"`, `'`} {
		if len(value) > 2*len(quote) && strings.HasPrefix(value, quote) && strings.HasSuffix(value, quote) {
			return strings.TrimSpace(value[len(quote) : len(value)-len(quote)])
		}
	}
	return value
}

func isLearnableSecret(value string) bool {
	if len(value) < minLearnedSecretLen {
		return false
	}
	// Never learn the redaction marker, whole or partial. Redaction has to be
	// idempotent, and re-redacting already-redacted text feeds this function a
	// truncated marker: the key/value pattern excludes "]" from a value, so
	// "api_token=[REDACTED]" yields "[REDACTED" without its bracket. Learning
	// that turned every later redaction into "[REDACTED]]".
	if strings.Contains(value, redactedSecret) || strings.Contains(redactedSecret, value) {
		return false
	}
	// A number under a token-shaped key is a counter, not a credential —
	// `tokens_since_progress: 1048576` is the common case, and source code
	// writes the same thing as `MaxTokensWithoutProgress: 2_000_000`.
	// Redacting that number would rewrite unrelated telemetry.
	if numericLiteralRe.MatchString(value) {
		return false
	}
	// Unresolved references and fill-me markers are not yet secrets, and
	// redacting them would hide the fact that they were never filled in.
	for _, placeholder := range []string{"${", "{{", "<FILL", "CHANGE-ME", "CHANGEME", "changeme", "change-me", "REPLACE_ME", "xxxxx", "*****"} {
		if strings.Contains(value, placeholder) {
			return false
		}
	}
	// A filesystem path or URL names a location, not a credential; redacting
	// one would destroy exactly the evidence a failed run is read for.
	if strings.HasPrefix(value, "/") || strings.HasPrefix(value, "./") || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "~/") ||
		strings.Contains(value, "://") || fileReferenceRe.MatchString(value) {
		return false
	}
	return !looksLikeSourceText(value)
}

var (
	numericLiteralRe = regexp.MustCompile(`^[0-9][0-9_.,]*$`)
	// fileReferenceRe is a file name or relative path ending in a short
	// extension, optionally with a line and column: `team.yaml`,
	// `docs/reference/action-providers.md`, `internal/team/runtime.go:42`.
	fileReferenceRe = regexp.MustCompile(`^[A-Za-z0-9_./-]*[A-Za-z0-9_-]\.[a-z][a-z0-9]{0,4}(?::[0-9]+)*$`)
	// callOrIndexRe is a name followed by a call or an index: `ToSlash(`,
	// `append(`, `token[`.
	callOrIndexRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*[(\[]`)
	// selectorChainRe is a dotted chain of names without digits, such as the
	// Go selector `cfg.ProviderAPIKey`. Requiring every segment to be letters
	// keeps a JWT, whose base64 segments carry digits, learnable.
	selectorChainRe = regexp.MustCompile(`^[A-Za-z_]+(?:\.[A-Za-z_]+)+$`)
)

// looksLikeSourceText reports a value whose shape says it is a word or a
// piece of code that merely sits beside a credential-named key. Reviewing
// source code feeds this file lines such as `token = filepath.ToSlash(token)`
// and `providerAPIKey: providerConfig.ProviderAPIKey`, and a model reasoning
// about it wrote `secretKeyValueRe: requires key names`. Learning those
// values redacted the word "requires", a document path, and a workset binding
// out of every later durable record, and each rewrite changed the record's
// idempotency key, so unchanged tasks were appended to the event store again.
func looksLikeSourceText(value string) bool {
	// A value made only of letters is a word or an identifier, such as
	// `requires` or `requestTokens`. A credential like that is still redacted
	// beside its key; it is only not matched when it later appears alone.
	if strings.IndexFunc(value, func(r rune) bool { return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') }) < 0 {
		return true
	}
	// The middle of a string concatenation: `"api_token=" + secret + "\n"`.
	if strings.HasPrefix(value, "+ ") || strings.HasSuffix(value, " +") {
		return true
	}
	return callOrIndexRe.MatchString(value) || selectorChainRe.MatchString(value)
}

// learnSecretsFrom scans content for key/value credential shapes and records
// their values. The cheap key-name pre-filter keeps this off the hot path for
// the overwhelming majority of content, which mentions no credential at all.
func learnSecretsFrom(content string) {
	if !mayContainSecretKeyName(content) && !strings.Contains(strings.ToLower(content), "authorization") {
		return
	}
	for _, re := range []*regexp.Regexp{secretKeyValueRe, secretJSONRe, secretEnvRe, secretAuthorizationRe} {
		for _, match := range re.FindAllStringSubmatch(content, -1) {
			if len(match) == 3 {
				if (re == secretKeyValueRe || re == secretJSONRe) &&
					safeSecretMetadataValue(secretKeyFromPrefix(match[1]), unquoteSecretValue(match[2])) {
					continue
				}
				learnSecretValue(match[2])
			}
		}
	}
}

// redactLearnedSecrets replaces every remembered credential value, wherever it
// appears and whether or not it still has a key beside it.
func redactLearnedSecrets(content string) string {
	learnedSecrets.RLock()
	patterns := make([]*regexp.Regexp, 0, len(learnedSecrets.order))
	prefixes := make([]string, 0, len(learnedSecrets.order))
	for _, value := range learnedSecrets.order {
		if pattern := learnedSecrets.values[value]; pattern != nil {
			patterns = append(patterns, pattern)
			prefixes = append(prefixes, learnedSecretProbe(value))
		}
	}
	learnedSecrets.RUnlock()
	for i, pattern := range patterns {
		// The literal probe is a cheap reject: scanning for a fixed substring
		// is far cheaper than running the prefix-chain regex over content that
		// cannot contain the secret at all.
		if !strings.Contains(content, prefixes[i]) {
			continue
		}
		content = pattern.ReplaceAllString(content, redactedSecret)
	}
	return content
}

// learnedSecretProbe is the shortest literal that any match of the value's
// pattern must contain, used as a cheap reject before running the pattern.
func learnedSecretProbe(value string) string {
	runes := []rune(value)
	mandatory := len(runes) - learnedSecretPrefixSlack
	if len(runes) > maxLearnedSecretPrefixLen || mandatory < minLearnedSecretPrefixLen {
		return value
	}
	return string(runes[:mandatory])
}

// RedactSecrets removes recognizable credential values before content is
// persisted to workspace logs or session state. It intentionally preserves
// keys and surrounding prose so diagnostics remain useful.
func RedactSecrets(content string) string {
	return redactSecretsText(content, true)
}

// redactSecretsText is RedactSecrets with the learning pass optional. The JSON
// walk learns from every string in discoverJSONSecrets before it redacts any,
// so learning again from each string would only repeat that work.
func redactSecretsText(content string, learn bool) string {
	// Every key-based pattern below requires a credential key name, and the
	// authorization pattern requires its header name. Checking for those
	// literals once skips the patterns for the ordinary text that has neither.
	// The replacements only ever insert redactedSecret, so an earlier pass
	// cannot introduce a match for a later one.
	hasKeyName := mayContainSecretKeyName(content)
	hasAuthorization := strings.Contains(strings.ToLower(content), "authorization")
	// Learn before redacting: the passes below destroy the very values that a
	// later bare occurrence has to be matched against.
	if learn && (hasKeyName || hasAuthorization) {
		learnSecretsFrom(content)
	}
	if strings.Contains(content, "PRIVATE KEY-----") {
		content = privateKeyBlockRe.ReplaceAllString(content, redactedSecret)
	}
	// Prefix-only credential formats (OpenAI `sk-`/`sk-proj-`, GitHub `ghp_`,
	// AWS `AKIA`, Slack `xoxb-`) are matched without an adjacent
	// credential-named key. Running this pass after the private-key block
	// pass keeps PEM armor intact, and running it before the
	// authorization/JSON/key/env passes means a value that appears both bare
	// and under a key in the same chunk is redacted on the first occurrence
	// regardless of which form comes first. The marker [REDACTED] does not
	// contain any prefix literal, so the literal gate rejects re-process
	// inputs cheaply.
	if mayContainCredentialPrefix(content) {
		content = openAISecretRe.ReplaceAllString(content, redactedSecret)
		content = openAIProjSecretRe.ReplaceAllString(content, redactedSecret)
		content = githubPATSecretRe.ReplaceAllString(content, redactedSecret)
		content = awsAccessKeyIDRe.ReplaceAllString(content, redactedSecret)
		content = slackTokenRe.ReplaceAllStringFunc(content, redactSlackTokenLengthGate)
	}
	if hasAuthorization {
		content = secretAuthorizationRe.ReplaceAllString(content, "${1}"+redactedSecret)
	}
	if hasKeyName {
		content = secretJSONRe.ReplaceAllStringFunc(content, redactJSONKeyValue)
		content = secretKeyValueRe.ReplaceAllStringFunc(content, redactKeyValue)
		content = secretEnvRe.ReplaceAllString(content, "${1}"+redactedSecret)
	}
	content = redactLearnedSecrets(content)
	processRedactors.RLock()
	redactors := append([]SecretRedactor(nil), processRedactors.items...)
	processRedactors.RUnlock()
	for _, redactor := range redactors {
		content = redactor.RedactText(content)
	}
	return content
}

// RedactJSON redacts secrets in a JSON document without applying text
// substitutions to the JSON syntax itself. String values are redacted as
// prose, while credential-looking object keys have their values replaced.
// The returned document is always freshly marshaled JSON.
func RedactJSON(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	discoverJSONSecrets(value, "")
	value = redactJSONValue(value, "")
	return json.MarshalIndent(value, "", "  ")
}

// RedactJSONCompact is RedactJSON re-marshaled compactly. It is intended for
// JSONL records where MarshalIndent's newlines would break the
// one-record-per-line framing. Like RedactJSON, it decodes the document first
// so escaped string values are unescaped before redaction, which text redaction
// applied to the raw marshaled bytes would miss.
func RedactJSONCompact(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	discoverJSONSecrets(value, "")
	value = redactJSONValue(value, "")
	return json.Marshal(value)
}

// RedactJSONFileData redacts a single JSON document with JSON-aware redaction.
// It returns the original bytes unchanged when redaction did not change the
// content semantically, so files without secrets are not reformatted.
func RedactJSONFileData(data []byte) []byte {
	redacted, err := RedactJSON(data)
	if err != nil {
		return []byte(RedactSecrets(string(data)))
	}
	if jsonSemanticEqual(data, redacted) {
		return data
	}
	return redacted
}

// RedactJSONLData redacts a JSONL stream line by line with JSON-aware
// redaction. Lines that do not change semantically are preserved verbatim so
// records without secrets keep their original formatting; non-JSON lines fall
// back to text redaction.
func RedactJSONLData(data []byte) []byte {
	lines := bytes.Split(data, []byte{'\n'})
	var out bytes.Buffer
	changed := false
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			r := []byte(RedactSecrets(string(line)))
			if !bytes.Equal(line, r) {
				changed = true
			}
			out.Write(r)
			continue
		}
		redacted, err := RedactJSONCompact(trimmed)
		if err != nil {
			r := []byte(RedactSecrets(string(line)))
			if !bytes.Equal(line, r) {
				changed = true
			}
			out.Write(r)
			continue
		}
		if !jsonSemanticEqual(trimmed, redacted) {
			changed = true
		}
		out.Write(redacted)
	}
	if !changed {
		return data
	}
	return out.Bytes()
}

// jsonSemanticEqual reports whether two JSON byte sequences decode to the same
// value, ignoring whitespace and key-ordering differences introduced by
// re-marshaling.
func jsonSemanticEqual(a, b []byte) bool {
	var va, vb any
	if err := json.Unmarshal(a, &va); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &vb); err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

// discoverJSONSecrets completes the credential-key discovery pass before any
// string values are redacted. This keeps learned-secret behavior independent
// of the unspecified iteration order of decoded JSON objects.
func discoverJSONSecrets(value any, key string) {
	if key != "" && containsSecretKeyName(key) {
		if safeSecretMetadataValue(key, value) {
			return
		}
		if isNumericTelemetryValue(key, value) {
			return
		}
		_, telemetry := numericTelemetryKeys[strings.ToLower(key)]
		if !telemetry {
			if text, ok := value.(string); ok {
				learnSecretValue(text)
			}
			return
		}
		if _, numeric := value.(json.Number); !numeric {
			return
		}
	}
	switch v := value.(type) {
	case string:
		learnSecretsFrom(v)
	case []any:
		for _, item := range v {
			discoverJSONSecrets(item, "")
		}
	case map[string]any:
		for k, item := range v {
			discoverJSONSecrets(item, k)
		}
	}
}

// IsRedactedJSONKey reports whether RedactJSON replaces the value stored
// under key regardless of the value's content. Numeric telemetry keys such
// as max_tokens keep numeric values and are not reported. Callers that must
// keep a JSON document byte-stable across event redaction use it to reject
// such keys up front.
func IsRedactedJSONKey(key string) bool {
	if key == "" || !containsSecretKeyName(key) {
		return false
	}
	_, telemetry := numericTelemetryKeys[strings.ToLower(key)]
	return !telemetry
}

func redactJSONValue(value any, key string) any {
	if key != "" && containsSecretKeyName(key) {
		if safeSecretMetadataValue(key, value) {
			return value
		}
		if isNumericTelemetryValue(key, value) {
			return redactJSONValue(value, "")
		}
		_, telemetry := numericTelemetryKeys[strings.ToLower(key)]
		if !telemetry {
			// A credential recognized by its JSON key must stay redacted when
			// the same value later shows up as bare text in a log or prompt.
			if text, ok := value.(string); ok {
				learnSecretValue(text)
			}
			return redactedSecret
		}
		if _, numeric := value.(json.Number); !numeric {
			return redactedSecret
		}
	}
	switch v := value.(type) {
	case string:
		return redactSecretsText(v, false)
	case []any:
		for i := range v {
			v[i] = redactJSONValue(v[i], "")
		}
	case map[string]any:
		for k, item := range v {
			v[k] = redactJSONValue(item, k)
		}
	}
	return value
}

// isNumericTelemetryValue accepts only the serialized shape of a numeric
// TelemetryValue. In particular, max_output_tokens must contain an integer
// value and may contain only the string metadata fields emitted by the
// telemetry DTO. Anything else remains a secret-looking value and is handled
// by the generic redaction path above.
func isNumericTelemetryValue(key string, value any) bool {
	if _, structured := structuredTelemetryKeys[strings.ToLower(key)]; !structured {
		return false
	}
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	rawValue, ok := object["value"]
	if !ok {
		return false
	}
	number, ok := rawValue.(json.Number)
	if !ok {
		return false
	}
	if _, err := strconv.Atoi(number.String()); err != nil {
		return false
	}
	for field, fieldValue := range object {
		switch field {
		case "value":
			// Validated above.
		case "source", "provenance":
			if !validNumericTelemetryLabel(fieldValue, numericTelemetrySources) {
				return false
			}
		case "confidence":
			if !validNumericTelemetryLabel(fieldValue, numericTelemetryConfidences) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func validNumericTelemetryLabel(value any, allowed map[string]struct{}) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return true
	}
	_, ok = allowed[text]
	return ok
}

func redactKeyValue(match string) string {
	parts := secretKeyValueRe.FindStringSubmatch(match)
	if len(parts) != 3 {
		return match
	}
	// A source-code location reference (`secrets.go:123`,
	// `internal/team/token_store.go:45:9`) carries a key whose own word is a
	// credential name and a value that is just a line number or
	// line-and-column. Treating the pair as a credential rewrites evidence a
	// failed run is read for; let it through unchanged.
	if fileReferenceRe.MatchString(match) {
		return match
	}
	value := parts[2]
	if safeSecretMetadataValue(secretKeyFromPrefix(parts[1]), unquoteSecretValue(value)) {
		return match
	}
	// Already redacted. Redaction has to be idempotent because redacted text is
	// re-redacted on the way to several surfaces, and the value pattern excludes
	// "]" — so a second pass over "api_token=[REDACTED]" captures "[REDACTED"
	// and re-appending the marker produced "api_token=[REDACTED]]".
	if unquoted := unquoteSecretValue(value); strings.Contains(unquoted, redactedSecret) || strings.Contains(redactedSecret, unquoted) {
		return match
	}
	if strings.HasPrefix(value, `"`) {
		return parts[1] + `"` + redactedSecret + `"`
	}
	if strings.HasPrefix(value, `\"`) {
		return parts[1] + `\"` + redactedSecret + `\"`
	}
	if strings.HasPrefix(value, "'") {
		return parts[1] + "'" + redactedSecret + "'"
	}
	return parts[1] + redactedSecret
}

func redactJSONKeyValue(match string) string {
	parts := secretJSONRe.FindStringSubmatch(match)
	if len(parts) != 3 {
		return match
	}
	value := unquoteSecretValue(parts[2])
	if safeSecretMetadataValue(secretKeyFromPrefix(parts[1]), value) {
		return match
	}
	learnSecretValue(value)
	return parts[1] + `"` + redactedSecret + `"`
}

func secretKeyFromPrefix(prefix string) string {
	if index := strings.IndexAny(prefix, ":="); index >= 0 {
		prefix = prefix[:index]
	}
	return strings.ToLower(strings.Trim(strings.TrimSpace(prefix), "\"'\\"))
}

func safeSecretMetadataValue(key string, value any) bool {
	allowed := safeSecretMetadataValues[strings.ToLower(strings.TrimSpace(key))]
	if len(allowed) == 0 {
		return false
	}
	text, ok := value.(string)
	if !ok {
		return false
	}
	_, ok = allowed[strings.ToLower(strings.TrimSpace(text))]
	return ok
}
