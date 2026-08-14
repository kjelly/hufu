package team

// Shared persistent knowledge lifecycle. Private worker memory is owned by
// WorkerMemoryService; this service owns team-visible candidates so shared
// LTM cannot fork into JSONL/Markdown state outside the canonical repository.

import (
	"context"
	"fmt"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
)

type SharedMemoryProposal struct {
	Scope      contextstore.Scope
	Content    string
	Section    string
	Category   string
	Source     string
	RunID      string
	TaskID     string
	Confidence *float64
	Evidence   []contextstore.EvidenceRef
	FilePaths  []string
	Supersedes []string
}

type SharedMemoryPromotion struct {
	Scope    contextstore.Scope
	Manifest *EvidenceManifest
}

type SharedMemoryRejection struct {
	Scope  contextstore.Scope
	RunID  string
	Reason string
}

type SharedMemoryService interface {
	Propose(context.Context, SharedMemoryProposal) (contextstore.ContextItem, error)
	ConfirmRun(context.Context, SharedMemoryPromotion) ([]contextstore.ContextItem, error)
	RejectRun(context.Context, SharedMemoryRejection) ([]contextstore.ContextItem, error)
}

type defaultSharedMemoryService struct{ repo contextstore.Repository }

func NewSharedMemoryService(repo contextstore.Repository) SharedMemoryService {
	if repo == nil {
		return nil
	}
	return &defaultSharedMemoryService{repo: repo}
}

func persistentSharedScope(scope contextstore.Scope) (contextstore.Scope, error) {
	if strings.TrimSpace(scope.ProjectID) == "" || strings.TrimSpace(scope.TeamID) == "" {
		return contextstore.Scope{}, fmt.Errorf("shared persistent memory requires trusted project/team scope")
	}
	return contextstore.Scope{ProjectID: scope.ProjectID, TeamID: scope.TeamID}, nil
}

func sharedMemoryKind(section string) contextstore.ContextKind {
	switch section {
	case ltmSectionConventions:
		return contextstore.ContextConvention
	case ltmSectionArchitecture:
		return contextstore.ContextArchitecture
	case ltmSectionIssues:
		return contextstore.ContextError
	case ltmSectionPatterns, ltmSectionFiles, ltmSectionTools:
		return contextstore.ContextPattern
	default:
		return contextstore.ContextPattern
	}
}

// memoryCategoryKind is the stable public memory_save vocabulary.  Categories
// are intentionally not free-form: callers may add their own prose tags, but
// canonical retrieval and extraction need an explicit semantic kind.
func memoryCategoryKind(category string) (contextstore.ContextKind, error) {
	switch strings.ToLower(strings.TrimSpace(category)) {
	case "":
		return "", nil
	case "decision":
		return contextstore.ContextDecision, nil
	case "convention":
		return contextstore.ContextConvention, nil
	case "architecture":
		return contextstore.ContextArchitecture, nil
	case "issue", "error", "lesson":
		return contextstore.ContextError, nil
	case "pattern":
		return contextstore.ContextPattern, nil
	case "observation", "finding":
		return contextstore.ContextObservation, nil
	case "verification":
		return contextstore.ContextVerification, nil
	case "artifact":
		return contextstore.ContextArtifact, nil
	case "requirement":
		return contextstore.ContextRequirement, nil
	case "instruction":
		return contextstore.ContextInstruction, nil
	case "summary":
		return contextstore.ContextSummary, nil
	default:
		return "", fmt.Errorf("unsupported memory category %q", category)
	}
}

func (s *defaultSharedMemoryService) Propose(ctx context.Context, req SharedMemoryProposal) (contextstore.ContextItem, error) {
	if s == nil || s.repo == nil {
		return contextstore.ContextItem{}, fmt.Errorf("shared memory repository is unavailable")
	}
	scope, err := persistentSharedScope(req.Scope)
	if err != nil {
		return contextstore.ContextItem{}, err
	}
	content := strings.TrimSpace(req.Content)
	if content == "" || strings.TrimSpace(req.RunID) == "" {
		return contextstore.ContextItem{}, fmt.Errorf("shared memory candidate requires content and active run")
	}
	section := strings.TrimSpace(req.Section)
	if section == "" {
		section = ltmSectionPatterns
	}
	confidence := 0.8
	if req.Confidence != nil {
		confidence = *req.Confidence
	}
	if confidence < 0 || confidence > 1 {
		return contextstore.ContextItem{}, fmt.Errorf("shared memory confidence must be between 0 and 1")
	}
	kind, err := memoryCategoryKind(req.Category)
	if err != nil {
		return contextstore.ContextItem{}, err
	}
	if kind == "" {
		kind = sharedMemoryKind(section)
	}
	evidence := append([]contextstore.EvidenceRef(nil), req.Evidence...)
	if strings.TrimSpace(req.TaskID) != "" {
		evidence = append(evidence, contextstore.EvidenceRef{Type: "task", Ref: req.TaskID})
	}
	for _, path := range req.FilePaths {
		if path = strings.TrimSpace(path); path != "" {
			evidence = append(evidence, contextstore.EvidenceRef{Type: "file_path", Ref: path})
		}
	}
	item := contextstore.ContextItem{
		Kind:       kind,
		Content:    content,
		Scope:      scope,
		Authority:  contextstore.AuthorityAgent,
		TrustLevel: contextstore.TrustInternal,
		Priority:   contextstore.PriorityNormal,
		Confidence: confidence,
		Source:     contextstore.SourceRef{Type: "shared_memory_candidate", Ref: strings.TrimSpace(req.Source)},
		Evidence:   evidence,
		Metadata: map[string]string{
			"visibility":      "shared",
			"memory_lifetime": "persistent",
			"legacy_section":  section,
			"category":        strings.TrimSpace(req.Category),
			"file_paths":      strings.Join(req.FilePaths, "\n"),
			"run_id":          req.RunID,
			"task_id":         req.TaskID,
			"source":          strings.TrimSpace(req.Source),
			"supersedes_ids":  strings.Join(req.Supersedes, "\n"),
		},
		Lifecycle: contextstore.LifecycleCandidate,
	}
	if err := validateSharedSupersedes(ctx, s.repo, scope, req.Supersedes); err != nil {
		return contextstore.ContextItem{}, err
	}
	return s.repo.UpsertCandidate(ctx, item)
}

func (s *defaultSharedMemoryService) ConfirmRun(ctx context.Context, req SharedMemoryPromotion) ([]contextstore.ContextItem, error) {
	if s == nil || s.repo == nil {
		return nil, fmt.Errorf("shared memory repository is unavailable")
	}
	if req.Manifest == nil || req.Manifest.Status != "accepted" || strings.TrimSpace(req.Manifest.RunID) == "" || strings.TrimSpace(req.Manifest.ManifestHash) == "" {
		return nil, fmt.Errorf("accepted manifest identity is incomplete")
	}
	scope, err := persistentSharedScope(req.Scope)
	if err != nil {
		return nil, err
	}
	items, err := s.repo.Query(ctx, contextstore.RepositoryQuery{Scope: scope, Visibility: contextstore.VisibilityExact, IncludeCandidates: true, Limit: 100000})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, item := range items {
		if item.Lifecycle == contextstore.LifecycleCandidate && item.Source.Type == "shared_memory_candidate" && item.Metadata["run_id"] == req.Manifest.RunID {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err = s.repo.ConfirmCandidates(ctx, ids, contextstore.CandidateBinding{
		Evidence: contextstore.EvidenceRef{Type: "evidence_manifest", Ref: req.Manifest.ManifestHash},
		Metadata: map[string]string{"manifest_hash": req.Manifest.ManifestHash},
	}); err != nil {
		return nil, err
	}
	return s.repo.GetMany(ctx, ids)
}

func sameContextScope(a, b contextstore.Scope) bool {
	return a.ProjectID == b.ProjectID && a.TeamID == b.TeamID && a.SessionID == b.SessionID &&
		a.BranchID == b.BranchID && a.AgentID == b.AgentID && a.TaskID == b.TaskID && a.AttemptID == b.AttemptID
}

func validateSharedSupersedes(ctx context.Context, repo contextstore.Repository, scope contextstore.Scope, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	items, err := repo.GetMany(ctx, ids)
	if err != nil {
		return fmt.Errorf("load superseded shared memory: %w", err)
	}
	for _, item := range items {
		if item.Lifecycle != contextstore.LifecycleConfirmed || item.SupersededBy != "" || !sameContextScope(item.Scope, scope) || item.Metadata["visibility"] != "shared" || item.Metadata["memory_lifetime"] != "persistent" {
			return fmt.Errorf("cannot supersede memory %q outside the current shared persistent identity", item.ID)
		}
	}
	return nil
}

func (s *defaultSharedMemoryService) RejectRun(ctx context.Context, req SharedMemoryRejection) ([]contextstore.ContextItem, error) {
	if s == nil || s.repo == nil {
		return nil, nil
	}
	if strings.TrimSpace(req.RunID) == "" {
		return nil, nil
	}
	scope, err := persistentSharedScope(req.Scope)
	if err != nil {
		return nil, err
	}
	items, err := s.repo.Query(ctx, contextstore.RepositoryQuery{Scope: scope, Visibility: contextstore.VisibilityExact, IncludeCandidates: true, Limit: 100000})
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for _, item := range items {
		if item.Lifecycle == contextstore.LifecycleCandidate && item.Source.Type == "shared_memory_candidate" && item.Metadata["run_id"] == req.RunID {
			ids = append(ids, item.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	if err = s.repo.UpdateLifecycle(ctx, ids, contextstore.LifecycleRejected); err != nil {
		return nil, err
	}
	return s.repo.GetMany(ctx, ids)
}
