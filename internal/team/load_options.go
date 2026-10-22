package team

// TeamLoadOptions controls discovery inputs, independently of execution policy.
type TeamLoadOptions struct {
	// SkillSearchPaths replaces the usual team, project and user skill search
	// paths when non-nil. An empty non-nil slice disables skill discovery.
	// Callers requiring reproducibility must provide absolute, validated paths.
	SkillSearchPaths []string
}

// LoadTeamWithOptions loads a team with explicit discovery inputs. The default
// options preserve LoadTeam behavior.
func LoadTeamWithOptions(teamDir string, vars map[string]string, forcedSkills []string, registry *ProviderRegistry, options TeamLoadOptions) (*TeamSession, error) {
	return loadTeamWithOptions(teamDir, vars, forcedSkills, registry, TeamCompileRuntime, nil, options)
}
