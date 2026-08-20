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

// autoSelectTeam picks the team best suited to the prompt. It prefers an LLM
// match via the sidecar and falls back to keyword scoring. It returns the
// chosen team name and the method used ("only"/"llm"/"keyword"), or "" when it
// cannot decide (no signal) so the caller can fall back to the interactive
// picker.
func autoSelectTeam(ctx context.Context, prompt string, registry *team.TeamRegistry) (name, method string) {
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

	if picked := keywordBestTeam(prompt, candidates); picked != "" {
		return picked, "keyword"
	}
	return "", ""
}

// gatherTeamSummaries builds (name, description) candidates for each discovered
// team. The description comes from team.yaml; when absent, the agents' own
// descriptions are concatenated so keyword/LLM matching still has signal.
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

// keywordBestTeam scores each candidate by how many distinct prompt words also
// appear in the team's name + description, and returns the highest scorer.
// Returns "" when nothing overlaps so the caller can fall back rather than pick
// arbitrarily.
func keywordBestTeam(prompt string, candidates []sidecar.TeamSummary) string {
	promptWords := tokenize(prompt)
	if len(promptWords) == 0 {
		return ""
	}
	best := ""
	bestScore := 0
	for _, c := range candidates {
		hay := tokenSet(c.Name + " " + c.Description)
		score := 0
		for w := range promptWords {
			if hay[w] {
				score++
			}
		}
		if score > bestScore {
			bestScore = score
			best = c.Name
		}
	}
	if bestScore == 0 {
		return ""
	}
	return best
}

// buildSelectionSidecar constructs a preflight coordinator for team matching.
// It never returns a raw sidecar: the coordinator opens the explicit workspace
// repository/event lineage and installs the prompt preparer before a model can
// be called. Returning nil selects the deterministic keyword fallback.
func buildSelectionSidecar(ctx context.Context) *preflightSidecarHandle {
	_ = ctx // coordinator-sidecar initialization is lazy; generation uses the caller context.
	cfg := config.LoadConfig()
	model := firstNonEmpty(opts.sidecarModelOverride, opts.modelOverride, cfg.SidecarModel, cfg.Model)
	if model == "" {
		return nil
	}
	url := config.ResolveProviderURL(opts.providerURL, "", "")
	key := config.ResolveProviderAPIKey(opts.providerAPIKey, "")
	workspace := getWorkspace()
	session := &team.TeamSession{Workspace: workspace, Config: agent.TeamConfig{Name: "preflight-team-selection", Providers: cfg.Providers}}
	coordinator, err := team.NewCoordinator(session, url, key, nil, nil, nil, team.RoleModels{Sidecar: model}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		return nil
	}
	handle, err := preparePreflightSidecarContext(ctx, coordinator)
	if err != nil {
		return nil
	}
	return handle
}

var tokenSplitRe = func() func(rune) bool {
	return func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	}
}()

// stopWords are common words that carry no team-selection signal.
var stopWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "to": true,
	"of": true, "for": true, "in": true, "on": true, "with": true, "please": true,
	"this": true, "that": true, "is": true, "are": true, "be": true, "do": true,
	"make": true, "help": true, "me": true, "my": true, "i": true, "we": true,
}

func tokenize(s string) map[string]bool {
	return tokenSet(s)
}

func tokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), tokenSplitRe) {
		if len(w) < 2 || stopWords[w] {
			continue
		}
		out[singularize(w)] = true
	}
	return out
}

// singularize strips a trailing plural "s" so "manuals" matches "manual".
// Applied to both sides so the comparison stays symmetric.
func singularize(w string) string {
	if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
		return w[:len(w)-1]
	}
	return w
}
