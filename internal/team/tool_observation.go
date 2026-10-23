package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	recallObservationToolName = "recall_observation"
	maxObservationBytes       = 1 << 20
	maxObservationsPerAttempt = 32
	maxObservationReadBytes   = 32 << 10
)

// ToolObservationPayload is an immutable publication, not task completion or
// a new observation when it is subsequently read by another workflow cycle.
type ToolObservationPayload struct {
	TeamID           string      `json:"team_id"`
	Tool             string      `json:"tool"`
	Artifact         ArtifactRef `json:"artifact"`
	ModelExecutionID string      `json:"model_execution_id,omitempty"`
}

type storedToolObservation struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

// ObservationReadPayload identifies the original producer and the exact range
// delivered to this attempt. Offsets address extracted evidence text when a
// result schema supplies an extraction rule, otherwise the original output.
type ObservationReadPayload struct {
	ReadEventID      string `json:"read_event_id,omitempty"`
	ModelExecutionID string `json:"model_execution_id,omitempty"`
	ObservationID    string `json:"observation_id"`
	SourceRunID      string `json:"source_run_id"`
	SourceTaskID     string `json:"source_task_id"`
	SourceCallID     string `json:"source_call_id"`
	ObservedAt       string `json:"observed_at"`
	SHA256           string `json:"sha256"`
	ContentSHA256    string `json:"content_sha256"`
	ContentFormat    string `json:"content_format"`
	Tool             string `json:"tool"`
	ReadCallID       string `json:"read_call_id"`
	Offset           int    `json:"offset"`
	Bytes            int    `json:"bytes"`
	TotalBytes       int    `json:"total_bytes"`
}

func validateToolObservationEvent(event RunEvent) error {
	if event.TaskID == "" || event.Attempt < 1 {
		return fmt.Errorf("tool observation requires a task attempt")
	}
	if event.Type == string(EventToolObservationRead) {
		var p ObservationReadPayload
		if err := json.Unmarshal(event.Payload, &p); err != nil {
			return err
		}
		if p.ObservationID == "" || p.SourceRunID == "" || p.SourceTaskID == "" || p.SourceCallID == "" || p.Tool == "" || p.ReadCallID == "" || !decisionDigestPattern.MatchString(p.SHA256) || !decisionDigestPattern.MatchString(p.ContentSHA256) || (p.ContentFormat != "tool_output" && p.ContentFormat != "evidence_text") || p.Offset < 0 || p.Bytes < 0 || p.Bytes > maxObservationReadBytes || p.TotalBytes < p.Offset+p.Bytes || p.TotalBytes > maxObservationBytes {
			return fmt.Errorf("invalid observation read identity or range")
		}
		_, err := time.Parse(time.RFC3339Nano, p.ObservedAt)
		return err
	}
	var p ToolObservationPayload
	if err := json.Unmarshal(event.Payload, &p); err != nil {
		return err
	}
	r := p.Artifact
	if p.TeamID == "" || p.Tool == "" || !validArtifactID(r.ID) || r.Kind != "tool_observation" || !decisionDigestPattern.MatchString(r.SHA256) || r.ByteSize < 1 || r.ByteSize > 2*maxObservationBytes || r.RunID != event.RunID || r.TaskID != event.TaskID || r.Attempt != event.Attempt || r.Agent != event.Actor || r.ToolCallID == "" || r.CreatedAt.IsZero() {
		return fmt.Errorf("invalid tool observation publication")
	}
	return nil
}

func observationContentHash(content string) string {
	digest := sha256.Sum256([]byte(content))
	return hex.EncodeToString(digest[:])
}

// The grant is explicit: adding a new runtime tool must not silently retain
// every existing worker's observations. Coordinator/sidecar and bound-workset
// paths do not acquire historical access through this helper.
func (c *Coordinator) observationWorker(ctx context.Context) (*agent.AgentDef, *TodoItem) {
	if c == nil || c.session == nil || c.taskTracker == nil || c.ExecutionProfile().DisableHistoricalMemory || c.toolDeniedByTeam(recallObservationToolName) {
		return nil, nil
	}
	name, _ := ctx.Value(tools.AgentNameKey).(string)
	def := c.session.Agents[strings.ToLower(strings.TrimSpace(name))]
	id, _ := ctx.Value(todoIDKey{}).(string)
	item := c.todoItemByID(id)
	if def == nil || !explicitlyDeclaresTool(def.Tools, recallObservationToolName) || item == nil || item.WorksetBinding != nil || !strings.EqualFold(item.Agent, def.Name) || id == CoordTodoID {
		return nil, nil
	}
	return def, item
}

// captureToolObservation runs only after the normal gate and actual tool
// execution. Failure merely disables reuse; it never replays the source tool.
func (c *Coordinator) captureToolObservation(ctx context.Context, call fantasy.ToolCall, name, input, output string) {
	def, item := c.observationWorker(ctx)
	attempt, _ := ctx.Value(executionAttemptKey{}).(int)
	if def == nil || attempt < 1 || call.ID == "" || !c.hasDurableEventJournal() || !c.reusableObservationTool(name, input) || len(input) > 64<<10 || len(output) == 0 || len(output) > maxObservationBytes || !utf8.ValidString(output) {
		return
	}
	owner := c.observationOwner()
	modelExecutionID := c.observationModelExecutionID(ctx)
	teamID := strings.ToLower(strings.TrimSpace(c.session.Config.Name))
	if teamID == "" {
		return
	}
	owner.observationCaptureMu.Lock()
	defer owner.observationCaptureMu.Unlock()
	events, err := queryJournalEvents(ctx, c.EventJournal(), EventQuery{
		RunID: c.executionRunID, TaskID: item.ID, Attempt: attempt,
		Types: []string{string(EventToolObservationRecorded)},
	})
	if err != nil {
		return
	}
	count := 0
	for _, e := range events {
		if e.RunID == c.executionRunID && e.TaskID == item.ID && e.Attempt == attempt && e.Type == string(EventToolObservationRecorded) {
			var previous ToolObservationPayload
			if json.Unmarshal(e.Payload, &previous) == nil && previous.TeamID == teamID && previous.Artifact.ToolCallID == call.ID && previous.ModelExecutionID == modelExecutionID {
				return
			}
			count++
		}
	}
	if count >= maxObservationsPerAttempt {
		return
	}
	body := storedToolObservation{Input: utils.RedactSecrets(input), Output: utils.RedactSecrets(output)}
	if len(body.Input) > 64<<10 || len(body.Output) > maxObservationBytes || !utf8.ValidString(body.Output) {
		return
	}
	raw, err := json.Marshal(body)
	if err != nil || len(raw) > 2*maxObservationBytes {
		return
	}
	store, err := NewFileArtifactStore(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		return
	}
	stored, err := store.Put(ctx, PutArtifactRequest{
		ID:   "observation-" + hashContentKey(teamID+"\x00"+c.executionRunID+"\x00"+item.ID+"\x00"+fmt.Sprint(attempt)+"\x00"+modelExecutionID+"\x00"+call.ID+"\x00"+string(raw)),
		Kind: "tool_observation", Role: "historical_observation", Path: "observations/" + item.ID,
		Content: raw, MediaType: "application/json", RunID: c.executionRunID, TaskID: item.ID,
		Attempt: attempt, Agent: def.Name, ToolCallID: call.ID, CreatedAt: time.Now().UTC(),
	})
	if err != nil || store.Verify(ctx, stored.ArtifactRef) != nil {
		return
	}
	payload, err := json.Marshal(ToolObservationPayload{TeamID: teamID, Tool: name, Artifact: stored.ArtifactRef, ModelExecutionID: modelExecutionID})
	if err != nil {
		return
	}
	_, _ = c.EventJournal().Append(ctx, RunEvent{Type: string(EventToolObservationRecorded), RunID: c.executionRunID, TaskID: item.ID, Attempt: attempt, Actor: def.Name, Payload: payload, IdempotencyKey: "observation:" + stored.ID})
}

func (c *Coordinator) reusableObservationTool(name, input string) bool {
	// Protocol, recall, random and orchestration outputs are not source
	// observations. Do not turn a historical read into a fresh observation.
	switch name {
	case "view":
		var arguments struct {
			ArtifactRef string `json:"artifact_ref"`
		}
		if json.Unmarshal([]byte(input), &arguments) != nil || arguments.ArtifactRef != "" {
			return false // Dependency artifact grants do not transfer to a later task.
		}
		return true
	case "grep", "glob", "ls", "bash", "web_search", "web_fetch":
		return c.readOnlyToolCall(name, input)
	default:
		return c.declaredMCPObservationTool(name)
	}
}

func (c *Coordinator) observationLineage(ctx context.Context) ([]RunEvent, error) {
	if !c.hasDurableEventJournal() {
		return nil, fmt.Errorf("historical observations require a durable event journal")
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		return nil, err
	}
	tree, err := LoadSessionTree(c.observationOwner().session.Workspace)
	if err != nil {
		return nil, err
	}
	return ProjectValidatedEventsForBranch(events, tree, tree.ActiveBranch)
}

func (c *Coordinator) observationOwner() *Coordinator {
	for c.modelFanout != nil && c.modelFanout.parent != nil {
		c = c.modelFanout.parent
	}
	return c
}

func (c *Coordinator) observationModelExecutionID(ctx context.Context) string {
	if metadata, ok := invocationMetadataFromContext(ctx); ok && metadata.ModelExecutionID != "" {
		return metadata.ModelExecutionID
	}
	return c.modelExecutionID
}

func (c *Coordinator) loadToolObservation(ctx context.Context, event RunEvent) (ToolObservationPayload, storedToolObservation, error) {
	var p ToolObservationPayload
	var body storedToolObservation
	if err := validateToolObservationEvent(event); err != nil {
		return p, body, err
	}
	if err := json.Unmarshal(event.Payload, &p); err != nil {
		return p, body, err
	}
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.projectDir)
	if err != nil {
		return p, body, err
	}
	resolved, err := store.Resolve(ctx, p.Artifact)
	if err != nil || !sameArtifactRef(resolved, p.Artifact) {
		return p, body, fmt.Errorf("observation metadata does not match its journal publication")
	}
	if err := store.Verify(ctx, p.Artifact); err != nil {
		return p, body, err
	}
	r, err := store.Open(ctx, p.Artifact.ID)
	if err != nil {
		return p, body, err
	}
	defer func() { _ = r.Close() }()
	raw, err := io.ReadAll(io.LimitReader(r, 2*maxObservationBytes+1))
	if err != nil || len(raw) > 2*maxObservationBytes {
		return p, body, fmt.Errorf("observation exceeds storage bounds")
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return p, body, err
	}
	if len(body.Output) > maxObservationBytes || len(body.Input) > 64<<10 {
		return p, body, fmt.Errorf("observation exceeds content bounds")
	}
	if denial := c.workerMCPArgumentDenial(p.Tool, body.Input); denial != "" {
		return p, body, fmt.Errorf("observation arguments violate the current policy")
	}
	return p, body, nil
}
