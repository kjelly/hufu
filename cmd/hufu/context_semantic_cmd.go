package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/embedding"
	"github.com/kjelly/hufu/internal/semantic"
)

type contextSemanticOptions struct {
	workspace string
	project   string
	team      string
	json      bool
	dryRun    bool
	strict    bool
	offline   bool
	warn      io.Writer
}

func init() {
	contextCmd.AddCommand(newContextSemanticCommand())
}

func newContextSemanticCommand() *cobra.Command {
	opts := &contextSemanticOptions{}
	cmd := &cobra.Command{
		Use:   "semantic",
		Short: "Inspect, build, and verify the semantic memory index",
		Long: "Operate the disposable semantic projection of canonical memory. Settings are the ones a run " +
			"of the team uses: the semantic-retrieval block of hufu.yaml merged with the team's own block. " +
			"The team is --team, else the only team with memories in the project.",
		PersistentPreRun: func(command *cobra.Command, _ []string) { opts.warn = command.ErrOrStderr() },
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show settings, the active generation, and per-item coverage (read-only, no network)",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runContextSemanticStatus(command.Context(), command.OutOrStdout(), opts)
		},
	}
	build := &cobra.Command{
		Use:   "build",
		Short: "Embed new or edited items and activate a new generation (never pulls a model)",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runContextSemanticBuild(command.Context(), command.OutOrStdout(), opts)
		},
	}
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Check the active generation's integrity, coverage, and encoder profile",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runContextSemanticVerify(command.Context(), command.OutOrStdout(), opts)
		},
	}
	for _, sub := range []*cobra.Command{status, build, verify} {
		sub.Flags().StringVarP(&opts.workspace, "workspace", "w", "", "Workspace directory containing context.sqlite (default: active managed workspace)")
		sub.Flags().StringVar(&opts.project, "project", "", "Canonical project ID (default: the only project in the store)")
		sub.Flags().StringVar(&opts.team, "team", "", "Team whose semantic-retrieval block overrides hufu.yaml (default: the only team in the project)")
		sub.Flags().BoolVar(&opts.json, "json", false, "Emit JSON")
	}
	build.Flags().BoolVar(&opts.dryRun, "dry-run", false, "Report what would change without contacting the encoder or writing")
	verify.Flags().BoolVar(&opts.strict, "strict", false, "Fail when any item lacks a vector (otherwise a warning)")
	verify.Flags().BoolVar(&opts.offline, "offline", false, "Skip the encoder profile check that contacts Ollama")
	cmd.AddCommand(status, build, verify)
	return cmd
}

// semanticReport is the content-free status/verify view.
type semanticReport struct {
	Workspace string `json:"workspace"`
	ProjectID string `json:"project_id"`
	// Team is the team whose block was considered; SettingsSource names the
	// layers the settings came from.
	Team            string                     `json:"team,omitempty"`
	SettingsSource  string                     `json:"settings_source"`
	Settings        semantic.Settings          `json:"settings"`
	InventoryPolicy string                     `json:"inventory_policy"`
	InventoryItems  int                        `json:"inventory_items"`
	Active          *semanticGenerationSummary `json:"active_generation,omitempty"`
	Covered         int                        `json:"covered"`
	Excluded        int                        `json:"excluded"`
	Uncovered       int                        `json:"uncovered"`
	Dropped         int                        `json:"dropped"`
	OtherStates     map[string]int             `json:"other_generations,omitempty"`
	ProfileCurrent  *bool                      `json:"profile_current,omitempty"`
	Problems        []string                   `json:"problems,omitempty"`
	Warnings        []string                   `json:"warnings,omitempty"`
}

type semanticGenerationSummary struct {
	GenerationID   string    `json:"generation_id"`
	IdentityScheme string    `json:"identity_scheme"`
	ModelID        string    `json:"model_id"`
	ArtifactDigest string    `json:"artifact_digest"`
	Profile        string    `json:"profile_fingerprint"`
	Dimensions     int       `json:"dimensions"`
	SourceRevision int64     `json:"source_revision"`
	Rows           int       `json:"rows"`
	Exclusions     int       `json:"exclusions"`
	ActivatedAt    time.Time `json:"activated_at,omitzero"`
}

func (o *contextSemanticOptions) workspacePath() (string, error) {
	if strings.TrimSpace(o.workspace) != "" {
		return o.workspace, nil
	}
	return requireResolvedWorkspace(getWorkspace())
}

// semanticStore opens the context store; read-only opens never migrate.
func semanticStore(workspace string, readOnly bool) (*contextstore.SQLiteRepository, error) {
	path := filepath.Join(workspace, "context.sqlite")
	if !readOnly {
		return contextstore.OpenSQLite(path)
	}
	repo, err := contextstore.OpenSQLiteReadOnly(path)
	if err != nil {
		return nil, err
	}
	store, ok := repo.(*contextstore.SQLiteRepository)
	if !ok {
		_ = repo.Close()
		return nil, errors.New("read-only context store does not expose semantic projections")
	}
	if store.SchemaVersion() < contextstore.SemanticSchemaVersion {
		_ = store.Close()
		return nil, fmt.Errorf("context store schema %d predates semantic inventory policies (%d); run `hufu context semantic build` or any run to migrate it", store.SchemaVersion(), contextstore.SemanticSchemaVersion)
	}
	return store, nil
}

func (o *contextSemanticOptions) resolveProject(ctx context.Context, store *contextstore.SQLiteRepository) (string, error) {
	if strings.TrimSpace(o.project) != "" {
		return o.project, nil
	}
	ids, err := store.ProjectIDs(ctx)
	if err != nil {
		return "", fmt.Errorf("list canonical projects: %w", err)
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("--project is required: the store holds %d projects", len(ids))
	}
	return ids[0], nil
}

// activeSemanticGeneration picks the newest active generation of the
// configured model under the retrievable policy, regardless of profile.
func activeSemanticGeneration(ctx context.Context, store *contextstore.SQLiteRepository, projectID, model string) (*contextstore.Generation, map[string]int, error) {
	generations, err := store.ListGenerations(ctx, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("list semantic generations: %w", err)
	}
	var active *contextstore.Generation
	others := map[string]int{}
	for i := range generations {
		generation := generations[i]
		if generation.InventoryPolicy != contextstore.InventoryPolicyRetrievable || generation.Model.ID != "ollama/"+model {
			continue
		}
		if generation.State != contextstore.GenerationActive {
			others[string(generation.State)]++
			continue
		}
		if active == nil || (generation.ActivatedAt != nil && active.ActivatedAt != nil && generation.ActivatedAt.After(*active.ActivatedAt)) {
			active = &generation
		}
	}
	return active, others, nil
}

func buildSemanticReport(ctx context.Context, store *contextstore.SQLiteRepository, workspace, projectID string, settings semantic.Settings, source semanticSource) (semanticReport, *contextstore.Generation, error) {
	report := semanticReport{
		Workspace: workspace, ProjectID: projectID, Team: source.team, SettingsSource: source.label,
		Settings: settings, InventoryPolicy: contextstore.InventoryPolicyRetrievable,
	}
	report.Settings.Endpoint = embedding.RedactEndpoint(settings.Endpoint)
	active, others, err := activeSemanticGeneration(ctx, store, projectID, settings.Model)
	if err != nil {
		return report, nil, err
	}
	if len(others) > 0 {
		report.OtherStates = others
	}
	plan, err := contextstore.PlanAgainstGeneration(ctx, store, projectID, contextstore.InventoryPolicyRetrievable, active)
	if err != nil {
		return report, nil, err
	}
	report.InventoryItems = len(plan.Inventory.Items)
	report.Covered, report.Excluded, report.Uncovered, report.Dropped = len(plan.Reuse), plan.Excluded, len(plan.Embed), plan.Dropped
	if active != nil {
		summary := &semanticGenerationSummary{
			GenerationID: active.GenerationID, IdentityScheme: active.Model.Scheme, ModelID: active.Model.ID,
			ArtifactDigest: active.Model.ManifestSHA256, Profile: active.Model.Revision, Dimensions: active.Model.Dimensions,
			SourceRevision: active.SourceRevision, Rows: active.RowCount, Exclusions: active.ExpectedCount - active.RowCount,
		}
		if active.ActivatedAt != nil {
			summary.ActivatedAt = *active.ActivatedAt
		}
		report.Active = summary
	}
	return report, active, nil
}

// semanticSource is where the settings of one command came from.
type semanticSource struct {
	team  string
	label string
}

// resolveSettings resolves the settings a run of the project's team uses:
// hufu.yaml merged with the team's semantic-retrieval block. The team is
// --team, which must be in the team search path, else the only team with
// memories in the project; an inferred team that is not found keeps
// hufu.yaml.
func (o *contextSemanticOptions) resolveSettings(ctx context.Context, store *contextstore.SQLiteRepository, projectID string) (semantic.Settings, semanticSource, error) {
	teamName := strings.TrimSpace(o.team)
	explicit := teamName != ""
	if !explicit {
		ids, err := store.TeamIDs(ctx, projectID)
		if err != nil {
			return semantic.Settings{}, semanticSource{}, fmt.Errorf("list canonical teams: %w", err)
		}
		if len(ids) == 1 {
			teamName = ids[0]
		}
	}
	warn := o.warn
	if warn == nil {
		warn = io.Discard
	}
	settings, found, overridden, err := semanticSettingsForTeam(teamName, warn)
	if err != nil {
		return semantic.Settings{}, semanticSource{}, err
	}
	switch {
	case explicit && !found:
		return semantic.Settings{}, semanticSource{}, fmt.Errorf("team %q is not in the team search path", teamName)
	case !found:
		return settings, semanticSource{label: "hufu.yaml"}, nil
	case overridden:
		return settings, semanticSource{team: teamName, label: "hufu.yaml + team " + teamName}, nil
	default:
		return settings, semanticSource{team: teamName, label: "hufu.yaml; team " + teamName + " has no override"}, nil
	}
}

func runContextSemanticStatus(ctx context.Context, out io.Writer, opts *contextSemanticOptions) error {
	report, _, err := loadSemanticReport(ctx, opts)
	if err != nil {
		return err
	}
	return writeSemanticReport(out, report, opts.json)
}

func loadSemanticReport(ctx context.Context, opts *contextSemanticOptions) (semanticReport, *contextstore.Generation, error) {
	workspace, err := opts.workspacePath()
	if err != nil {
		return semanticReport{}, nil, err
	}
	store, err := semanticStore(workspace, true)
	if err != nil {
		return semanticReport{}, nil, err
	}
	defer func() { _ = store.Close() }()
	projectID, err := opts.resolveProject(ctx, store)
	if err != nil {
		return semanticReport{}, nil, err
	}
	settings, source, err := opts.resolveSettings(ctx, store, projectID)
	if err != nil {
		return semanticReport{}, nil, err
	}
	return buildSemanticReport(ctx, store, workspace, projectID, settings, source)
}

func runContextSemanticBuild(ctx context.Context, out io.Writer, opts *contextSemanticOptions) error {
	if opts.dryRun {
		report, _, err := loadSemanticReport(ctx, opts)
		if err != nil {
			return err
		}
		if opts.json {
			return writeSemanticJSON(out, map[string]any{"dry_run": true, "inventory_items": report.InventoryItems, "reuse": report.Covered, "embed": report.Uncovered, "drop": report.Dropped,
				"note": "assumes the encoder profile of the active generation; the encoder was not contacted"})
		}
		_, err = fmt.Fprintf(out, "Dry run (encoder not contacted, nothing written): %d inventory items; would reuse %d, embed %d, drop %d.\nA changed model digest or profile would re-embed every item.\n",
			report.InventoryItems, report.Covered, report.Uncovered, report.Dropped)
		return err
	}
	workspace, err := opts.workspacePath()
	if err != nil {
		return err
	}
	store, err := semanticStore(workspace, false)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	projectID, err := opts.resolveProject(ctx, store)
	if err != nil {
		return err
	}
	settings, _, err := opts.resolveSettings(ctx, store, projectID)
	if err != nil {
		return err
	}
	embedder, err := openSemanticEncoder(ctx, settings, true)
	if err != nil {
		return err
	}
	builder, err := contextstore.NewSemanticBuilder(store, embedder, contextstore.SemanticBuilderOptions{
		InventoryPolicy: contextstore.InventoryPolicyRetrievable, BatchSize: settings.IndexBatchSize,
	})
	if err != nil {
		return err
	}
	buildCtx, cancel := context.WithTimeout(ctx, settings.IndexTimeout)
	defer cancel()
	started := time.Now()
	result, err := builder.Refresh(buildCtx, projectID)
	if err != nil {
		return fmt.Errorf("build semantic generation: %w", err)
	}
	if opts.json {
		return writeSemanticJSON(out, result)
	}
	switch {
	case result.Unchanged:
		_, err = fmt.Fprintf(out, "Semantic index is current: generation %s covers %d items.\n", result.GenerationID, result.Inventory)
	case !result.Activated:
		_, err = fmt.Fprintf(out, "A newer generation %s won; nothing activated.\n", result.GenerationID)
	default:
		_, err = fmt.Fprintf(out, "Activated generation %s in %s: %d items (reused %d, embedded %d, excluded %d).\n",
			result.GenerationID, time.Since(started).Round(time.Millisecond), result.Inventory, result.Reused, result.Embedded, result.Excluded)
	}
	return err
}

// openSemanticEncoder pins the profile from Ollama. preload loads the model
// for a build; verify only reads /api/tags and /api/show.
func openSemanticEncoder(ctx context.Context, settings semantic.Settings, preload bool) (*embedding.OllamaEmbedder, error) {
	client, err := embedding.NewOllamaClient(settings.Endpoint, nil)
	if err != nil {
		return nil, err
	}
	loadCtx, cancel := context.WithTimeout(ctx, settings.LoadTimeout)
	defer cancel()
	profile, query, err := embedding.ResolveProfile(loadCtx, client, embedding.ProfileSettings{
		Model: settings.Model, MaxInputTokens: settings.MaxInputTokens, QueryMaxTokens: settings.QueryMaxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("resolve encoder %s at %s: %w", settings.Model, embedding.RedactEndpoint(settings.Endpoint), err)
	}
	encoder, err := embedding.NewOllamaEmbedder(client, profile, query, settings.KeepAlive)
	if err != nil {
		return nil, err
	}
	if preload {
		if err = encoder.Preload(loadCtx); err != nil {
			return nil, fmt.Errorf("load encoder %s: %w", settings.Model, err)
		}
	}
	return encoder, nil
}

func runContextSemanticVerify(ctx context.Context, out io.Writer, opts *contextSemanticOptions) error {
	workspace, err := opts.workspacePath()
	if err != nil {
		return err
	}
	store, err := semanticStore(workspace, true)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	projectID, err := opts.resolveProject(ctx, store)
	if err != nil {
		return err
	}
	settings, source, err := opts.resolveSettings(ctx, store, projectID)
	if err != nil {
		return err
	}
	report, active, err := buildSemanticReport(ctx, store, workspace, projectID, settings, source)
	if err != nil {
		return err
	}
	if active == nil {
		report.Problems = append(report.Problems, "no active generation for "+settings.Model+"; run `hufu context semantic build`")
	} else {
		if verifyErr := contextstore.VerifyActiveGeneration(ctx, store, *active); verifyErr != nil {
			report.Problems = append(report.Problems, "active generation failed integrity checks: "+verifyErr.Error())
		}
		if !opts.offline {
			encoder, encoderErr := openSemanticEncoder(ctx, settings, false)
			if encoderErr != nil {
				report.Problems = append(report.Problems, encoderErr.Error())
			} else {
				current := encoder.Identity().Revision == active.Model.Revision && encoder.Identity().ManifestSHA256 == active.Model.ManifestSHA256
				report.ProfileCurrent = &current
				if !current {
					report.Problems = append(report.Problems, "active generation was built with a different encoder profile or model digest; run `hufu context semantic build`")
				}
			}
		}
	}
	if report.Uncovered > 0 || report.Dropped > 0 {
		message := fmt.Sprintf("%d items lack vectors and %d stale entries remain; a refresh will add them", report.Uncovered, report.Dropped)
		if opts.strict {
			report.Problems = append(report.Problems, message)
		} else {
			report.Warnings = append(report.Warnings, message)
		}
	}
	if err = writeSemanticReport(out, report, opts.json); err != nil {
		return err
	}
	if len(report.Problems) > 0 {
		return fmt.Errorf("semantic verification found %d problem(s)", len(report.Problems))
	}
	return nil
}

func writeSemanticJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func writeSemanticReport(out io.Writer, report semanticReport, asJSON bool) error {
	if asJSON {
		return writeSemanticJSON(out, report)
	}
	var b strings.Builder
	s := report.Settings
	fmt.Fprintf(&b, "Semantic retrieval (%s): mode=%s model=%s endpoint=%s\n", report.SettingsSource, s.Mode, s.Model, s.Endpoint)
	fmt.Fprintf(&b, "  query budget %d tokens, input limit %d tokens, query timeout %s, load timeout %s\n", s.QueryMaxTokens, s.MaxInputTokens, s.QueryTimeout, s.LoadTimeout)
	fmt.Fprintf(&b, "Project %s, inventory %s: %d items\n", report.ProjectID, report.InventoryPolicy, report.InventoryItems)
	if a := report.Active; a != nil {
		fmt.Fprintf(&b, "Active generation %s (%s, %s, digest %.12s, profile %.12s, %d dims)\n", a.GenerationID, a.IdentityScheme, a.ModelID, a.ArtifactDigest, a.Profile, a.Dimensions)
		fmt.Fprintf(&b, "  %d vectors, %d exclusions, built from canonical revision %d\n", a.Rows, a.Exclusions, a.SourceRevision)
	} else {
		b.WriteString("Active generation: none\n")
	}
	fmt.Fprintf(&b, "Coverage: %d covered (%d excluded), %d uncovered, %d stale\n", report.Covered, report.Excluded, report.Uncovered, report.Dropped)
	if report.ProfileCurrent != nil {
		fmt.Fprintf(&b, "Encoder profile current: %t\n", *report.ProfileCurrent)
	}
	for state, count := range report.OtherStates {
		fmt.Fprintf(&b, "Other generations: %d %s\n", count, state)
	}
	for _, warning := range report.Warnings {
		fmt.Fprintf(&b, "warning: %s\n", warning)
	}
	for _, problem := range report.Problems {
		fmt.Fprintf(&b, "problem: %s\n", problem)
	}
	_, err := io.WriteString(out, b.String())
	return err
}
