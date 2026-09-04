package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/modelcatalog"
)

var (
	modelsCachePath   string
	modelsJSON        bool
	modelsUpdateNoNet bool
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
	Short: "Inspect one catalog model without network access",
	Args:  cobra.ExactArgs(1),
	RunE:  runModelsInspect,
}

var newModelCatalogStore = func() *modelcatalog.Store {
	return modelcatalog.NewStore(modelsCachePath, modelcatalog.StoreOptions{NoNet: opts.noNet || modelsUpdateNoNet})
}

func init() {
	modelsCmd.AddCommand(modelsUpdateCmd, modelsInspectCmd)
	modelsCmd.PersistentFlags().StringVar(&modelsCachePath, "cache", "", "Catalog cache path (default: user cache directory)")
	modelsCmd.PersistentFlags().BoolVar(&modelsJSON, "json", false, "Write machine-readable output")
	modelsUpdateCmd.Flags().BoolVar(&modelsUpdateNoNet, "no-net", false, "Refuse network access")
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
	if modelsJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
			Origin string                     `json:"origin"`
			Path   string                     `json:"cache_path"`
			Found  bool                       `json:"found"`
			Model  *modelcatalog.CatalogModel `json:"model,omitempty"`
		}{Origin: origin, Path: store.Path, Found: found, Model: catalogModelPointer(result, found)})
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
		return write("Model:   %s/%s (unknown)\n", provider, modelID)
	}
	model := result.Model
	for _, line := range []struct {
		format string
		values []any
	}{
		{format: "Model:   %s/%s\n", values: []any{model.Provider, model.ID}},
		{format: "Family:  %s\n", values: []any{model.Family}},
		{format: "Context: %d\n", values: []any{model.Context}},
		{format: "Output:  %d\n", values: []any{model.Output}},
		{format: "Tools:   %s\n", values: []any{optionalBoolText(model.ToolCall)}},
		{format: "Vision:  %s\n", values: []any{optionalBoolText(model.Attachment)}},
		{format: "Reasoning: %s\n", values: []any{optionalBoolText(model.Reasoning)}},
		{format: "Temperature: %s\n", values: []any{optionalBoolText(model.Temperature)}},
	} {
		if err := write(line.format, line.values...); err != nil {
			return err
		}
	}
	return nil
}

func catalogModelPointer(result modelcatalog.LookupResult, found bool) *modelcatalog.CatalogModel {
	if !found {
		return nil
	}
	model := result.Model
	return &model
}

func optionalBoolText(value *bool) string {
	if value == nil {
		return "unknown"
	}
	if *value {
		return "yes"
	}
	return "no"
}
