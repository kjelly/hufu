package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/team"
)

func archiveCurrentSessionToMemory(ctx context.Context, tc *teamContext) {
	if tc == nil || tc.sessionData == nil || len(tc.sessionData.Entries) == 0 {
		fmt.Fprintf(os.Stderr, "%s No session data to archive.\n", dimStyle.Render("○"))
		return
	}

	resolvedURL := config.ResolveProviderURL(opts.providerURL, "", "")
	ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedURL)
	embedModel := config.ResolveEmbeddingModel(opts.memoryModel)
	projectDir, _ := os.Getwd()

	memStore, err := memory.NewMemoryStore(projectDir, ollamaAPIURL, embedModel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s Memory unavailable for archive: %v\n", errStyle.Render("⚠"), err)
		return
	}
	defer func() { _ = memStore.Close() }()

	var entries []memory.SessionSummaryEntry
	for _, e := range tc.sessionData.Entries {
		entries = append(entries, memory.SessionSummaryEntry{
			Role:      e.Role,
			Content:   e.Content,
			Timestamp: e.Timestamp,
		})
	}

	var summarizeFn memory.SummarizeFunc
	if s := tc.coordinator.Sidecar(); s != nil {
		summarizeFn = s.Summarize
	}
	if err := memory.ArchiveSessionSummary(ctx, memStore, entries, tc.session.Config.Name, summarizeFn); err != nil {
		fmt.Fprintf(os.Stderr, "%s Failed to archive session to memory: %v\n", errStyle.Render("⚠"), err)
		return
	}
	fmt.Fprintf(os.Stderr, "%s Session archived to memory.\n", doneStyle.Render("✓"))
}

func runArchiveMemory(ctx context.Context, registry *team.TeamRegistry, vars map[string]string) error {
	teams := registry.ListTeams()
	if len(teams) == 0 {
		return fmt.Errorf("no teams available")
	}

	var teamNames []string
	if opts.agentTeamName != "" {
		teamNames = []string{strings.ToLower(opts.agentTeamName)}
	} else {
		teamNames = teams
	}

	archived := 0
	for _, name := range teamNames {
		tc, err := loadTeamByName(ctx, name, registry, opts.providerURL, opts.providerAPIKey, newPathConsent(), vars, nil, false, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s Failed to load team %q: %v\n", errStyle.Render("⚠"), name, err)
			continue
		}
		if tc.sessionData != nil && len(tc.sessionData.Entries) > 0 {
			archiveCurrentSessionToMemory(ctx, tc)
			archived++
		} else {
			fmt.Fprintf(os.Stderr, "%s No session data for team %q\n", dimStyle.Render("○"), name)
		}
	}

	if archived == 0 {
		fmt.Fprintf(os.Stderr, "%s No session data found to archive.\n", dimStyle.Render("○"))
	}
	return nil
}

func savePromptToHistory(ctx context.Context, prompt string, defaultProviderURL string) {
	resolvedProviderURL := config.ResolveProviderURL(defaultProviderURL, "", "")
	ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedProviderURL)
	embedModel := config.ResolveEmbeddingModel("")

	store, err := memory.NewGlobalMemoryStore(ollamaAPIURL, embedModel)
	if err != nil {
		return
	}

	id := fmt.Sprintf("hist_%d", time.Now().UnixNano())
	metadata := map[string]string{
		"type":      "prompt_history",
		"timestamp": time.Now().Format(time.RFC3339),
	}
	saveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = store.Save(saveCtx, id, prompt, metadata)
}

var historyCmd = &cobra.Command{
	Use:   "history [query]",
	Short: "Search semantic history of prompts",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		resolvedProviderURL := config.ResolveProviderURL(opts.providerURL, "", "")
		ollamaAPIURL := config.ProviderURLToOllamaAPI(resolvedProviderURL)
		embedModel := config.ResolveEmbeddingModel(opts.memoryModel)

		store, err := memory.NewGlobalMemoryStore(ollamaAPIURL, embedModel)
		if err != nil {
			return fmt.Errorf("failed to open global memory store: %w", err)
		}

		query := ""
		if len(args) > 0 {
			query = args[0]
		}

		if query == "" {
			fmt.Println("Please provide a query to search prompt history. Example: hufu history \"write python script\"")
			return nil
		}

		results, err := store.Query(ctx, query, 5, map[string]string{"type": "prompt_history"})
		if err != nil {
			return fmt.Errorf("semantic search failed: %w", err)
		}

		if len(results) == 0 {
			fmt.Println("No matching prompt history found.")
			return nil
		}

		fmt.Printf("\n%s Semantic History Search Results (query: %q):\n\n", boldStyle.Render("🗂️"), query)
		for i, res := range results {
			ts := res.Metadata["timestamp"]
			fmt.Printf("  %d. %s %s\n", i+1, doneStyle.Render("→"), boldStyle.Render(res.Content))
			if ts != "" {
				fmt.Printf("     %s\n", dimStyle.Render("Time: "+ts))
			}
			fmt.Println()
		}
		return nil
	},
}
