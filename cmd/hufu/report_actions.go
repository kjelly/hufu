package main

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// writeCatalogActionReport lists the run's catalog action tasks. Arguments
// appear only as their hash; the report never prints them. It writes nothing
// for a run without catalog actions.
func writeCatalogActionReport(b *strings.Builder, todos []*team.TodoItem) {
	var catalog []*team.TodoItem
	for _, item := range todos {
		if item != nil && item.CatalogAction != nil {
			catalog = append(catalog, item)
		}
	}
	if len(catalog) == 0 {
		return
	}
	b.WriteString("### Catalog Actions\n\n")
	b.WriteString("| Todo | Action | Status | Arguments hash | Proposals |\n")
	b.WriteString("|------|--------|--------|----------------|-----------|\n")
	for _, item := range catalog {
		proposals := strings.Join(item.CatalogAction.ProposalIDs, ", ")
		if proposals == "" {
			proposals = "none"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
			reportTableValue(item.ID, 40), reportTableValue(item.CatalogAction.ActionID, 64), reportTableValue(string(item.Status), 32),
			reportTableValue(item.CatalogAction.ArgumentsHash, 80), reportTableValue(proposals, 480))
	}
	b.WriteString("\n")
}

// catalogStepLabel describes a catalog task in the --steps confirmation
// prompt by action ID and a short arguments hash.
func catalogStepLabel(binding *team.CatalogActionBinding) string {
	hash := strings.TrimPrefix(binding.ArgumentsHash, "sha256:")
	if len(hash) > 12 {
		hash = hash[:12]
	}
	return fmt.Sprintf("catalog action %s args=%s", binding.ActionID, hash)
}
