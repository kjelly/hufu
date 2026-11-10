package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var ErrHistoricalTaskReaderUnavailable = errors.New("canonical historical task reader unavailable")

// HistoricalTaskScope is runtime-owned. A worker cannot supply a different
// workspace, team, branch or an all-branches override through team_info.
type HistoricalTaskScope struct {
	Workspace string
	Team      string
	Branch    string
}

// HistoricalTaskResult is a detached, redacted reference backed by a verified
// event. Validity is evaluated on the consumer lineage, not across all branches.
type HistoricalTaskResult struct {
	ResultID       string      `json:"result_id"`
	Team           string      `json:"team"`
	SessionID      string      `json:"session_id"`
	SourceBranchID string      `json:"source_branch_id"`
	RunID          string      `json:"run_id"`
	OriginRunID    string      `json:"origin_run_id"`
	TaskID         string      `json:"task_id"`
	Agent          string      `json:"agent"`
	Attempt        int         `json:"attempt"`
	Status         string      `json:"status"`
	CurrentStatus  string      `json:"current_status"`
	Validity       string      `json:"validity"`
	Description    string      `json:"description,omitempty"`
	Summary        string      `json:"summary,omitempty"`
	Output         string      `json:"output,omitempty"`
	TypedResult    *TaskResult `json:"typed_result,omitempty"`
	SourceEventID  string      `json:"source_event_id"`
	SourceHash     string      `json:"source_hash"`
	PayloadSHA256  string      `json:"payload_sha256,omitempty"`
	// TypedResultJSON preserves the complete detached projection, including
	// fields a newer writer added. It is runtime data, not another model input.
	TypedResultJSON json.RawMessage `json:"-"`
	Workspace       string          `json:"-"`
}

// HistoricalTaskReader is implemented by the command layer's inspect adapter;
// team must not import inspect (which already imports team).
type HistoricalTaskReader interface {
	Load(context.Context, HistoricalTaskScope) ([]HistoricalTaskResult, error)
}

func (c *Coordinator) SetHistoricalTaskReader(reader HistoricalTaskReader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.historicalTaskReader = reader
}

func (c *Coordinator) HistoricalTaskReader() HistoricalTaskReader {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.historicalTaskReader
}

func (c *Coordinator) readHistoricalTaskResults(ctx context.Context) ([]HistoricalTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.ExecutionProfile().DisableHistoricalTaskReuse {
		return nil, fmt.Errorf("historical task inspection is disabled by the active execution profile")
	}
	reader := c.HistoricalTaskReader()
	if reader == nil || c.session == nil || c.session.Config.Name == "" {
		return nil, ErrHistoricalTaskReaderUnavailable
	}
	// Clones retain their parent's canonical control/artifact root and journal.
	// Never use an isolated leaf's disposable Workspace as a history store.
	root := c.session.Scope.ControlRoot
	if root == "" {
		root = c.artifactStoreRootPath()
	}
	branch := c.recoveryBranch
	if branch == "" {
		store := c.EventStore()
		if store == nil {
			return nil, ErrHistoricalTaskReaderUnavailable
		}
		branch = store.BranchID()
		if branch == "" {
			branch = "main"
		} // the journal's documented implicit legacy root
	}
	if strings.TrimSpace(root) == "" {
		return nil, ErrHistoricalTaskReaderUnavailable
	}
	return reader.Load(ctx, HistoricalTaskScope{Workspace: root, Team: strings.ToLower(strings.TrimSpace(c.session.Config.Name)), Branch: branch})
}
