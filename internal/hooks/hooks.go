package hooks

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type HookResult int

const (
	HookContinue HookResult = iota
	HookSkip
	HookReplace
	HookError
)

type HookContext struct {
	TeamName  string `json:"team_name"`
	AgentName string `json:"agent_name"`
	TaskID    string `json:"task_id"`
	TaskDesc  string `json:"task_desc"`
	Model     string `json:"model"`
	SessionID string `json:"session_id"`
	Timestamp string `json:"timestamp"`
}

type teamNameKeyType struct{}

var TeamNameKey = teamNameKeyType{}

type agentNameKeyType struct{}

var AgentNameKey = agentNameKeyType{}

type taskDescKeyType struct{}

var TaskDescKey = taskDescKeyType{}

type HookPayload struct {
	HookPoint string          `json:"hook_point"`
	Context   HookContext     `json:"context"`
	ToolName  string          `json:"tool_name,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
	Result    string          `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Model     string          `json:"model,omitempty"`
	Response  string          `json:"response,omitempty"`
	Usage     UsageSummary    `json:"usage,omitempty"`
}

type UsageSummary struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type HookResponse struct {
	Result       HookResult      `json:"result"`
	Replacement  json.RawMessage `json:"replacement,omitempty"`
	ErrorMessage string          `json:"error_message,omitempty"`
}

type HookFunc func(ctx context.Context, payload HookPayload) HookResponse

type HookRegistry struct {
	mu    sync.RWMutex
	hooks map[string][]HookFunc
}

func NewHookRegistry() *HookRegistry {
	return &HookRegistry{
		hooks: make(map[string][]HookFunc),
	}
}

func (r *HookRegistry) Register(hookPoint string, fn HookFunc) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hooks[hookPoint] = append(r.hooks[hookPoint], fn)
}

func (r *HookRegistry) Dispatch(ctx context.Context, hookPoint string, payload HookPayload) HookResponse {
	r.mu.RLock()
	fns := r.hooks[hookPoint]
	r.mu.RUnlock()

	if len(fns) == 0 {
		return HookResponse{Result: HookContinue}
	}

	if payload.Context.Timestamp == "" {
		payload.Context.Timestamp = time.Now().Format(time.RFC3339)
	}

	var lastResp HookResponse
	for _, fn := range fns {
		resp := fn(ctx, payload)
		if resp.Result == HookError {
			return resp
		}
		if resp.Result != HookContinue {
			lastResp = resp
			if resp.Result == HookReplace {
				if resp.Replacement != nil {
					payload.Args = resp.Replacement
				}
			}
		}
	}

	if lastResp.Result != HookContinue {
		return lastResp
	}
	return HookResponse{Result: HookContinue}
}

func (r *HookRegistry) HasHooks(hookPoint string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.hooks[hookPoint]) > 0
}

func MakeContext(teamName, agentName, taskID, taskDesc, model, sessionID string) HookContext {
	return HookContext{
		TeamName:  teamName,
		AgentName: agentName,
		TaskID:    taskID,
		TaskDesc:  taskDesc,
		Model:     model,
		SessionID: sessionID,
		Timestamp: time.Now().Format(time.RFC3339),
	}
}
