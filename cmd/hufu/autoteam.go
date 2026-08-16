package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
)

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

	if s := buildSelectionSidecar(ctx); s != nil {
		if picked, err := s.MatchTeam(sidecar.WithPurpose(ctx, "team_selection"), prompt, candidates); err == nil && picked != "" {
			return picked, "llm"
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
func buildSelectionSidecar(ctx context.Context) *sidecar.Sidecar {
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
	if err := coordinator.PrepareContextPreflight(); err != nil {
		coordinator.CloseContextPreflight()
		return nil
	}
	return coordinator.Sidecar()
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
