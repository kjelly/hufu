package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
)

// contextPreflightCoordinator is the narrow ownership boundary needed by
// CLI-owned sidecar invocations. It deliberately does not expose ordinary
// coordinator execution or any tools.
type contextPreflightCoordinator interface {
	PrepareContextPreflight() error
	CloseContextPreflight()
	Sidecar() *sidecar.Sidecar
}

type contextPreflightContextCoordinator interface {
	PrepareContextPreflightContext(context.Context) error
	ContextPreflight() context.Context
}

// preflightSidecarHandle keeps the coordinator's cleanup reachable for the
// entire lifetime of a CLI sidecar call. It is intentionally private: callers
// only need the sidecar for one decision and must not retain it.
type preflightSidecarHandle struct {
	sidecar *sidecar.Sidecar
	ctx     context.Context
	close   func()
	once    sync.Once
}

func (h *preflightSidecarHandle) Context() context.Context {
	if h == nil || h.ctx == nil {
		return context.Background()
	}
	return h.ctx
}

func (h *preflightSidecarHandle) Sidecar() *sidecar.Sidecar {
	if h == nil {
		return nil
	}
	return h.sidecar
}

func (h *preflightSidecarHandle) Close() {
	if h == nil {
		return
	}
	h.once.Do(func() {
		if h.close != nil {
			h.close()
		}
	})
}

// preparePreflightSidecar establishes the context boundary before returning a
// sidecar and makes every failure path release the coordinator immediately.
func preparePreflightSidecar(coordinator contextPreflightCoordinator) (*preflightSidecarHandle, error) {
	return preparePreflightSidecarContext(context.Background(), coordinator)
}

func preparePreflightSidecarContext(ctx context.Context, coordinator contextPreflightCoordinator) (*preflightSidecarHandle, error) {
	if coordinator == nil {
		return nil, fmt.Errorf("context preflight coordinator is unavailable")
	}
	var err error
	callCtx := ctx
	if scoped, ok := coordinator.(contextPreflightContextCoordinator); ok {
		err = scoped.PrepareContextPreflightContext(ctx)
		callCtx = scoped.ContextPreflight()
	} else {
		err = coordinator.PrepareContextPreflight()
	}
	if err != nil {
		coordinator.CloseContextPreflight()
		return nil, err
	}
	s := coordinator.Sidecar()
	if s == nil {
		coordinator.CloseContextPreflight()
		return nil, fmt.Errorf("sidecar is unavailable after context preflight")
	}
	return &preflightSidecarHandle{sidecar: s, ctx: callCtx, close: coordinator.CloseContextPreflight}, nil
}

var selectionSidecarBuilder = buildSelectionSidecar

var matchTeamWithSelectionSidecar = func(ctx context.Context, s *sidecar.Sidecar, prompt string, candidates []sidecar.TeamSummary) (string, error) {
	return s.MatchTeam(sidecar.WithPurpose(ctx, "team_selection"), prompt, candidates)
}

// autoSelectTeam picks the team best suited to the prompt through a
// schema-validated sidecar decision. A sole candidate is an unambiguous
// deterministic choice. Resolver failures return no choice so the caller can
// request explicit selection instead of guessing from task prose.
func autoSelectTeam(ctx context.Context, prompt string, registry *team.TeamRegistry) (name, method string) {
	if registry == nil {
		return "", ""
	}
	candidates := gatherTeamSummaries(registry)
	if len(candidates) == 0 {
		return "", ""
	}
	if len(candidates) == 1 {
		return candidates[0].Name, "only"
	}

	if handle := selectionSidecarBuilder(ctx); handle != nil {
		defer handle.Close()
		if s := handle.Sidecar(); s != nil {
			if picked, err := matchTeamWithSelectionSidecar(handle.Context(), s, prompt, candidates); err == nil && picked != "" {
				return picked, "llm"
			}
		}
	}

	return "", ""
}

// gatherTeamSummaries builds (name, description) candidates for each discovered
// team. The description comes from team.yaml; when absent, the agents' own
// descriptions are concatenated so model-backed matching still has signal.
func gatherTeamSummaries(registry *team.TeamRegistry) []sidecar.TeamSummary {
	var out []sidecar.TeamSummary
	for _, name := range registry.ListTeams() {
		dir, err := registry.Resolve(name)
		if err != nil {
			continue
		}
		out = append(out, sidecar.TeamSummary{
			Name:        name,
			Description: teamDescription(dir),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// teamDescription reads the `description` field from team.yaml/team.yml, or
// falls back to joining the agent .md descriptions in the directory.
func teamDescription(dir string) string {
	for _, fn := range []string{"team.yaml", "team.yml"} {
		data, err := os.ReadFile(filepath.Join(dir, fn))
		if err != nil {
			continue
		}
		var cfg struct {
			Description string `yaml:"description"`
		}
		if yaml.Unmarshal(data, &cfg) == nil && strings.TrimSpace(cfg.Description) != "" {
			return cfg.Description
		}
		break
	}
	// Fall back to agent descriptions.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var descs []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		fm := readAgentFrontmatter(filepath.Join(dir, e.Name()))
		if fm.Description != "" {
			descs = append(descs, fm.Description)
		}
	}
	return strings.Join(descs, "; ")
}

// buildSelectionSidecar constructs a preflight coordinator for team matching.
// It never returns a raw sidecar: the coordinator opens the explicit workspace
// repository/event lineage and installs the prompt preparer before a model can
// be called. Returning nil leaves selection unresolved.
func buildSelectionSidecar(ctx context.Context) *preflightSidecarHandle {
	_ = ctx // coordinator-sidecar initialization is lazy; generation uses the caller context.
	cfg := config.LoadConfig()
	// Team selection is an auxiliary LLM role. It must not silently inherit a
	// worker or coordinator target, either of which may be an agent backend.
	model := firstNonEmpty(opts.sidecarModelOverride, cfg.SidecarModel)
	if model == "" {
		return nil
	}
	url := config.ResolveProviderURL(opts.providerURL, "", "")
	key := config.ResolveProviderAPIKey(opts.providerAPIKey, "")
	session := &team.TeamSession{Config: agent.TeamConfig{Name: "preflight-team-selection", Providers: cfg.Providers}}
	if err := preflightSidecarTarget(session, cfg, model, nil); err != nil {
		return nil
	}
	session.Workspace = getWorkspace()
	if session.Workspace == "" {
		return nil
	}
	if err := session.SetCompatibilityWorkspaceScope(runtimeSubjectRoot()); err != nil {
		return nil
	}
	coordinator, err := team.NewCoordinator(session, url, key, nil, nil, nil, team.RoleModels{Sidecar: model}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		return nil
	}
	handle, err := preparePreflightSidecarContext(ctx, coordinator)
	if err != nil {
		_ = coordinator.Close()
		return nil
	}
	closePreflight := handle.close
	handle.close = func() {
		closePreflight()
		_ = coordinator.Close()
	}
	return handle
}
