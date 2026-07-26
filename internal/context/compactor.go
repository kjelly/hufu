package context

// Verified, deterministic context compaction.  This code deliberately lives
// beside the canonical model instead of the coordinator: callers hand it
// structured ContextItems and receive a result that can be validated without
// trusting an LLM summary.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var ErrCompactionBudget = errors.New("context compaction cannot fit required evidence in token budget")

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
	Scope         Scope
}

// ToolEvidenceItems turns a call/result pair into canonical evidence and the
// directional relationship required by the compactor and repository.
func ToolEvidenceItems(call ToolCallEvidence, result ToolResultEvidence) ([]ContextItem, []ContextEdge) {
	if call.ID == "" {
		call.ID = "tool-call:" + call.Tool + ":" + call.Command
	}
	if result.ID == "" {
		result.ID = "tool-result:" + call.ID
	}
	if result.ToolCallID == "" {
		result.ToolCallID = call.ID
	}
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
	callItem := ContextItem{ID: call.ID, Kind: ContextToolCall, Content: renderToolCall(call), Scope: call.Scope, Authority: AuthorityTool, TrustLevel: TrustInternal, Priority: PriorityHigh, MustKeep: true}
	resultItem := ContextItem{ID: result.ID, Kind: ContextToolResult, Content: renderToolResult(result), Scope: result.Scope, Authority: AuthorityTool, TrustLevel: TrustInternal, Priority: PriorityHigh, MustKeep: true}
	return []ContextItem{callItem, resultItem}, []ContextEdge{{FromID: result.ID, Relation: "tool_result_of", ToID: result.ToolCallID}}
}

func renderToolCall(call ToolCallEvidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tool: %s\ncommand: %s\nworking_dir: %s", call.Tool, call.Command, call.WorkingDir)
	if !call.StartedAt.IsZero() {
		fmt.Fprintf(&b, "\nstarted_at: %s", call.StartedAt.UTC().Format(time.RFC3339Nano))
	}
	return b.String()
}

func renderToolResult(result ToolResultEvidence) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tool: %s\ntool_call_id: %s\ncommand: %s\nworking_dir: %s", result.Tool, result.ToolCallID, result.Command, result.WorkingDir)
	if result.ExitCode != nil {
		fmt.Fprintf(&b, "\nexit_code: %d", *result.ExitCode)
	}
	if !result.StartedAt.IsZero() {
		fmt.Fprintf(&b, "\nstarted_at: %s", result.StartedAt.UTC().Format(time.RFC3339Nano))
	}
	if !result.EndedAt.IsZero() {
		fmt.Fprintf(&b, "\nended_at: %s", result.EndedAt.UTC().Format(time.RFC3339Nano))
	}
	if result.StderrSummary != "" {
		fmt.Fprintf(&b, "\nstderr_summary: %s", result.StderrSummary)
	}
	stdout := result.Stdout
	if stdout == "" {
		stdout = result.Output
	}
	if result.StdoutHead != "" {
		fmt.Fprintf(&b, "\nstdout_head:\n%s", result.StdoutHead)
	}
	if result.StdoutTail != "" {
		fmt.Fprintf(&b, "\nstdout_tail:\n%s", result.StdoutTail)
	}
	if stdout != "" {
		fmt.Fprintf(&b, "\nstdout:\n%s", CompactToolOutput(stdout, 6_000))
	}
	if len(result.MatchedErrors) > 0 {
		fmt.Fprintf(&b, "\nmatched_errors:\n%s", strings.Join(result.MatchedErrors, "\n"))
	}
	if len(result.ArtifactPaths) > 0 {
		fmt.Fprintf(&b, "\nartifact_paths:\n%s", strings.Join(result.ArtifactPaths, "\n"))
	}
	if len(result.ModifiedFiles) > 0 {
		fmt.Fprintf(&b, "\nmodified_files:\n%s", strings.Join(result.ModifiedFiles, "\n"))
	}
	if result.Verification != "" {
		fmt.Fprintf(&b, "\nverification: %s", result.Verification)
	}
	return b.String()
}

// CompactToolOutput extracts diagnostics from the full output before adding a
// bounded excerpt. It is used before verified history compaction so an error
// in the middle of a large result remains canonical evidence.
func CompactToolOutput(output string, capChars int) string {
	if capChars <= 0 || len([]rune(output)) <= capChars {
		return output
	}
	var diagnostics []string
	for _, line := range strings.Split(output, "\n") {
		lower := strings.ToLower(line)
		if strings.Contains(lower, "error") || strings.Contains(lower, "fail") || strings.Contains(lower, "panic") || strings.Contains(lower, "exit status") || strings.Contains(lower, "exit code") || strings.Contains(lower, "warning") {
			diagnostics = append(diagnostics, line)
		}
	}
	diagnosticText := strings.Join(diagnostics, "\n")
	if len([]rune(diagnosticText)) > capChars/2 {
		diagnosticText = squeezeRunes(diagnosticText, capChars/2)
	}
	remaining := capChars - len([]rune(diagnosticText)) - 64
	if remaining < 100 {
		remaining = 100
	}
	excerpt := squeezeRunes(output, remaining)
	if diagnosticText == "" {
		return excerpt
	}
	return "[preserved diagnostics]\n" + diagnosticText + "\n[output excerpt]\n" + excerpt
}

func squeezeRunes(s string, capChars int) string {
	runes := []rune(s)
	if len(runes) <= capChars {
		return s
	}
	if capChars < 2 {
		return string(runes[:capChars])
	}
	marker := fmt.Sprintf("\n…[truncated %d chars]…\n", len(runes)-capChars)
	head := capChars * 2 / 3
	tail := capChars - head
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
