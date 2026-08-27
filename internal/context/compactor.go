package context

// Verified, deterministic context compaction.  This code deliberately lives
// beside the canonical model instead of the coordinator: callers hand it
// structured ContextItems and receive a result that can be validated without
// trusting an LLM summary.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var ErrCompactionBudget = errors.New("context compaction cannot fit required evidence in token budget")

var (
	toolExitStatusPattern   = regexp.MustCompile(`(?i)exit (?:status|code)\s+(\d+)`)
	toolEvidencePathPattern = regexp.MustCompile(`(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+`)
)

// ErrToolEvidenceMandatoryCapacity indicates that the provider-visible part
// of a tool result cannot fit the configured output caps. Callers must retain
// the original paired messages when this occurs.
var ErrToolEvidenceMandatoryCapacity = errors.New("tool-result mandatory provenance cannot fit configured output caps")

// ErrToolEvidenceToolCallMismatch indicates that a result was supplied with a
// non-empty tool-call ID that does not identify the supplied call.
var ErrToolEvidenceToolCallMismatch = errors.New("tool-result tool_call_id does not match tool call")

// ToolResultCapacity describes the three independent limits used for tool
// result evidence.
type ToolResultCapacity struct {
	Bytes  int
	Runes  int
	Tokens int
}

// ToolResultMandatoryEnvelope is the canonical provider-visible provenance
// envelope. Optional output is appended only after this exact envelope fits.
func ToolResultMandatoryEnvelope(tool, toolCallID, command, workingDir, verification string) string {
	return fmt.Sprintf("tool: %s\ntool_call_id: %s\ncommand: %s\nworking_dir: %s\nverification: %s", tool, toolCallID, command, workingDir, verification)
}

// ToolResultMandatoryMinimum returns the static minimum capacity for the
// current canonical non-empty call ID and the required "passed" verification.
// Keeping this derived from ToolResultMandatoryEnvelope makes the policy
// boundary and rendering boundary share one source of truth.
func ToolResultMandatoryMinimum() ToolResultCapacity {
	callID := canonicalToolEvidenceID("x")
	envelope := ToolResultMandatoryEnvelope("x", callID, "x", "x", "passed")
	return ToolResultCapacity{Bytes: len(envelope), Runes: len([]rune(envelope)), Tokens: estimateOutputTokens(envelope)}
}

type ToolEvidenceCapacityError struct {
	Required ToolResultCapacity
	Limit    ToolResultCapacity
}

func (e *ToolEvidenceCapacityError) Error() string {
	return fmt.Sprintf("%v: required bytes=%d runes=%d tokens=%d, limits bytes=%d runes=%d tokens=%d", ErrToolEvidenceMandatoryCapacity, e.Required.Bytes, e.Required.Runes, e.Required.Tokens, e.Limit.Bytes, e.Limit.Runes, e.Limit.Tokens)
}

func (e *ToolEvidenceCapacityError) Unwrap() error { return ErrToolEvidenceMandatoryCapacity }

type CompactionRequest struct {
	Items        []ContextItem
	Edges        []ContextEdge
	TargetTokens int
}

type CompactionResult struct {
	Content          string   `json:"content"`
	SourceItemIDs    []string `json:"source_item_ids"`
	PreservedItemIDs []string `json:"preserved_item_ids"`
	OmittedItemIDs   []string `json:"omitted_item_ids"`
	MissingItemIDs   []string `json:"missing_item_ids"`
	SourceTokens     int      `json:"source_tokens"`
	OutputTokens     int      `json:"output_tokens"`
	CompressionRatio float64  `json:"compression_ratio"`
	ValidationScore  float64  `json:"validation_score"`
	UsedFallback     bool     `json:"used_fallback"`
	Strategy         string   `json:"strategy"`
}

// Compactor is intentionally small so an LLM implementation can be injected
// later.  A successful implementation must still pass ValidateCompaction.
type Compactor interface {
	Compact(context.Context, CompactionRequest) (CompactionResult, error)
}

type DeterministicCompactor struct{}

// ValidatedCompactor is the boundary for LLM-backed compactors.  The delegate
// receives only ContextItems, and a non-empty prose answer is not considered
// success unless it declares the source/preserved IDs and passes validation.
type ValidatedCompactor struct{ Delegate Compactor }

func (c ValidatedCompactor) Compact(ctx context.Context, req CompactionRequest) (CompactionResult, error) {
	if c.Delegate == nil {
		return DeterministicCompactor{}.Compact(ctx, req)
	}
	result, err := c.Delegate.Compact(ctx, req)
	if err != nil {
		return fallbackCompact(ctx, req)
	}
	if len(result.SourceItemIDs) == 0 || len(result.PreservedItemIDs) == 0 {
		return fallbackCompact(ctx, req)
	}
	if err := ValidateCompaction(req, result); err != nil {
		return fallbackCompact(ctx, req)
	}
	return result, nil
}

func fallbackCompact(ctx context.Context, req CompactionRequest) (CompactionResult, error) {
	result, err := DeterministicCompactor{}.Compact(ctx, req)
	result.UsedFallback = true
	if result.Strategy == "deterministic" {
		result.Strategy = "fallback-deterministic"
	}
	return result, err
}

type ToolCallEvidence struct {
	ID         string
	Tool       string
	Command    string
	WorkingDir string
	StartedAt  time.Time
	Scope      Scope
}

type ToolResultEvidence struct {
	ID            string
	ToolCallID    string
	Tool          string
	Command       string
	WorkingDir    string
	ExitCode      *int
	StartedAt     time.Time
	EndedAt       time.Time
	StderrSummary string
	Stdout        string
	StdoutHead    string
	StdoutTail    string
	MatchedErrors []string
	ArtifactPaths []string
	ModifiedFiles []string
	Verification  string
	Output        string // compatibility input when stdout/stderr are unavailable
	OutputPolicy  ToolOutputPolicy
	Scope         Scope
}

// ToolOutputPolicy bounds the normalized evidence inserted into verified
// context. All limits apply to the final framed value, not just the source
// excerpt. Zero values use DefaultToolOutputPolicy.
type ToolOutputPolicy struct {
	MaxBytes         int
	MaxRunes         int
	MaxTokens        int
	DiagnosticLines  int
	DiagnosticTokens int
}

func DefaultToolOutputPolicy() ToolOutputPolicy {
	return ToolOutputPolicy{MaxBytes: 24_576, MaxRunes: 6_000, MaxTokens: 1_500, DiagnosticLines: 32, DiagnosticTokens: 768}
}

func (p ToolOutputPolicy) normalized() ToolOutputPolicy {
	defaults := DefaultToolOutputPolicy()
	if p.MaxBytes == 0 {
		p.MaxBytes = defaults.MaxBytes
	}
	if p.MaxRunes == 0 {
		p.MaxRunes = defaults.MaxRunes
	}
	if p.MaxTokens == 0 {
		p.MaxTokens = defaults.MaxTokens
	}
	if p.DiagnosticLines == 0 {
		p.DiagnosticLines = defaults.DiagnosticLines
	}
	if p.DiagnosticTokens == 0 {
		p.DiagnosticTokens = defaults.DiagnosticTokens
	}
	return p
}

// ToolEvidenceItems turns a call/result pair into canonical evidence and the
// directional relationship required by the compactor and repository.
func ToolEvidenceItems(call ToolCallEvidence, result ToolResultEvidence) ([]ContextItem, []ContextEdge) {
	items, edges, err := ToolEvidenceItemsChecked(call, result)
	if err != nil {
		// Preserve the historical two-value API without ever returning clipped
		// or unlinked evidence. Production callers use the checked API below.
		return nil, nil
	}
	return items, edges
}

// ToolEvidenceItemsChecked is the error-returning form of ToolEvidenceItems.
// It is the canonical path for callers that can retain original messages on
// mandatory-provenance capacity failure.
func ToolEvidenceItemsChecked(call ToolCallEvidence, result ToolResultEvidence) ([]ContextItem, []ContextEdge, error) {
	var err error
	call, result, err = normalizeToolEvidencePair(call, result)
	if err != nil {
		return nil, nil, err
	}
	callItem := ContextItem{ID: call.ID, Kind: ContextToolCall, Content: renderToolCall(call), Scope: call.Scope, Authority: AuthorityTool, TrustLevel: TrustInternal, Priority: PriorityHigh, MustKeep: true}
	content, err := renderToolResult(result)
	if err != nil {
		return nil, nil, err
	}
	resultItem := ContextItem{ID: result.ID, Kind: ContextToolResult, Content: content, Scope: result.Scope, Authority: AuthorityTool, TrustLevel: TrustInternal, Priority: PriorityHigh, MustKeep: true}
	return []ContextItem{callItem, resultItem}, []ContextEdge{{FromID: result.ID, Relation: "tool_result_of", ToID: result.ToolCallID}}, nil
}

// normalizeToolEvidencePair is the sole owner of tool-pair identity. It runs
// before any item, edge, or rendered provenance value is constructed.
func normalizeToolEvidencePair(call ToolCallEvidence, result ToolResultEvidence) (ToolCallEvidence, ToolResultEvidence, error) {
	suppliedResultToolCallID := result.ToolCallID
	if call.ID == "" {
		call.ID = canonicalToolEvidenceID("tool-call:" + RedactSecrets(call.Tool) + ":" + RedactSecrets(call.Command))
	} else {
		call.ID = canonicalToolEvidenceID(call.ID)
	}
	if result.ID == "" {
		result.ID = canonicalToolEvidenceID("tool-result:" + call.ID)
	} else {
		result.ID = canonicalToolEvidenceID(result.ID)
	}
	if suppliedResultToolCallID != "" && canonicalToolEvidenceID(suppliedResultToolCallID) != call.ID {
		return ToolCallEvidence{}, ToolResultEvidence{}, ErrToolEvidenceToolCallMismatch
	}
	result.ToolCallID = call.ID
	if result.Tool == "" {
		result.Tool = call.Tool
	}
	if result.Command == "" {
		result.Command = call.Command
	}
	if result.WorkingDir == "" {
		result.WorkingDir = call.WorkingDir
	}
	if result.StartedAt.IsZero() {
		result.StartedAt = call.StartedAt
	}
	result = NormalizeToolResultEvidence(result)
	// NormalizeToolResultEvidence redacts fields, but identity remains the
	// canonical call ID selected above even if its redaction rules evolve.
	result.ToolCallID = call.ID
	return call, result, nil
}

func canonicalToolEvidenceID(raw string) string {
	redacted := RedactSecrets(raw)
	if redacted == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(redacted))
	return fmt.Sprintf("tool-id:%x", sum[:])
}

func renderToolCall(call ToolCallEvidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tool: %s\ncommand: %s\nworking_dir: %s", RedactSecrets(call.Tool), RedactSecrets(call.Command), RedactSecrets(call.WorkingDir))
	if !call.StartedAt.IsZero() {
		fmt.Fprintf(&b, "\nstarted_at: %s", call.StartedAt.UTC().Format(time.RFC3339Nano))
	}
	return b.String()
}

func renderToolResult(result ToolResultEvidence) (string, error) {
	p := result.OutputPolicy
	mandatory := ToolResultMandatoryEnvelope(result.Tool, result.ToolCallID, result.Command, result.WorkingDir, result.Verification)
	if err := validateMandatoryToolResultEnvelope(mandatory, p); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(mandatory)
	appendOptional := func(prefix, value string, compact bool) {
		if value == "" {
			return
		}
		base := b.String() + prefix
		if !withinToolOutputCaps(base, p) {
			return
		}
		remaining := ToolResultCapacity{Bytes: p.MaxBytes - len([]byte(base)), Runes: p.MaxRunes - len([]rune(base)), Tokens: p.MaxTokens - estimateOutputTokens(base)}
		if remaining.Bytes <= 0 || remaining.Runes <= 0 || remaining.Tokens <= 0 {
			return
		}
		if compact {
			optionalPolicy := p
			optionalPolicy.MaxBytes = remaining.Bytes
			optionalPolicy.MaxRunes = remaining.Runes
			optionalPolicy.MaxTokens = remaining.Tokens
			optionalPolicy.DiagnosticTokens = min(optionalPolicy.DiagnosticTokens, remaining.Tokens)
			value = CompactToolOutputWithPolicy(value, optionalPolicy)
		} else {
			value = limitToolOutput(value, remaining.Runes, remaining.Bytes, remaining.Tokens)
		}
		if value != "" && withinToolOutputCaps(base+value, p) {
			b.WriteString(prefix)
			b.WriteString(value)
		}
	}
	appendFixed := func(value string) {
		if value != "" && withinToolOutputCaps(b.String()+value, p) {
			b.WriteString(value)
		}
	}
	if result.ExitCode != nil {
		appendFixed("\nexit_code: " + fmt.Sprint(*result.ExitCode))
	}
	if !result.StartedAt.IsZero() {
		appendFixed("\nstarted_at: " + result.StartedAt.UTC().Format(time.RFC3339Nano))
	}
	if !result.EndedAt.IsZero() {
		appendFixed("\nended_at: " + result.EndedAt.UTC().Format(time.RFC3339Nano))
	}
	if result.StderrSummary != "" {
		appendOptional("\nstderr_summary: ", result.StderrSummary, false)
	}
	stdout := result.Stdout
	if stdout == "" {
		stdout = result.Output
	}
	if result.StdoutHead != "" {
		appendOptional("\nstdout_head:\n", result.StdoutHead, true)
	}
	if result.StdoutTail != "" {
		appendOptional("\nstdout_tail:\n", result.StdoutTail, true)
	}
	if stdout != "" {
		appendOptional("\nstdout:\n", stdout, true)
	}
	if len(result.MatchedErrors) > 0 {
		appendOptional("\nmatched_errors:\n", strings.Join(result.MatchedErrors, "\n"), false)
	}
	if len(result.ArtifactPaths) > 0 {
		appendOptional("\nartifact_paths:\n", strings.Join(result.ArtifactPaths, "\n"), false)
	}
	if len(result.ModifiedFiles) > 0 {
		appendOptional("\nmodified_files:\n", strings.Join(result.ModifiedFiles, "\n"), false)
	}
	return b.String(), nil
}

func validateMandatoryToolResultEnvelope(envelope string, p ToolOutputPolicy) error {
	required := ToolResultCapacity{Bytes: len([]byte(envelope)), Runes: len([]rune(envelope)), Tokens: estimateOutputTokens(envelope)}
	limit := ToolResultCapacity{Bytes: p.MaxBytes, Runes: p.MaxRunes, Tokens: p.MaxTokens}
	if required.Bytes > limit.Bytes || required.Runes > limit.Runes || required.Tokens > limit.Tokens {
		return &ToolEvidenceCapacityError{Required: required, Limit: limit}
	}
	return nil
}

// NormalizeToolResultEvidence is the canonical tool-result boundary. It
// redacts every result field before deriving diagnostics, paths, exit status,
// or excerpts, then returns a policy-bound representation for rendering.
func NormalizeToolResultEvidence(result ToolResultEvidence) ToolResultEvidence {
	p := result.OutputPolicy.normalized()
	result.OutputPolicy = p
	result.Tool = RedactSecrets(result.Tool)
	result.ToolCallID = RedactSecrets(result.ToolCallID)
	result.Command = RedactSecrets(result.Command)
	result.WorkingDir = RedactSecrets(result.WorkingDir)
	result.StderrSummary = RedactSecrets(result.StderrSummary)
	result.Stdout = RedactSecrets(result.Stdout)
	result.StdoutHead = RedactSecrets(result.StdoutHead)
	result.StdoutTail = RedactSecrets(result.StdoutTail)
	result.Output = RedactSecrets(result.Output)
	result.Verification = RedactSecrets(result.Verification)
	if result.Verification == "" {
		result.Verification = "passed"
	}
	result.MatchedErrors = redactEvidenceLines(result.MatchedErrors)
	result.ArtifactPaths = redactEvidenceLines(result.ArtifactPaths)
	result.ModifiedFiles = redactEvidenceLines(result.ModifiedFiles)

	stdout := result.Stdout
	if stdout == "" {
		stdout = result.Output
	}
	if stdout != "" {
		// The full output is already redacted above. All derived evidence below
		// therefore observes only safe content.
		if result.StdoutHead == "" || result.StdoutTail == "" {
			result.StdoutHead, result.StdoutTail = toolOutputHeadTail(stdout, 1_000)
		}
		if len(result.MatchedErrors) == 0 {
			result.MatchedErrors = diagnosticEvidenceLines(stdout, p)
		}
		if len(result.ArtifactPaths) == 0 {
			result.ArtifactPaths = toolEvidencePaths(stdout, p)
		}
		if len(result.ModifiedFiles) == 0 {
			result.ModifiedFiles = append([]string(nil), result.ArtifactPaths...)
		}
		if result.ExitCode == nil {
			if match := toolExitStatusPattern.FindStringSubmatch(stdout); len(match) == 2 {
				var code int
				if _, err := fmt.Sscanf(match[1], "%d", &code); err == nil {
					result.ExitCode = &code
				}
			}
		}
	}
	if result.ExitCode != nil && *result.ExitCode != 0 {
		result.Verification = "failed"
	}
	return result
}

func redactEvidenceLines(lines []string) []string {
	if len(lines) == 0 {
		return nil
	}
	out := make([]string, len(lines))
	for i, line := range lines {
		out[i] = RedactSecrets(line)
	}
	return out
}

func diagnosticEvidenceLines(output string, policy ToolOutputPolicy) []string {
	type diagnostic struct {
		line  string
		index int
		score int
	}
	var diagnostics []diagnostic
	for index, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		score := 0
		for _, marker := range []string{"error", "fail", "panic", "exit status", "exit code"} {
			if strings.Contains(lower, marker) {
				score = 2
				break
			}
		}
		if score == 0 && strings.Contains(lower, "warning") {
			score = 1
		}
		if score > 0 {
			diagnostics = append(diagnostics, diagnostic{line: line, index: index, score: score})
		}
	}
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].score != diagnostics[j].score {
			return diagnostics[i].score > diagnostics[j].score
		}
		return diagnostics[i].index < diagnostics[j].index
	})
	lines := make([]string, 0, min(len(diagnostics), policy.DiagnosticLines))
	seen := make(map[string]struct{})
	for _, diagnostic := range diagnostics {
		if len(lines) >= policy.DiagnosticLines {
			break
		}
		if _, ok := seen[diagnostic.line]; ok {
			continue
		}
		lines = append(lines, diagnostic.line)
		seen[diagnostic.line] = struct{}{}
	}
	return lines
}

func toolEvidencePaths(output string, policy ToolOutputPolicy) []string {
	matches := toolEvidencePathPattern.FindAllString(output, -1)
	if len(matches) > policy.DiagnosticLines {
		matches = matches[:policy.DiagnosticLines]
	}
	return matches
}

func toolOutputHeadTail(output string, capRunes int) (string, string) {
	runes := []rune(output)
	if capRunes <= 0 || len(runes) <= capRunes {
		return output, output
	}
	head := capRunes / 2
	return string(runes[:head]), string(runes[len(runes)-(capRunes-head):])
}

// CompactToolOutput extracts diagnostics from the full output before adding a
// bounded excerpt. It is used before verified history compaction so an error
// in the middle of a large result remains canonical evidence.
func CompactToolOutput(output string, capChars int) string {
	if capChars <= 0 {
		return output
	}
	policy := DefaultToolOutputPolicy()
	policy.MaxRunes = capChars
	policy.MaxBytes = capChars * 4
	policy.MaxTokens = max(1, (capChars+3)/4)
	policy.DiagnosticTokens = max(1, policy.MaxTokens/2)
	return CompactToolOutputWithPolicy(output, policy)
}

// CompactToolOutputWithPolicy performs redaction before classification, then
// retains prioritized diagnostic lines and a deterministic head/tail excerpt.
// The final framing is bounded byte-, rune-, and token-safely.
func CompactToolOutputWithPolicy(output string, policy ToolOutputPolicy) string {
	p := policy.normalized()
	redacted := RedactSecrets(output)
	if withinToolOutputCaps(redacted, p) {
		return redacted
	}

	type diagnostic struct {
		line  string
		index int
		score int
	}
	var diagnostics []diagnostic
	for index, line := range strings.Split(redacted, "\n") {
		lower := strings.ToLower(line)
		score := 0
		for _, marker := range []string{"error", "fail", "panic", "exit status", "exit code"} {
			if strings.Contains(lower, marker) {
				score = 2
				break
			}
		}
		if score == 0 && strings.Contains(lower, "warning") {
			score = 1
		}
		if score > 0 {
			diagnostics = append(diagnostics, diagnostic{line: line, index: index, score: score})
		}
	}
	sort.SliceStable(diagnostics, func(i, j int) bool {
		if diagnostics[i].score != diagnostics[j].score {
			return diagnostics[i].score > diagnostics[j].score
		}
		return diagnostics[i].index < diagnostics[j].index
	})
	var selected []string
	seen := make(map[string]struct{})
	for _, item := range diagnostics {
		if len(selected) >= p.DiagnosticLines {
			break
		}
		if _, ok := seen[item.line]; ok {
			continue
		}
		line := item.line
		candidate := strings.Join(append(selected, line), "\n")
		if estimateOutputTokens(candidate) > p.DiagnosticTokens {
			remainingTokens := p.DiagnosticTokens - estimateOutputTokens(strings.Join(selected, "\n"))
			if remainingTokens <= 0 {
				continue
			}
			line = limitToolOutput(line, remainingTokens*4, remainingTokens*4, remainingTokens)
			candidate = strings.Join(append(selected, line), "\n")
			if line == "" || estimateOutputTokens(candidate) > p.DiagnosticTokens {
				continue
			}
		}
		selected = append(selected, line)
		seen[item.line] = struct{}{}
	}

	const diagnosticFrame = "[preserved diagnostics]\n"
	const excerptFrame = "\n[output excerpt]\n"
	diagnosticText := strings.Join(selected, "\n")
	framedDiagnostics := diagnosticFrame + diagnosticText
	if diagnosticText == "" {
		framedDiagnostics = ""
	}
	available := p.MaxRunes - len([]rune(framedDiagnostics)) - len([]rune(excerptFrame))
	if available < 0 {
		available = 0
	}
	excerpt := limitToolOutput(redacted, available, p.MaxBytes-len([]byte(framedDiagnostics))-len([]byte(excerptFrame)), p.MaxTokens-estimateOutputTokens(framedDiagnostics)-estimateOutputTokens(excerptFrame))
	result := framedDiagnostics + excerptFrame + excerpt
	if framedDiagnostics == "" {
		result = excerpt
	}
	return limitToolOutput(result, p.MaxRunes, p.MaxBytes, p.MaxTokens)
}

func estimateOutputTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len([]rune(s)) + 3) / 4
}

func withinToolOutputCaps(s string, p ToolOutputPolicy) bool {
	return len(s) <= p.MaxBytes && len([]rune(s)) <= p.MaxRunes && estimateOutputTokens(s) <= p.MaxTokens
}

func limitToolOutput(s string, maxRunes, maxBytes, maxTokens int) string {
	if s == "" {
		return s
	}
	if maxRunes <= 0 || maxBytes <= 0 || maxTokens <= 0 {
		return ""
	}
	limit := maxRunes
	if tokenRunes := maxTokens * 4; tokenRunes < limit {
		limit = tokenRunes
	}
	if len([]rune(s)) <= limit && len(s) <= maxBytes && estimateOutputTokens(s) <= maxTokens {
		return s
	}
	for limit > 0 {
		candidate := boundedHeadTail(s, limit)
		if len(candidate) <= maxBytes && estimateOutputTokens(candidate) <= maxTokens {
			return candidate
		}
		limit--
	}
	return ""
}

func boundedHeadTail(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 0 {
		return ""
	}
	marker := "\n…[truncated]…\n"
	if len([]rune(marker)) >= maxRunes {
		return string(runes[:maxRunes])
	}
	remaining := maxRunes - len([]rune(marker))
	head := (remaining + 1) / 2
	tail := remaining - head
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}

func (DeterministicCompactor) Compact(_ context.Context, req CompactionRequest) (CompactionResult, error) {
	items := activeItems(dedupeItems(req.Items))
	result := CompactionResult{Strategy: "deterministic", SourceTokens: itemsTokens(items)}
	for _, item := range items {
		result.SourceItemIDs = append(result.SourceItemIDs, item.ID)
	}
	if req.TargetTokens <= 0 {
		return result, fmt.Errorf("target tokens must be positive")
	}

	// Required evidence is never summarized away.  Tool calls and their results
	// form an indivisible pair, as do must-keep/critical items.
	required := requiredItemIDs(items, req.Edges)
	selected := make(map[string]bool, len(required))
	known := make(map[string]bool, len(items))
	for _, item := range items {
		known[item.ID] = true
	}
	for _, id := range required {
		if known[id] {
			selected[id] = true
		}
	}
	ordered := append([]ContextItem(nil), items...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Priority != ordered[j].Priority {
			return ordered[i].Priority > ordered[j].Priority
		}
		if !ordered[i].UpdatedAt.Equal(ordered[j].UpdatedAt) {
			return ordered[i].UpdatedAt.After(ordered[j].UpdatedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})
	for _, item := range ordered {
		if selected[item.ID] {
			continue
		}
		candidate := selectedItems(items, selected, item)
		if estimateItems(candidate) <= req.TargetTokens {
			selected[item.ID] = true
		}
	}

	chosen := selectedItems(items, selected)
	if estimateItems(chosen) > req.TargetTokens {
		result.UsedFallback = true
		result.Strategy = "fallback-required-evidence"
		// Required items are rendered losslessly; silently clipping one would
		// violate the contract even if the resulting prompt happened to fit.
		result.Content = renderItems(chosen)
		result.OutputTokens = estimateText(result.Content)
		result.PreservedItemIDs = itemIDs(chosen)
		result.MissingItemIDs = missingIDs(required, selected)
		result.ValidationScore = validationScore(result)
		return result, ErrCompactionBudget
	}
	result.Content = renderItems(chosen)
	result.OutputTokens = estimateText(result.Content)
	result.PreservedItemIDs = itemIDs(chosen)
	for _, item := range items {
		if !selected[item.ID] {
			result.OmittedItemIDs = append(result.OmittedItemIDs, item.ID)
		}
	}
	result.MissingItemIDs = missingIDs(required, selected)
	result.ValidationScore = validationScore(result)
	if result.SourceTokens > 0 {
		result.CompressionRatio = float64(result.OutputTokens) / float64(result.SourceTokens)
	}
	if err := ValidateCompaction(req, result); err != nil {
		return result, err
	}
	return result, nil
}

// ValidateCompaction verifies the invariants that are safe to check without a
// model: required items, anchors, tool pairing, verification polarity, and
// the token ceiling.  It accepts LLM output only when those checks succeed.
func ValidateCompaction(req CompactionRequest, result CompactionResult) error {
	if result.OutputTokens > req.TargetTokens {
		return ErrCompactionBudget
	}
	if len(result.MissingItemIDs) > 0 {
		return fmt.Errorf("missing must-preserve items: %s", strings.Join(result.MissingItemIDs, ", "))
	}
	present := make(map[string]bool, len(result.PreservedItemIDs))
	for _, id := range result.PreservedItemIDs {
		present[id] = true
	}
	for _, id := range requiredItemIDs(req.Items, req.Edges) {
		if !present[id] {
			return fmt.Errorf("required item %q was not preserved", id)
		}
	}
	for _, item := range req.Items {
		if !present[item.ID] {
			continue
		}
		for _, anchor := range anchors(item.Content) {
			if !strings.Contains(result.Content, anchor) {
				return fmt.Errorf("anchor %q from %s changed or missing", anchor, item.ID)
			}
		}
		if item.Kind == ContextVerification && verificationFailed(item.Content) && !verificationFailed(result.Content) {
			return fmt.Errorf("failed verification %s was summarized as successful", item.ID)
		}
		if item.Kind == ContextOpenQuestion && questionDescribedAsResolved(result.Content, item.ID) {
			return fmt.Errorf("unresolved question %s was described as resolved", item.ID)
		}
		if item.SupersededBy != "" {
			return fmt.Errorf("superseded item %s was included as current context", item.ID)
		}
	}
	return nil
}

func requiredItemIDs(items []ContextItem, edges []ContextEdge) []string {
	required := map[string]bool{}
	for _, i := range items {
		if i.SupersededBy != "" {
			continue
		}
		if i.MustKeep || i.Pinned || i.Priority >= PriorityCritical {
			required[i.ID] = true
		}
	}
	for _, e := range edges {
		if e.Relation == "tool_result_of" {
			required[e.FromID], required[e.ToID] = true, true
		}
	}
	ids := make([]string, 0, len(required))
	for id := range required {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func activeItems(items []ContextItem) []ContextItem {
	out := make([]ContextItem, 0, len(items))
	for _, item := range items {
		if item.SupersededBy == "" {
			out = append(out, item)
		}
	}
	return out
}
func questionDescribedAsResolved(content, id string) bool {
	start := strings.Index(content, "["+id+" ")
	if start < 0 {
		return false
	}
	block := content[start:]
	if next := strings.Index(block[1:], "\n["); next >= 0 {
		block = block[:next+1]
	}
	lower := strings.ToLower(block)
	return (strings.Contains(lower, "resolved") || strings.Contains(lower, "answered") || strings.Contains(lower, "closed")) && !strings.Contains(lower, "unresolved") && !strings.Contains(lower, "not resolved")
}
func dedupeItems(items []ContextItem) []ContextItem {
	seen := map[string]bool{}
	out := make([]ContextItem, 0, len(items))
	for _, i := range items {
		if i.ID != "" && !seen[i.ID] {
			seen[i.ID] = true
			out = append(out, i)
		}
	}
	return out
}
func selectedItems(items []ContextItem, selected map[string]bool, extra ...ContextItem) []ContextItem {
	out := make([]ContextItem, 0, len(selected)+len(extra))
	for _, i := range items {
		if selected[i.ID] {
			out = append(out, i)
		}
	}
	out = append(out, extra...)
	return out
}
func itemIDs(items []ContextItem) []string {
	out := make([]string, 0, len(items))
	for _, i := range items {
		out = append(out, i.ID)
	}
	return out
}
func missingIDs(required []string, selected map[string]bool) []string {
	var out []string
	for _, id := range required {
		if !selected[id] {
			out = append(out, id)
		}
	}
	return out
}
func estimateText(s string) int { return (len([]rune(s)) + 3) / 4 }
func estimateItems(items []ContextItem) int {
	n := 0
	for _, i := range items {
		n += estimateText(i.Content) + 4
	}
	return n
}
func itemsTokens(items []ContextItem) int {
	n := 0
	for _, i := range items {
		n += estimateText(i.Content)
	}
	return n
}
func renderItems(items []ContextItem) string {
	var b strings.Builder
	for _, i := range items {
		fmt.Fprintf(&b, "[%s %s]\n%s\n\n", i.ID, i.Kind, strings.TrimSpace(i.Content))
	}
	return strings.TrimSpace(b.String())
}
func validationScore(r CompactionResult) float64 {
	total := len(r.PreservedItemIDs) + len(r.MissingItemIDs)
	if total == 0 {
		return 1
	}
	return float64(len(r.PreservedItemIDs)) / float64(total)
}
func verificationFailed(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "fail") || strings.Contains(s, "error") || strings.Contains(s, "exit status") || strings.Contains(s, "exit code")
}

var anchorPattern = regexp.MustCompile(`(?m)(?:\b[0-9a-f]{7,64}\b|\b(?:[A-Za-z]+Error|E\d{2,5})\b|\b\d{1,5}\b|(?:[A-Za-z0-9._-]+/)+[A-Za-z0-9._-]+)`)

func anchors(s string) []string { return uniqueStrings(anchorPattern.FindAllString(s, -1)) }
func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
