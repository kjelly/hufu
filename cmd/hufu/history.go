package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/memory"
	"github.com/kjelly/hufu/internal/team"
)

func archiveCurrentSessionToMemory(ctx context.Context, tc *teamContext) {
	if tc == nil || tc.sessionData == nil || len(tc.sessionData.Entries) == 0 {
		fmt.Fprintf(os.Stderr, "%s No session data to archive.\n", dimStyle.Render("○"))
		return
	}

	var entries []memory.SessionSummaryEntry
	for _, e := range tc.sessionData.Entries {
		entries = append(entries, memory.SessionSummaryEntry{Role: e.Role, Content: e.Content, Timestamp: e.Timestamp})
	}
	if handled, err := tc.coordinator.ArchiveSessionSummary(ctx, entries); handled {
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s Failed to archive session to canonical context: %v\n", errStyle.Render("⚠"), err)
			return
		}
		fmt.Fprintf(os.Stderr, "%s Session archived to canonical context.\n", doneStyle.Render("✓"))
		return
	}

	// Coordinators require the canonical repository. Keep this guard explicit
	// rather than recreating the retired chromem write path if a nonstandard
	// caller supplied a coordinator without one.
	fmt.Fprintf(os.Stderr, "%s Session archive unavailable: canonical context is not configured.\n", errStyle.Render("⚠"))
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
	_ = defaultProviderURL // retained for call-site compatibility during migration.
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return
	}
	repo, err := contextstore.OpenSQLite(filepath.Join(getWorkspace(), "history-context.sqlite"))
	if err != nil {
		return
	}
	defer func() { _ = repo.Close() }()
	item := contextstore.ContextItem{
		ID: fmt.Sprintf("hist_%d", time.Now().UnixNano()), Kind: contextstore.ContextSummary, Content: prompt,
		Scope: contextstore.Scope{ProjectID: "__global_prompt_history__"}, Authority: contextstore.AuthorityUser,
		TrustLevel: contextstore.TrustInternal, Priority: contextstore.PriorityBackground, Confidence: 1.0,
		Source: contextstore.SourceRef{Type: "prompt_history"}, Metadata: map[string]string{"type": "prompt_history"},
	}
	saveCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = repo.Append(saveCtx, item)
}

var historyCmd = &cobra.Command{
	Use:   "history [query]",
	Short: "Search semantic history of prompts",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		repo, err := contextstore.OpenSQLite(filepath.Join(getWorkspace(), "history-context.sqlite"))
		if err != nil {
			return fmt.Errorf("failed to open canonical prompt history: %w", err)
		}
		defer func() { _ = repo.Close() }()

		query := ""
		if len(args) > 0 {
			query = args[0]
		}

		if query == "" {
			fmt.Println("Please provide a query to search prompt history. Example: hufu history \"write python script\"")
			return nil
		}

		results, _, err := contextstore.HybridRetrieve(ctx, repo, nil, contextstore.SearchRequest{Query: query, Scope: contextstore.Scope{ProjectID: "__global_prompt_history__"}, Limit: 5})
		if err != nil {
			return fmt.Errorf("semantic search failed: %w", err)
		}

		if len(results) == 0 {
			fmt.Println("No matching prompt history found.")
			return nil
		}

		fmt.Printf("\n%s Semantic History Search Results (query: %q):\n\n", boldStyle.Render("🗂️"), query)
		for i, res := range results {
			fmt.Printf("  %d. %s %s\n", i+1, doneStyle.Render("→"), boldStyle.Render(res.Item.Content))
			if !res.Item.CreatedAt.IsZero() {
				fmt.Printf("     %s\n", dimStyle.Render("Time: "+res.Item.CreatedAt.Format(time.RFC3339)))
			}
			fmt.Println()
		}
		return nil
	},
}
