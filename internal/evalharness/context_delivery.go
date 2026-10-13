package evalharness

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

const maxCapturedProviderBody = 2 * 1024 * 1024

var contextMarker = regexp.MustCompile(`<!-- hufu-context authority=[a-z_]+ id=([^\s<>]+) -->`)

// ContextDeliveryMetrics reports fixture evidence, never prompt content.
type ContextDeliveryMetrics struct {
	SchemaVersion     int                     `json:"schema_version"`
	OracleSource      string                  `json:"oracle_source"`
	ProviderCalls     int                     `json:"provider_calls"`
	Requests          []ContextRequestMetrics `json:"requests,omitempty"`
	UnavailableReason string                  `json:"unavailable_reason,omitempty"`
}

type ContextRequestMetrics struct {
	Agent                    string                        `json:"agent"`
	ModelID                  string                        `json:"model_id"`
	TaskIndex                *int                          `json:"task_index,omitzero"`
	Attempt                  int                           `json:"attempt"`
	Step                     int                           `json:"step"`
	Purpose                  string                        `json:"purpose"`
	ModelExecutionID         string                        `json:"model_execution_id,omitempty"`
	DeliveredIDs             []string                      `json:"delivered_ids,omitempty"`
	OmittedIDs               []string                      `json:"omitted_ids,omitempty"`
	OmittedReasons           map[string]string             `json:"omitted_reasons,omitempty"`
	RequiredDeliveryRecall   *float64                      `json:"required_delivery_recall"`
	DeliveryRecall           *float64                      `json:"delivery_recall"`
	DeliveryPrecision        *float64                      `json:"delivery_precision"`
	RetrievalRecallAtK       *float64                      `json:"retrieval_recall_at_k"`
	RetrievalPrecisionAtK    *float64                      `json:"retrieval_precision_at_k"`
	RetrievedIDs             []string                      `json:"retrieved_ids,omitempty"`
	RetrievalCalls           *ContextQueryCalls            `json:"retrieval_calls"`
	EmbeddingCalls           *int                          `json:"embedding_calls"`
	RetrievalLatencyMS       *float64                      `json:"retrieval_latency_ms"`
	SelectionLatencyMS       *float64                      `json:"selection_latency_ms"`
	StageLatencyMS           map[string]float64            `json:"stage_latency_ms,omitempty"`
	MetricReasons            map[string]string             `json:"metric_reasons,omitempty"`
	ForbiddenDeliveryCount   int                           `json:"forbidden_delivery_count"`
	IrrelevantOptionalTokens int                           `json:"irrelevant_optional_tokens"`
	TokenEstimator           string                        `json:"token_estimator"`
	ManifestMatched          bool                          `json:"manifest_matched"`
	QueryAttribution         *team.ContextQueryAttribution `json:"query_attribution,omitempty"`
	ManifestSchema           int                           `json:"manifest_schema,omitzero"`
}

type capturedContextRequest struct {
	metadata team.InvocationMetadata
	modelID  string
	step     int
	hash     string
}

type receivedContextRequest struct {
	hash        string
	fragments   map[string]string
	visibleText string
}

type contextDeliveryCapture struct {
	mu              sync.Mutex
	observed        []capturedContextRequest
	received        []receivedContextRequest
	steps           map[string]int
	invocationSteps map[string]int
	findings        []EvalFinding
	queries         map[string]contextstore.QueryWork
	selections      map[string][]string
	stages          map[string]map[string]time.Duration
}

type ContextQueryCalls struct {
	Exact         int `json:"exact"`
	Lexical       int `json:"lexical"`
	Vector        int `json:"vector"`
	CandidateRows int `json:"candidate_rows"`
}

func newContextDeliveryCapture() *contextDeliveryCapture {
	return &contextDeliveryCapture{steps: make(map[string]int), invocationSteps: make(map[string]int), queries: make(map[string]contextstore.QueryWork), selections: make(map[string][]string), stages: make(map[string]map[string]time.Duration)}
}

func contextSelectionKey(requestID, executionID string) string {
	return requestID + "\x00" + executionID
}

func (c *contextDeliveryCapture) observeQuery(ctx context.Context, work contextstore.QueryWork) {
	identity, ok := team.ContextQueryIdentitySnapshot(ctx)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := contextSelectionKey(identity.ParentRequestID, identity.ModelExecutionID)
	current := c.queries[key]
	current.ExactCalls += work.ExactCalls
	current.LexicalCalls += work.LexicalCalls
	current.VectorCalls += work.VectorCalls
	current.CandidateRows += work.CandidateRows
	current.Elapsed += work.Elapsed
	c.queries[key] = current
}

func (c *contextDeliveryCapture) observeSelection(selection team.ContextSelectionObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := contextSelectionKey(selection.RequestID, selection.ModelExecutionID)
	if selection.Source == "routing" || selection.Source == "private_retrieval" || selection.Source == "compilation" || selection.Source == "manifest" {
		if c.stages[key] == nil {
			c.stages[key] = make(map[string]time.Duration)
		}
		c.stages[key][selection.Source] += selection.Elapsed
		return
	}
	if selection.Source != "primary_candidates" && selection.Source != "query_candidates" {
		return
	}
	if selection.Source == "query_candidates" {
		ids := slices.Clone(selection.SelectedIDs)
		slices.Sort(ids)
		c.selections[key] = slices.Compact(ids)
		return
	}
	ids := append(slices.Clone(c.selections[key]), selection.SelectedIDs...)
	slices.Sort(ids)
	c.selections[key] = slices.Compact(ids)
}

func (c *contextDeliveryCapture) observe(ctx context.Context, observation agent.ProviderTransportObservation) {
	metadata, ok := team.InvocationMetadataSnapshot(ctx)
	if !ok && observation.UnavailableReason == "" {
		c.recordFailure("runtime_identity_unavailable")
		return
	}
	c.recordObserved(metadata, observation)
}

func (c *contextDeliveryCapture) recordObserved(metadata team.InvocationMetadata, observation agent.ProviderTransportObservation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if observation.UnavailableReason != "" {
		c.findings = append(c.findings, EvalFinding{Dimension: "context-delivery.transport", Expected: "bounded request body", Actual: observation.UnavailableReason})
		return
	}
	if observation.InvocationID == "" {
		c.findings = append(c.findings, EvalFinding{Dimension: "context-delivery.identity", Expected: "runtime invocation identity", Actual: "unavailable"})
		return
	}
	hash, _, err := decodeContextRequest(observation.RequestBody)
	if err != nil {
		c.findings = append(c.findings, EvalFinding{Dimension: "context-delivery.transport", Expected: "valid messages", Actual: "request_decode_failed"})
		return
	}
	key := strings.Join([]string{metadata.RunID, metadata.TaskID, fmt.Sprint(metadata.Attempt), metadata.ModelExecutionID, metadata.Purpose, observation.ModelID}, "\x00")
	step, seen := c.invocationSteps[observation.InvocationID]
	if !seen {
		c.steps[key]++
		step = c.steps[key]
		c.invocationSteps[observation.InvocationID] = step
	}
	c.observed = append(c.observed, capturedContextRequest{metadata: metadata, modelID: observation.ModelID, step: step, hash: hash})
}

func (c *contextDeliveryCapture) handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			next.ServeHTTP(w, request)
			return
		}
		originalBody := request.Body
		defer func() { _ = originalBody.Close() }()
		data, err := io.ReadAll(io.LimitReader(request.Body, maxCapturedProviderBody+1))
		if err != nil || len(data) > maxCapturedProviderBody {
			http.Error(w, "eval request capture limit", http.StatusRequestEntityTooLarge)
			c.recordFailure("request_body_unavailable")
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(data))
		hash, fragments, err := decodeContextRequest(data)
		if err != nil {
			c.recordFailure("request_decode_failed")
		} else {
			c.mu.Lock()
			c.received = append(c.received, receivedContextRequest{hash: hash, fragments: fragments, visibleText: providerVisibleText(data)})
			c.mu.Unlock()
		}
		next.ServeHTTP(w, request)
	})
}

func (c *contextDeliveryCapture) recordFailure(reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.findings = append(c.findings, EvalFinding{Dimension: "context-delivery.receiver", Expected: "bounded valid messages", Actual: reason})
}

// decodeContextRequest hashes the complete payload for transport pairing,
// but counts context only in system/user/developer text. Tool schemas, tool-call
// arguments, and assistant responses cannot satisfy an inclusion assertion.
func decodeContextRequest(data []byte) (string, map[string]string, error) {
	var request struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(data, &request); err != nil || request.Messages == nil {
		return "", nil, fmt.Errorf("invalid provider messages")
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", nil, err
	}
	canonical, err := json.Marshal(payload, json.Deterministic(true))
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	fragments := make(map[string]string)
	for _, message := range request.Messages {
		role, _ := message["role"].(string)
		if role != "system" && role != "user" && role != "developer" {
			continue
		}
		for _, text := range modelMessageTexts(message["content"]) {
			for id, fragment := range contextFragments(text) {
				fragments[id] += fragment + "\n"
			}
		}
	}
	return hex.EncodeToString(sum[:]), fragments, nil
}

func providerVisibleText(data []byte) string {
	var request struct {
		Messages []map[string]any `json:"messages"`
	}
	if json.Unmarshal(data, &request) != nil {
		return ""
	}
	var text strings.Builder
	for _, message := range request.Messages {
		role, _ := message["role"].(string)
		if role != "system" && role != "user" && role != "developer" {
			continue
		}
		for _, part := range modelMessageTexts(message["content"]) {
			text.WriteString(part)
			text.WriteByte('\n')
		}
	}
	return text.String()
}

func forbiddenContextDelivered(request receivedContextRequest, id string, fixtures []ContextItemFixture) bool {
	if _, delivered := request.fragments[id]; delivered {
		return true
	}
	for _, fixture := range fixtures {
		if fixture.ID == id && strings.Contains(request.visibleText, contextstore.NormalizeContent(fixture.Content)) {
			return true
		}
	}
	return false
}

func modelMessageTexts(content any) []string {
	if text, ok := content.(string); ok {
		return []string{text}
	}
	var texts []string
	if parts, ok := content.([]any); ok {
		for _, part := range parts {
			object, _ := part.(map[string]any)
			kind, _ := object["type"].(string)
			if kind == "text" || kind == "input_text" {
				if text, ok := object["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
	}
	return texts
}

func contextFragments(text string) map[string]string {
	fragments := make(map[string]string)
	matches := contextMarker.FindAllStringSubmatchIndex(text, -1)
	for index, match := range matches {
		end := len(text)
		if index+1 < len(matches) {
			end = matches[index+1][0]
		}
		id := strings.TrimPrefix(text[match[2]:match[3]], "context:")
		fragments[id] += strings.TrimSpace(text[match[1]:end]) + "\n"
	}
	return fragments
}

func (c *contextDeliveryCapture) evaluate(expect ExpectSpec, fixtures []ContextItemFixture, tasks []*team.TodoItem, coordinatorManifests []team.ContextInjectionManifest) ([]EvalFinding, *ContextDeliveryMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()
	findings := slices.Clone(c.findings)
	metrics := &ContextDeliveryMetrics{SchemaVersion: 1, OracleSource: "synthetic_contract", ProviderCalls: len(c.received)}
	for _, request := range c.received {
		for _, id := range expect.ForbiddenContext {
			if forbiddenContextDelivered(request, id, fixtures) {
				findings = append(findings, EvalFinding{Dimension: "forbidden-context", Expected: id + " absent in every request", Actual: "delivered"})
			}
		}
	}
	if expect.ProviderCallCount != nil && len(c.received) != *expect.ProviderCallCount {
		findings = append(findings, EvalFinding{Dimension: "provider-call-count", Expected: fmt.Sprint(*expect.ProviderCallCount), Actual: fmt.Sprint(len(c.received))})
	}
	manifests := append(team.ContextManifestsFromTodos(tasks), coordinatorManifests...)
	used := make([]bool, len(c.received))
	for _, observed := range c.observed {
		at := slices.IndexFunc(c.received, func(received receivedContextRequest) bool { return received.hash == observed.hash })
		for at >= 0 && used[at] {
			next := slices.IndexFunc(c.received[at+1:], func(received receivedContextRequest) bool { return received.hash == observed.hash })
			if next < 0 {
				at = -1
			} else {
				at += next + 1
			}
		}
		if at < 0 {
			findings = append(findings, EvalFinding{Dimension: "context-delivery.transport-pairing", Expected: "received request matching observed messages", Actual: "not_received"})
			continue
		}
		used[at] = true
		for index, want := range expect.ContextDelivery {
			if !matchesContextInvocation(want, observed, tasks) {
				continue
			}
			requestFindings, requestMetrics := evaluateContextRequest(want, observed, c.received[at], fixtures, tasks, manifests)
			c.addRetrievalMetrics(&requestMetrics, want, observed)
			for _, finding := range requestFindings {
				finding.Dimension = fmt.Sprintf("context-delivery[%d].%s", index, finding.Dimension)
				findings = append(findings, finding)
			}
			metrics.Requests = append(metrics.Requests, requestMetrics)
		}
	}
	for index, received := range used {
		if !received {
			findings = append(findings, EvalFinding{Dimension: "context-delivery.transport-pairing", Expected: "runtime identity for every received request", Actual: fmt.Sprintf("unpaired request %d", index)})
		}
	}
	for index, want := range expect.ContextDelivery {
		if !slices.ContainsFunc(c.observed, func(request capturedContextRequest) bool { return matchesContextInvocation(want, request, tasks) }) {
			findings = append(findings, EvalFinding{Dimension: fmt.Sprintf("context-delivery[%d]", index), Expected: "matching runtime invocation", Actual: "not_called"})
		}
	}
	slices.SortFunc(metrics.Requests, func(a, b ContextRequestMetrics) int {
		return cmp.Or(strings.Compare(a.Agent, b.Agent), cmp.Compare(contextTaskIndex(a.TaskIndex), contextTaskIndex(b.TaskIndex)), cmp.Compare(a.Attempt, b.Attempt), strings.Compare(a.ModelExecutionID, b.ModelExecutionID), strings.Compare(a.ModelID, b.ModelID), cmp.Compare(a.Step, b.Step), strings.Compare(a.Purpose, b.Purpose))
	})
	return findings, metrics
}

func (c *contextDeliveryCapture) addRetrievalMetrics(metrics *ContextRequestMetrics, want ContextDeliveryExpect, observed capturedContextRequest) {
	key := contextSelectionKey(observed.metadata.ParentRequestID, observed.metadata.ModelExecutionID)
	if ids, ok := c.selections[key]; ok {
		metrics.RetrievedIDs = slices.Clone(ids)
		relevant := 0
		for _, id := range ids {
			if slices.Contains(want.Relevant, id) {
				relevant++
			}
		}
		metrics.RetrievalRecallAtK = contextRatio(relevant, len(want.Relevant))
		metrics.RetrievalPrecisionAtK = contextRatio(relevant, len(ids))
		if metrics.RetrievalRecallAtK == nil {
			metrics.MetricReasons["retrieval_recall_at_k"] = "not_applicable"
		}
		if metrics.RetrievalPrecisionAtK == nil {
			metrics.MetricReasons["retrieval_precision_at_k"] = "not_applicable"
		}
	} else {
		metrics.MetricReasons["retrieval_recall_at_k"] = "unavailable"
		metrics.MetricReasons["retrieval_precision_at_k"] = "unavailable"
	}
	if work, ok := c.queries[key]; ok {
		metrics.RetrievalCalls = &ContextQueryCalls{Exact: work.ExactCalls, Lexical: work.LexicalCalls, Vector: work.VectorCalls, CandidateRows: work.CandidateRows}
		metrics.RetrievalLatencyMS = new(float64(work.Elapsed.Microseconds()) / 1000)
		delete(metrics.MetricReasons, "retrieval_calls")
		delete(metrics.MetricReasons, "retrieval_latency_ms")
	} else {
		metrics.MetricReasons["retrieval_calls"] = "unavailable"
		metrics.MetricReasons["retrieval_latency_ms"] = "unavailable"
	}
	metrics.MetricReasons["embedding_calls"] = "backend_not_observed"
	if stages, ok := c.stages[key]; ok {
		metrics.StageLatencyMS = make(map[string]float64)
		var total time.Duration
		complete := true
		for _, stage := range []string{"routing", "compilation", "manifest"} {
			elapsed, seen := stages[stage]
			complete = complete && seen
			if seen {
				metrics.StageLatencyMS[stage] = float64(elapsed.Nanoseconds()) / 1e6
				total += elapsed
			}
		}
		if elapsed, seen := stages["private_retrieval"]; seen {
			metrics.StageLatencyMS["private_retrieval"] = float64(elapsed.Nanoseconds()) / 1e6
			total += elapsed
		}
		if complete {
			metrics.SelectionLatencyMS = new(float64(total.Nanoseconds()) / 1e6)
			delete(metrics.MetricReasons, "selection_latency_ms")
		} else {
			metrics.MetricReasons["selection_latency_ms"] = "stage_not_observed"
		}
	} else {
		metrics.MetricReasons["selection_latency_ms"] = "stage_not_observed"
	}
}

func matchesContextInvocation(want ContextDeliveryExpect, request capturedContextRequest, tasks []*team.TodoItem) bool {
	if want.Agent != request.metadata.AgentName || (want.Attempt > 0 && want.Attempt != request.metadata.Attempt) || (want.Purpose != "" && want.Purpose != request.metadata.Purpose) {
		return false
	}
	if want.TaskIndex != nil && (*want.TaskIndex < 0 || *want.TaskIndex >= len(tasks) || tasks[*want.TaskIndex] == nil || tasks[*want.TaskIndex].ID != request.metadata.TaskID) {
		return false
	}
	return want.Step == 0 || want.Step == request.step
}

func evaluateContextRequest(want ContextDeliveryExpect, observed capturedContextRequest, received receivedContextRequest, fixtures []ContextItemFixture, tasks []*team.TodoItem, manifests []team.ContextInjectionManifest) ([]EvalFinding, ContextRequestMetrics) {
	var findings []EvalFinding
	metrics := ContextRequestMetrics{Agent: observed.metadata.AgentName, ModelID: observed.modelID, Attempt: observed.metadata.Attempt, Step: observed.step, Purpose: observed.metadata.Purpose, ModelExecutionID: observed.metadata.ModelExecutionID, TokenEstimator: "rune_estimate", OmittedReasons: make(map[string]string), MetricReasons: make(map[string]string)}
	if index := slices.IndexFunc(tasks, func(item *team.TodoItem) bool { return item != nil && item.ID == observed.metadata.TaskID }); index >= 0 {
		metrics.TaskIndex = new(index)
	}
	metrics.DeliveredIDs = slices.Sorted(maps.Keys(received.fragments))
	var manifest *team.ContextInjectionManifest
	for _, candidate := range manifests {
		if candidate.RequestID == observed.metadata.ParentRequestID && candidate.ModelExecutionID == observed.metadata.ModelExecutionID {
			manifest = new(candidate)
			break
		}
	}
	if manifest == nil {
		findings = append(findings, EvalFinding{Dimension: "manifest", Expected: "matching durable manifest", Actual: "missing"})
	} else {
		metrics.ManifestMatched = true
		// Read attribution from the exact durable manifest of the received
		// request rather than replaying selection against current memory.
		if manifest.Query != nil {
			query := *manifest.Query
			query.Sources = slices.Clone(query.Sources)
			metrics.QueryAttribution = &query
			metrics.ManifestSchema = manifest.SchemaVersion
			if err := team.ValidateContextQueryManifest(*manifest); err != nil {
				findings = append(findings, EvalFinding{Dimension: "manifest", Expected: "valid query fingerprint", Actual: "invalid_query_attribution"})
			}
		}
		for _, item := range manifest.Items {
			if !item.Included {
				metrics.OmittedIDs = append(metrics.OmittedIDs, item.ID)
				metrics.OmittedReasons[item.ID] = string(item.Reason)
			}
		}
		slices.Sort(metrics.OmittedIDs)
	}
	for _, id := range want.Include {
		if strings.TrimSpace(received.fragments[id]) == "" {
			findings = append(findings, EvalFinding{Dimension: "include", Expected: id, Actual: "not_delivered"})
		}
		if manifest != nil && !slices.ContainsFunc(manifest.Items, func(item team.ContextManifestItem) bool { return item.ID == id && item.Included }) {
			findings = append(findings, EvalFinding{Dimension: "attribution", Expected: id + " included in durable manifest", Actual: "missing"})
		}
	}
	for _, id := range want.Exclude {
		if forbiddenContextDelivered(received, id, fixtures) {
			metrics.ForbiddenDeliveryCount++
			findings = append(findings, EvalFinding{Dimension: "exclude", Expected: id + " absent", Actual: "delivered"})
		}
	}
	required := 0
	for _, id := range slices.Sorted(maps.Keys(want.RequiredFragments)) {
		attributed := manifest != nil && slices.ContainsFunc(manifest.Items, func(item team.ContextManifestItem) bool {
			return item.ID == id && item.Included && item.Reason == team.ContextIncludedRequired
		})
		if !attributed {
			findings = append(findings, EvalFinding{Dimension: "required-attribution", Expected: id + " required in durable manifest", Actual: "missing"})
		}
		if strings.Contains(received.fragments[id], want.RequiredFragments[id]) && attributed {
			required++
		} else {
			findings = append(findings, EvalFinding{Dimension: "required-fragment", Expected: id, Actual: "sentinel_missing"})
		}
	}
	metrics.RequiredDeliveryRecall = contextRatio(required, len(want.RequiredFragments))
	relevant, optional := 0, 0
	for _, fixture := range fixtures {
		fragment, delivered := received.fragments[fixture.ID]
		if !delivered {
			continue
		}
		if !strings.Contains(fragment, contextstore.NormalizeContent(fixture.Content)) {
			findings = append(findings, EvalFinding{Dimension: "content", Expected: fixture.ID + " content", Actual: "sentinel_missing"})
		}
	}
	if manifest != nil {
		for _, item := range manifest.Items {
			if !item.Included || item.Reason == team.ContextIncludedRequired || (item.Source != "shared_persistent" && item.Source != "shared_session" && !strings.HasPrefix(item.Source, "worker_")) {
				continue
			}
			fragment, delivered := received.fragments[item.ID]
			if !delivered {
				continue
			}
			optional++
			if slices.Contains(want.Relevant, item.ID) {
				relevant++
			} else {
				metrics.IrrelevantOptionalTokens += max(1, (len([]rune(strings.TrimSpace(fragment)))+3)/4)
			}
		}
	}
	metrics.DeliveryRecall = contextRatio(relevant, len(want.Relevant))
	metrics.DeliveryPrecision = contextRatio(relevant, optional)
	for name, value := range map[string]*float64{"required_delivery_recall": metrics.RequiredDeliveryRecall, "delivery_recall": metrics.DeliveryRecall, "delivery_precision": metrics.DeliveryPrecision} {
		if value == nil {
			metrics.MetricReasons[name] = "not_applicable"
		}
	}
	return findings, metrics
}

func contextRatio(numerator, denominator int) *float64 {
	if denominator == 0 {
		return nil
	}
	return new(float64(numerator) / float64(denominator))
}

func contextTaskIndex(index *int) int {
	if index == nil {
		return -1
	}
	return *index
}

func validateContextDeliveryExpect(expect ExpectSpec) error {
	if expect.ProviderCallCount != nil && *expect.ProviderCallCount < 0 {
		return fmt.Errorf("provider-call-count must be non-negative")
	}
	for _, id := range expect.ForbiddenContext {
		if id == "" || strings.ContainsAny(id, " \t\n\r<>") {
			return fmt.Errorf("forbidden-context requires canonical item IDs")
		}
	}
	for index, request := range expect.ContextDelivery {
		if strings.TrimSpace(request.Agent) == "" || request.Attempt < 0 || request.Step < 0 || (request.TaskIndex != nil && *request.TaskIndex < 0) {
			return fmt.Errorf("context-delivery[%d] requires an agent and non-negative invocation selectors", index)
		}
		for _, values := range [][]string{request.Include, request.Exclude, request.Relevant} {
			seen := make(map[string]bool, len(values))
			for _, id := range values {
				if id == "" || strings.ContainsAny(id, " \t\n\r<>") || seen[id] {
					return fmt.Errorf("context-delivery[%d] requires unique canonical item IDs", index)
				}
				seen[id] = true
			}
		}
		for _, id := range request.Include {
			if slices.Contains(request.Exclude, id) || slices.Contains(expect.ForbiddenContext, id) {
				return fmt.Errorf("context-delivery[%d] includes and excludes the same item", index)
			}
		}
		for _, id := range request.Relevant {
			if slices.Contains(request.Exclude, id) || slices.Contains(expect.ForbiddenContext, id) {
				return fmt.Errorf("context-delivery[%d] relevant oracle includes forbidden content", index)
			}
		}
		for id, sentinel := range request.RequiredFragments {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(sentinel) == "" {
				return fmt.Errorf("context-delivery[%d] requires non-empty fragment IDs and sentinels", index)
			}
		}
	}
	return nil
}
