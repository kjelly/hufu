package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/config"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
)

// maintenanceTextGenerator runs one prompt for an offline maintenance command
// (promotion drafting, conflict judging, consolidation drafting).
type maintenanceTextGenerator interface {
	GenerateText(ctx context.Context, prompt string) (string, error)
}

type maintenanceGeneratorOptions struct {
	TeamDir       string
	Workspace     string
	ModelOverride string
	// Purpose must be registered in team's context purpose registry.
	Purpose string
	Profile sidecar.Profile
}

type sidecarTextGenerator struct {
	s       *sidecar.Sidecar
	ctx     context.Context
	purpose string
	profile sidecar.Profile
}

func (g sidecarTextGenerator) GenerateText(ctx context.Context, prompt string) (string, error) {
	if g.ctx != nil {
		ctx = g.ctx
	}
	return g.s.ExecuteProfile(sidecar.WithPurpose(ctx, g.purpose), prompt, g.profile)
}

// newMaintenanceTextGenerator binds the team to the maintenance workspace and
// returns a sidecar-backed generator, the resolved model name, and a release
// function. The model resolves from the override, then the team sidecar and
// generation models, then the config sidecar and default models.
func newMaintenanceTextGenerator(ctx context.Context, options maintenanceGeneratorOptions) (maintenanceTextGenerator, string, func(), error) {
	session, err := team.LoadTeam(options.TeamDir, nil, nil, team.DefaultProviderRegistry)
	if err != nil {
		return nil, "", nil, err
	}
	cfg := config.LoadConfig()
	model := firstNonEmpty(options.ModelOverride, session.Config.SidecarModel, session.Config.Generation.Model, cfg.SidecarModel, cfg.Model)
	if model == "" {
		return nil, "", nil, fmt.Errorf("%s requires --model or a team/config sidecar/model", options.Purpose)
	}
	url := config.ResolveProviderURL(opts.providerURL, session.Config.ProviderURL, "")
	key := config.ResolveProviderAPIKey(opts.providerAPIKey, session.Config.ProviderAPIKey)
	if err := preflightSidecarTarget(session, cfg, model, nil); err != nil {
		return nil, "", nil, fmt.Errorf("%s execution target: %w", options.Purpose, err)
	}
	// A maintenance command is a CLI model invocation, but it still needs the
	// same repository, compiler, redaction, manifest, and event boundary as a
	// coordinator sidecar. Bind the loaded team to the maintenance workspace
	// before constructing the coordinator so the invocation lineage is
	// replayable next to context.sqlite rather than in an ambient workspace.
	session.Workspace = options.Workspace
	if err := session.SetCompatibilityWorkspaceScope(runtimeSubjectRoot()); err != nil {
		return nil, "", nil, fmt.Errorf("bind %s workspace scope: %w", options.Purpose, err)
	}
	coordinator, err := team.NewCoordinator(session, url, key, nil, nil, nil, team.RoleModels{Sidecar: model}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		return nil, "", nil, err
	}
	handle, err := preparePreflightSidecarContext(ctx, coordinator)
	if err != nil {
		_ = coordinator.Close()
		return nil, "", nil, err
	}
	release := func() {
		handle.Close()
		_ = coordinator.Close()
	}
	return sidecarTextGenerator{s: handle.Sidecar(), ctx: handle.Context(), purpose: options.Purpose, profile: options.Profile}, model, release, nil
}

// teamRegistryFromSearchPath resolves teams from a comma-separated search
// path, falling back to the default search paths.
func teamRegistryFromSearchPath(csv string) *team.TeamRegistry {
	paths := team.DefaultSearchPaths()
	if csv != "" {
		paths = strings.Split(csv, ",")
	}
	return team.NewTeamRegistry(paths)
}

// flushGovernanceEvents delivers pending promotion and memory-conflict
// lifecycle events from the context outbox to the workspace event store.
func flushGovernanceEvents(ctx context.Context, repo *contextstore.SQLiteRepository, workspace string) error {
	events, err := repo.PendingPromotionEvents(ctx)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	store, err := team.NewEventStore(workspace, "promotion", "")
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	for _, event := range events {
		if err = store.Append(team.RunEvent{Type: event.EventType, Actor: "operator", IdempotencyKey: event.IdempotencyKey, Payload: event.Payload}); err != nil {
			return err
		}
		if err = repo.MarkPromotionEventDelivered(ctx, event.IdempotencyKey); err != nil {
			return err
		}
	}
	return nil
}
