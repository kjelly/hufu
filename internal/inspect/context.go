package inspect

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

type ContextOptions struct {
	ShowContent bool
	AllAgents   bool
}

type ContextManifestItemData struct {
	ContextItemID       string `json:"context_item_id"`
	ManifestFingerprint string `json:"manifest_fingerprint"`
	RequestID           string `json:"request_id"`
	Agent               string `json:"agent,omitempty"`
	Attempt             int    `json:"attempt"`
	ModelExecutionID    string `json:"model_execution_id,omitempty"`
	Kind                string `json:"kind,omitempty"`
	Source              string `json:"source,omitempty"`
	Included            bool   `json:"included"`
	Tokens              int    `json:"tokens,omitzero"`
	Reason              string `json:"reason"`
}

type MemoryManifestItemData struct {
	ContextItemID       string                `json:"context_item_id"`
	RetrievalID         string                `json:"retrieval_id"`
	ManifestFingerprint string                `json:"manifest_fingerprint"`
	PolicyVersion       string                `json:"policy_version"`
	Agent               string                `json:"agent,omitempty"`
	Attempt             int                   `json:"attempt"`
	Rank                int                   `json:"rank"`
	Source              string                `json:"source,omitempty"`
	TokenCount          int                   `json:"token_count,omitzero"`
	BaseScore           float64               `json:"base_score"`
	FinalScore          float64               `json:"final_score"`
	ScoreParts          team.MemoryScoreParts `json:"score_parts"`
}

type ContextItemData struct {
	ID         string                        `json:"id"`
	Kind       contextstore.ContextKind      `json:"kind,omitempty"`
	SourceType string                        `json:"source_type,omitempty"`
	SourceRef  string                        `json:"source_ref,omitempty"`
	Authority  contextstore.Authority        `json:"authority,omitempty"`
	Trust      contextstore.TrustLevel       `json:"trust,omitempty"`
	Scope      contextstore.Scope            `json:"scope,omitzero"`
	Lifecycle  contextstore.ContextLifecycle `json:"lifecycle,omitempty"`
	Available  bool                          `json:"available"`
	Content    string                        `json:"content,omitempty"`
}

type ContextAuthorizationData struct {
	ProjectID   string `json:"project_id"`
	AgentID     string `json:"agent_id,omitempty"`
	AllAgents   bool   `json:"all_agents,omitzero"`
	ShowContent bool   `json:"show_content,omitzero"`
	Status      string `json:"status"`
}

type ContextData struct {
	RunID           string                    `json:"run_id"`
	TaskID          string                    `json:"task_id"`
	Attempts        []int                     `json:"attempts"`
	Manifests       []ContextManifestItemData `json:"manifests"`
	MemoryManifests []MemoryManifestItemData  `json:"memory_manifests"`
	Items           []ContextItemData         `json:"items"`
	Authorization   ContextAuthorizationData  `json:"authorization"`
}

func InspectContext(ctx context.Context, query InspectQuery, options ContextOptions) (*Envelope, error) {
	if err := query.Validate(KindContext); err != nil {
		return nil, err
	}
	if options.AllAgents && strings.TrimSpace(query.AgentID) != "" {
		return nil, fmt.Errorf("%w: agent and all-agents are mutually exclusive", ErrInvalidQuery)
	}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return nil, err
	}
	selected, err := selectRun(lineage, query)
	if err != nil {
		return nil, err
	}
	tasks, err := team.ReplayTodoList(selected.runEvents)
	if err != nil {
		return nil, fmt.Errorf("%w: replay tasks for run %q: %v", ErrIntegrity, query.RunID, err)
	}
	var task *team.TodoItem
	for _, candidate := range tasks {
		if candidate != nil && candidate.ID == query.TaskID {
			if task != nil {
				return nil, fmt.Errorf("%w: task %q in run %q", ErrAmbiguous, query.TaskID, query.RunID)
			}
			task = candidate
		}
	}
	if task == nil {
		return nil, fmt.Errorf("%w: task %q in run %q", ErrNotFound, query.TaskID, query.RunID)
	}

	data := ContextData{
		RunID: query.RunID, TaskID: query.TaskID, Attempts: []int{},
		Manifests: []ContextManifestItemData{}, MemoryManifests: []MemoryManifestItemData{}, Items: []ContextItemData{},
		Authorization: ContextAuthorizationData{ProjectID: query.ProjectID, AgentID: query.AgentID, AllAgents: options.AllAgents, ShowContent: options.ShowContent, Status: "authorized"},
	}
	itemIDs := make(map[string]struct{})
	for _, manifest := range task.ContextManifests {
		if query.Attempt > 0 && manifest.Attempt != query.Attempt {
			continue
		}
		data.Attempts = append(data.Attempts, manifest.Attempt)
		for _, item := range manifest.Items {
			itemID := strings.TrimPrefix(item.ID, "context:")
			itemIDs[itemID] = struct{}{}
			data.Manifests = append(data.Manifests, ContextManifestItemData{
				ContextItemID: itemID, ManifestFingerprint: manifest.Fingerprint, RequestID: manifest.RequestID,
				Agent: manifest.Agent, Attempt: manifest.Attempt, ModelExecutionID: manifest.ModelExecutionID,
				Kind: item.Kind, Source: contextstore.RedactSecrets(item.Source), Included: item.Included,
				Tokens: item.Tokens, Reason: string(item.Reason),
			})
		}
	}
	for _, manifest := range task.MemoryManifests {
		if query.Attempt > 0 && manifest.Attempt != query.Attempt {
			continue
		}
		data.Attempts = append(data.Attempts, manifest.Attempt)
		for _, item := range manifest.Items {
			itemIDs[item.ContextItemID] = struct{}{}
			data.MemoryManifests = append(data.MemoryManifests, MemoryManifestItemData{
				ContextItemID: item.ContextItemID, RetrievalID: manifest.RetrievalID, ManifestFingerprint: manifest.Fingerprint,
				PolicyVersion: manifest.PolicyVersion, Agent: manifest.Agent, Attempt: manifest.Attempt,
				Rank: item.Rank, Source: contextstore.RedactSecrets(item.Source), TokenCount: item.TokenCount, BaseScore: item.BaseScore,
				FinalScore: item.FinalScore, ScoreParts: item.ScoreParts,
			})
		}
	}
	data.Attempts = uniqueAttempts(data.Attempts)
	if query.Attempt > 0 && len(data.Attempts) == 0 {
		return nil, fmt.Errorf("%w: attempt %d for task %q in run %q", ErrNotFound, query.Attempt, query.TaskID, query.RunID)
	}
	data.Items, err = loadContextItems(ctx, query, options, itemIDs)
	if err != nil {
		return nil, err
	}
	return envelope(KindContext, query, lineage.BranchID, data), nil
}

func loadContextItems(ctx context.Context, query InspectQuery, options ContextOptions, ids map[string]struct{}) ([]ContextItemData, error) {
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			ordered = append(ordered, id)
		}
	}
	slices.Sort(ordered)
	out := make([]ContextItemData, 0, len(ordered))
	dbPath := filepath.Join(query.Workspace, "context.sqlite")
	repo, err := contextstore.OpenSQLiteReadOnly(dbPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			for _, id := range ordered {
				out = append(out, ContextItemData{ID: id})
			}
			return out, nil
		}
		return nil, fmt.Errorf("%w: open context database read-only: %v", ErrIntegrity, err)
	}
	defer func() { _ = repo.Close() }()
	for _, id := range ordered {
		item, getErr := repo.Get(ctx, id)
		if getErr != nil {
			if errors.Is(getErr, sql.ErrNoRows) {
				out = append(out, ContextItemData{ID: id})
				continue
			}
			return nil, fmt.Errorf("%w: read context item %q: %v", ErrIntegrity, id, getErr)
		}
		if item.Scope.ProjectID != query.ProjectID || query.TeamID != "" && item.Scope.TeamID != query.TeamID {
			return nil, fmt.Errorf("%w: context item %q is outside requested project/team scope", ErrUnauthorized, id)
		}
		if item.Scope.AgentID != "" && item.Scope.AgentID != query.AgentID && !options.AllAgents {
			return nil, fmt.Errorf("%w: private context item %q requires matching agent or all-agents", ErrUnauthorized, id)
		}
		projected := ContextItemData{
			ID: item.ID, Kind: item.Kind, SourceType: item.Source.Type, SourceRef: utils.RedactSecrets(item.Source.Ref),
			Authority: item.Authority, Trust: item.TrustLevel, Scope: item.Scope, Lifecycle: item.Lifecycle, Available: true,
		}
		if options.ShowContent {
			projected.Content = contextstore.RedactSecrets(item.Content)
		}
		out = append(out, projected)
	}
	return out, nil
}

func uniqueAttempts(values []int) []int {
	filtered := values[:0]
	for _, value := range values {
		if value > 0 {
			filtered = append(filtered, value)
		}
	}
	slices.SortFunc(filtered, cmp.Compare[int])
	return slices.Compact(filtered)
}
