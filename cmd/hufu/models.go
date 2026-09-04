package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/modelcatalog"
	"github.com/kjelly/hufu/internal/modelprofile"
	"github.com/kjelly/hufu/internal/team"
)

var (
	modelsCachePath    string
	modelsJSON         bool
	modelsUpdateNoNet  bool
	modelsInspectNoNet bool
)

var modelsCmd = &cobra.Command{
	Use:   "models",
	Short: "Inspect and update the offline model catalog",
}

var modelsUpdateCmd = &cobra.Command{
	Use:   "update",
	Short: "Download and atomically update the models.dev catalog",
	Args:  cobra.NoArgs,
	RunE:  runModelsUpdate,
}

var modelsInspectCmd = &cobra.Command{
	Use:   "inspect <provider>/<model>",
	Short: "Inspect one model's effective runtime profile",
	Args:  cobra.ExactArgs(1),
	RunE:  runModelsInspect,
}

var newModelCatalogStore = func() *modelcatalog.Store {
	return modelcatalog.NewStore(modelsCachePath, modelcatalog.StoreOptions{NoNet: opts.noNet || modelsUpdateNoNet})
}

var newModelProfileRuntime = func(manager *agent.ProviderManager, noNet bool, catalog modelcatalog.Reader) *team.ModelProfileRuntime {
	return team.NewModelProfileRuntimeWithCatalog(manager, noNet, catalog)
}

func init() {
	modelsCmd.AddCommand(modelsUpdateCmd, modelsInspectCmd)
	modelsCmd.PersistentFlags().StringVar(&modelsCachePath, "cache", "", "Catalog cache path (default: user cache directory)")
	modelsCmd.PersistentFlags().BoolVar(&modelsJSON, "json", false, "Write machine-readable output")
	modelsUpdateCmd.Flags().BoolVar(&modelsUpdateNoNet, "no-net", false, "Refuse network access")
	modelsInspectCmd.Flags().BoolVar(&modelsInspectNoNet, "no-net", false, "Skip all provider runtime requests")
}

func runModelsUpdate(cmd *cobra.Command, _ []string) error {
	store := newModelCatalogStore()
	catalog, err := store.Update(cmd.Context())
	if err != nil {
		return err
	}
	if modelsJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Path    string `json:"path"`
			Version string `json:"version"`
			Models  int    `json:"models"`
		}{Path: store.Path, Version: catalog.Version, Models: len(catalog.Models)})
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "updated model catalog: %s (%d models)\n", store.Path, len(catalog.Models))
	return err
}

func runModelsInspect(cmd *cobra.Command, args []string) error {
	provider, modelID, ok := strings.Cut(args[0], "/")
	if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(modelID) == "" {
		return fmt.Errorf("model must be specified as <provider>/<model>")
	}
	store := newModelCatalogStore()
	catalog, origin, err := store.LoadWithOrigin()
	if err != nil {
		return err
	}
	result, found := catalog.Lookup(provider, modelID)
	providerURL := config.ResolveProviderURL(opts.providerURL, "", "")
	providerAPIKey := config.ResolveProviderAPIKey(opts.providerAPIKey, "")
	cfg := config.LoadConfig()
	manager, err := agent.NewProviderManager(providerURL, providerAPIKey, cfg.Providers)
	if err != nil {
		return err
	}
	noRuntime := opts.noNet || modelsInspectNoNet
	runtime := newModelProfileRuntime(manager, noRuntime, catalog)
	profile, profileErr := runtime.Diagnostic(cmd.Context(), provider, modelID, 0, 0, noRuntime)
	if profileErr != nil && profile.ModelID == "" {
		return profileErr
	}
	if modelsJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Origin       string                           `json:"origin"`
			Path         string                           `json:"cache_path"`
			Found        bool                             `json:"found"`
			Model        *modelcatalog.CatalogModel       `json:"model,omitempty"`
			Profile      modelprofile.TelemetryProjection `json:"profile"`
			RuntimeError string                           `json:"runtime_error,omitempty"`
		}{Origin: origin, Path: store.Path, Found: found, Model: catalogModelPointer(result, found), Profile: modelprofile.TelemetryFromProfile(profile), RuntimeError: safeProfileError(profileErr)})
	}
	output := cmd.OutOrStdout()
	write := func(format string, values ...any) error {
		_, writeErr := fmt.Fprintf(output, format, values...)
		return writeErr
	}
	if err := write("Catalog: %s\n", origin); err != nil {
		return err
	}
	if err := write("Cache:   %s\n", store.Path); err != nil {
		return err
	}
	if !found {
		if err := write("Catalog model: unknown\n"); err != nil {
			return err
		}
	}
	return writeProfileText(write, profile, profileErr)
}

func writeProfileText(write func(string, ...any) error, profile modelprofile.ModelProfile, profileErr error) error {
	projection := modelprofile.TelemetryFromProfile(profile)
	for _, line := range []struct {
		format string
		values []any
	}{
		{format: "Model:   %s/%s\n", values: []any{projection.Provider, projection.ModelID}},
		{format: "Family:  %s\n", values: []any{projection.Family}},
		{format: "Estimator: %s (%s)\n", values: []any{projection.Estimator.Value, projection.Estimator.Provenance}},
		{format: "Operator context: %d (%s)\n", values: []any{projection.Operator.Value, projection.Operator.Source}},
		{format: "Catalog context: %d (%s)\n", values: []any{projection.Catalog.Value, projection.Catalog.Source}},
		{format: "Configured context: %d (%s)\n", values: []any{projection.Configured.Value, projection.Configured.Source}},
		{format: "Runtime context: %d (%s)\n", values: []any{projection.Runtime.Value, projection.Runtime.Source}},
		{format: "Effective context: %d (%s)\n", values: []any{projection.Effective.Value, projection.Effective.Source}},
		{format: "Max output: %d (%s)\n", values: []any{projection.MaxOutput.Value, projection.MaxOutput.Source}},
		{format: "Tools:   %s\n", values: []any{projection.Capabilities["tools"].State}},
		{format: "Vision:  %s\n", values: []any{projection.Capabilities["attachments"].State}},
		{format: "Reasoning: %s\n", values: []any{projection.Capabilities["reasoning"].State}},
		{format: "Temperature: %s\n", values: []any{projection.Capabilities["temperature"].State}},
	} {
		if err := write(line.format, line.values...); err != nil {
			return err
		}
	}
	if profileErr != nil {
		if err := write("Runtime: unavailable (%s)\n", safeProfileError(profileErr)); err != nil {
			return err
		}
	}
	return nil
}

func safeProfileError(err error) string {
	if err == nil {
		return ""
	}
	// Provider adapter errors are intentionally not projected: they may carry
	// endpoint details. Diagnostics expose only the bounded status label.
	return "unavailable"
}

func catalogModelPointer(result modelcatalog.LookupResult, found bool) *modelcatalog.CatalogModel {
	if !found {
		return nil
	}
	model := result.Model
	return &model
}
