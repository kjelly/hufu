package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

const (
	contextArtifactIDVersion    = 1
	contextArtifactEventVersion = 1
	contextArtifactIDPrefix     = "ctxart-"
	// contextArtifactLineCap bounds each of the two header lines of a
	// preview response.
	contextArtifactLineCap = 256
)

type contextArtifactServiceKey struct{}

// contextArtifactService publishes large tool results for one executeTask
// call and authorizes reads of what it published. It lives on the attempt
// context rather than on the coordinator because extra-model leaves run on a
// cloned coordinator while the view opener stays bound to the original. A
// reference is readable from every attempt of the same call, since a retry
// carries the previous attempt's tool messages in its history.
type contextArtifactService struct {
	c        *Coordinator
	runID    string
	taskID   string
	policy   ExecutionContextArtifactPolicySnapshot
	excluded map[string]bool

	mu     sync.Mutex
	counts map[int]int
	refs   map[string]ArtifactRef
}

// newContextArtifactService returns nil unless offload is admitted for this
// run and the task can safely use it.
func (c *Coordinator) newContextArtifactService(todoID string, task TaskDef) *contextArtifactService {
	policy, ok := c.admittedContextArtifactPolicy()
	if !ok || todoID == "" || todoID == CoordTodoID || !c.hasDurableEventJournal() {
		return nil
	}
	runID := strings.TrimSpace(c.executionRunID)
	if runID == "" {
		return nil
	}
	// A closed tool sequence fails on any out-of-order call, including the
	// view call needed to read an offloaded result.
	if len(task.Execution.ToolSequence) > 0 {
		return nil
	}
	return &contextArtifactService{
		c: c, runID: runID, taskID: todoID, policy: policy,
		excluded: resultContainsAssertedTools(task.VerifySpec),
		counts:   make(map[int]int), refs: make(map[string]ArtifactRef),
	}
}

// resultContainsAssertedTools names the tools whose model-visible result a
// tool_call_assert substring check reads. Offloading them would hide the
// text the assertion matches.
func resultContainsAssertedTools(spec *VerificationSpec) map[string]bool {
	excluded := make(map[string]bool)
	if spec == nil {
		return excluded
	}
	for _, assertion := range spec.ToolCallAssertions {
		if assertion.ResultContains != "" {
			excluded[strings.TrimSpace(assertion.Tool)] = true
		}
	}
	return excluded
}

// install puts the service on an attempt context when the attempt's surface
// can read references back through view.
func (s *contextArtifactService) install(ctx context.Context, resolved ResolvedWorkerTools) context.Context {
	if s == nil || !slices.Contains(resolved.Names, "view") || !slices.Contains(resolved.AuthorizedNames, "view") {
		return ctx
	}
	ctx = context.WithValue(ctx, contextArtifactServiceKey{}, s)
	return tools.WithArtifactMaxReadBytes(ctx, s.policy.MaxReadBytes)
}

func contextArtifactServiceFromContext(ctx context.Context) *contextArtifactService {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(contextArtifactServiceKey{}).(*contextArtifactService)
	return s
}

// withoutContextArtifactService hides any service inherited by ctx.
func withoutContextArtifactService(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextArtifactServiceKey{}, (*contextArtifactService)(nil))
}

// withToolOffloader binds an offloader to one tool call when the tool is an
// eligible source. It is called by the policy gate after every check.
func (s *contextArtifactService) withToolOffloader(ctx context.Context, toolName, toolCallID string) context.Context {
	if s == nil || s.excluded[toolName] || (toolName != "bash" && toolName != dynamicToolGatewayName) {
		return ctx
	}
	attempt, _ := ctx.Value(executionAttemptKey{}).(int)
	return tools.WithToolOutputOffloader(ctx, func(callCtx context.Context, capture tools.ToolOutputCapture) (fantasy.ToolResponse, bool) {
		return s.publish(callCtx, toolName, toolCallID, attempt, capture)
	})
}

func (s *contextArtifactService) eligible(toolName string, attempt int, capture tools.ToolOutputCapture) bool {
	size := len(capture.Content)
	if capture.ToolName != toolName || !utf8.ValidString(capture.Content) ||
		size > s.policy.MaxArtifactBytes || size <= s.policy.PreviewBytes ||
		(size <= s.policy.MinBytes && !capture.LegacyWouldTruncate) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[attempt] < s.policy.MaxArtifactsPerAttempt
}

func contextArtifactID(runID, taskID string, attempt int, toolCallID, contentSHA string) string {
	hasher := sha256.New()
	writeDigestRecord(hasher, "context_artifact_id_version", strconv.Itoa(contextArtifactIDVersion))
	writeDigestRecord(hasher, "run", runID)
	writeDigestRecord(hasher, "task", taskID)
	writeDigestRecord(hasher, "attempt", strconv.Itoa(attempt))
	writeDigestRecord(hasher, "tool_call", toolCallID)
	writeDigestRecord(hasher, "content_sha256", contentSHA)
	return contextArtifactIDPrefix + hex.EncodeToString(hasher.Sum(nil))[:40]
}

// publish stores one captured result, records it durably, and returns the
// preview response. Any failure returns false so the caller delivers its
// legacy bounded response; the tool is never rerun.
func (s *contextArtifactService) publish(ctx context.Context, toolName, toolCallID string, attempt int, capture tools.ToolOutputCapture) (fantasy.ToolResponse, bool) {
	if s == nil || !s.eligible(toolName, attempt, capture) {
		return fantasy.ToolResponse{}, false
	}
	sum := sha256.Sum256([]byte(capture.Content))
	contentSHA := hex.EncodeToString(sum[:])
	id := contextArtifactID(s.runID, s.taskID, attempt, toolCallID, contentSHA)
	s.mu.Lock()
	existing, republished := s.refs[id]
	s.mu.Unlock()
	if republished {
		return contextArtifactResponse(existing, toolName, capture, s.policy), true
	}

	ref, reason := s.store(ctx, id, toolCallID, attempt, capture.Content)
	if reason != "" {
		s.reportSkipped(toolCallID, reason)
		return fantasy.ToolResponse{}, false
	}
	response := contextArtifactResponse(ref, toolName, capture, s.policy)
	if err := s.appendPublished(ctx, ref, toolName, toolCallID, attempt, len(response.Content), capture); err != nil {
		s.reportSkipped(toolCallID, "event_append_failed")
		return fantasy.ToolResponse{}, false
	}
	s.mu.Lock()
	if _, raced := s.refs[id]; !raced {
		s.refs[id] = ref
		s.counts[attempt]++
	}
	s.mu.Unlock()
	s.c.report(s.c.newEvent("step").withTodoID(s.taskID).withMessage(fmt.Sprintf(
		"context artifact %s published for %s call %s: %d bytes, %d-byte preview", ref.ID, toolName, toolCallID, ref.ByteSize, len(response.Content))))
	return response, true
}

// store writes and verifies the artifact. It returns a reason code instead
// of the error, because store errors carry absolute filesystem paths.
func (s *contextArtifactService) store(ctx context.Context, id, toolCallID string, attempt int, content string) (ArtifactRef, string) {
	root := s.c.artifactStoreRootPath()
	if scope, scoped := artifactAccessScopeFromContext(ctx); scoped && strings.TrimSpace(scope.StoreRoot) != "" {
		root = scope.StoreRoot
	}
	store, err := NewFileArtifactStore(root, s.c.projectDir)
	if err != nil {
		return ArtifactRef{}, "store_unavailable"
	}
	stored, err := store.Put(ctx, PutArtifactRequest{
		ID: id, Kind: "context", Role: "tool_output",
		Path:      "context-artifacts/" + s.taskID + "/" + toolCallID,
		MediaType: "text/plain; charset=utf-8", Content: []byte(content),
		RunID: s.runID, TaskID: s.taskID, Attempt: attempt, ToolCallID: toolCallID,
	})
	if err != nil {
		return ArtifactRef{}, "store_put_failed"
	}
	if err := store.Verify(ctx, stored.ArtifactRef); err != nil {
		return ArtifactRef{}, "store_verify_failed"
	}
	return stored.ArtifactRef, ""
}

type contextArtifactPublishedPayload struct {
	Version      int    `json:"version"`
	RunID        string `json:"run_id"`
	TaskID       string `json:"task_id"`
	Attempt      int    `json:"attempt"`
	ToolName     string `json:"tool_name"`
	ToolCallID   string `json:"tool_call_id"`
	ArtifactID   string `json:"artifact_id"`
	SHA256       string `json:"sha256"`
	Bytes        int64  `json:"bytes"`
	PreviewBytes int    `json:"preview_bytes"`
	Reason       string `json:"reason"`
}

// appendPublished records the publication without any content, path, or
// tool input. A reference is exposed only after this append succeeds.
func (s *contextArtifactService) appendPublished(ctx context.Context, ref ArtifactRef, toolName, toolCallID string, attempt, previewBytes int, capture tools.ToolOutputCapture) error {
	reason := "size"
	if len(capture.Content) <= s.policy.MinBytes {
		reason = "legacy_truncation"
	}
	raw, err := json.Marshal(contextArtifactPublishedPayload{
		Version: contextArtifactEventVersion, RunID: s.runID, TaskID: s.taskID, Attempt: attempt,
		ToolName: toolName, ToolCallID: toolCallID, ArtifactID: ref.ID, SHA256: ref.SHA256,
		Bytes: ref.ByteSize, PreviewBytes: previewBytes, Reason: reason,
	})
	if err != nil {
		return fmt.Errorf("encode context artifact event: %w", err)
	}
	journal := s.c.EventJournal()
	if journal == nil {
		return fmt.Errorf("context artifact event journal is unavailable")
	}
	if _, err := journal.Append(context.WithoutCancel(ctx), RunEvent{
		Type: string(EventContextArtifactPublished), Actor: "runtime", TaskID: s.taskID, Attempt: attempt,
		IdempotencyKey: "context-artifact:" + ref.ID, Payload: raw,
	}); err != nil {
		return fmt.Errorf("append context artifact event: %w", err)
	}
	return nil
}

func (s *contextArtifactService) reportSkipped(toolCallID, reason string) {
	s.c.report(s.c.newEvent("step").withTodoID(s.taskID).withMessage(fmt.Sprintf(
		"context artifact offload skipped for call %s (%s); the bounded legacy result was returned", toolCallID, reason)))
}

// authorizedRef resolves an ID this service published, but only for the
// same run and task that published it.
func (s *contextArtifactService) authorizedRef(c *Coordinator, ctx context.Context, id string) (ArtifactRef, bool) {
	if s == nil || c == nil || s.runID != strings.TrimSpace(c.executionRunID) {
		return ArtifactRef{}, false
	}
	if todoID, _ := ctx.Value(todoIDKey{}).(string); todoID != s.taskID {
		return ArtifactRef{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.refs[id]
	return ref, ok
}

// contextArtifactResponse renders the preview. The reference header is the
// first line so squeeze and compaction keep it, and for bash the tail keeps
// the exit-code footer that exit-status parsing reads.
func contextArtifactResponse(ref ArtifactRef, toolName string, capture tools.ToolOutputCapture, policy ExecutionContextArtifactPolicySnapshot) fantasy.ToolResponse {
	content := capture.Content
	head, tail := contextArtifactPreview(content, policy.PreviewBytes)
	total := len(content)
	tailStart := total - len(tail)
	omitted := tailStart - len(head)
	var b strings.Builder
	b.WriteString(truncateHeaderLine(fmt.Sprintf("[context artifact] artifact_ref=%s sha256=%s bytes=%d source=%s", ref.ID, ref.SHA256, total, toolName)))
	b.WriteByte('\n')
	b.WriteString(truncateHeaderLine(fmt.Sprintf(`[preview: bytes [0,%d) and [%d,%d) of %d; %d bytes omitted. Read more with view {"artifact_ref":"%s","byte_offset":%d,"byte_limit":%d}]`,
		len(head), tailStart, total, total, omitted, ref.ID, len(head), policy.MaxReadBytes)))
	b.WriteByte('\n')
	b.WriteString(head)
	if !strings.HasSuffix(head, "\n") {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "[... %d bytes omitted ...]\n", omitted)
	b.WriteString(tail)
	if capture.IsError {
		return fantasy.NewTextErrorResponse(b.String())
	}
	return fantasy.NewTextResponse(b.String())
}

// contextArtifactPreview splits the preview budget: three quarters for the
// head and the rest for the tail. The tail starts at a line boundary when the
// final line fits, so it always ends with that complete line.
func contextArtifactPreview(content string, budget int) (string, string) {
	head := content[:utf8Floor(content, budget*3/4)]
	start := max(len(head), len(content)-(budget-len(head)))
	finalLine := strings.LastIndexByte(strings.TrimSuffix(content, "\n"), '\n') + 1
	if finalLine >= start {
		// The final line fits: advance to the next line start so no line
		// is split. That newline is at or before the final line's start.
		if start > 0 && content[start-1] != '\n' {
			start += strings.IndexByte(content[start:], '\n') + 1
		}
		return head, content[start:]
	}
	// The final line alone exceeds the tail budget: keep its last bytes.
	for start < len(content) && !utf8.RuneStart(content[start]) {
		start++
	}
	return head, content[start:]
}

// utf8Floor returns the largest rune boundary at or below n.
func utf8Floor(s string, n int) int {
	if n >= len(s) {
		return len(s)
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

func truncateHeaderLine(line string) string {
	if len(line) <= contextArtifactLineCap {
		return line
	}
	return line[:utf8Floor(line, contextArtifactLineCap)]
}
