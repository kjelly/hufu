package inspect

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// historicalLineage budgets retained content after exact branch projection.
// Headers and team identities remain global, and both passes validate the full
// journal. A single immutable snapshot prevents mixed reads during appends or
// branch checkout between the header and content passes.
func historicalLineage(ctx context.Context, scope HistoricalResultScope, maxEvents, maxPayloadBytes int) (Lineage, *team.SessionTree, map[string]bool, error) {
	if strings.TrimSpace(scope.Workspace) == "" {
		return Lineage{}, nil, nil, fmt.Errorf("%w: workspace is required after CLI resolution", ErrInvalidQuery)
	}
	snapshot, err := team.OpenRunEventSnapshot(ctx, scope.Workspace)
	if err != nil {
		return Lineage{}, nil, nil, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	defer func() { _ = snapshot.Close() }()
	tree, err := snapshot.SessionTree(ctx)
	if err != nil {
		return Lineage{}, nil, nil, fmt.Errorf("%w: load session tree: %v", ErrIntegrity, err)
	}
	var headers []team.RunEvent
	knownTeams := make(map[string]bool)
	err = snapshot.Visit(ctx, func(event team.RunEvent) error {
		if maxEvents > 0 && len(headers) >= maxEvents {
			return historicalReadBudgetError()
		}
		if event.Type == string(team.EventRunStarted) || strings.HasPrefix(event.Type, "task_") {
			var metadata struct {
				Team string `json:"team"`
			}
			if json.Unmarshal(event.Payload, &metadata) == nil && strings.TrimSpace(metadata.Team) != "" {
				knownTeams[strings.ToLower(strings.TrimSpace(metadata.Team))] = true
			}
		}
		event.Payload = nil
		headers = append(headers, event)
		return nil
	})
	if err != nil {
		return Lineage{}, nil, nil, historicalStreamError(err)
	}
	branchID, err := resolveBranch(tree, scope.Branch)
	if err != nil {
		return Lineage{}, nil, nil, err
	}
	// This pass selects headers only, so payload-dependent lineage checks
	// cannot run yet. The full checked projector below remains mandatory
	// after the exact lineage's content has been loaded within its budget.
	selected := make(map[string]bool)
	for _, event := range team.FilterEventsForBranch(headers, tree, branchID) {
		selected[event.ID] = true
	}
	ordinal, payloadBytes := 0, 0
	err = snapshot.Visit(ctx, func(event team.RunEvent) error {
		index := ordinal
		ordinal++
		if !scope.AllBranches && !selected[event.ID] {
			return nil
		}
		if maxPayloadBytes > 0 && len(event.Payload) > maxPayloadBytes-payloadBytes {
			return historicalReadBudgetError()
		}
		payloadBytes += len(event.Payload)
		headers[index] = event
		return nil
	})
	if err != nil {
		return Lineage{}, nil, nil, historicalStreamError(err)
	}
	lineage, err := indexLineage(headers, tree, scope.Branch)
	return lineage, tree, knownTeams, err
}

func historicalReadBudgetError() error {
	return fmt.Errorf("%w: historical event replay exceeds the read budget; no partial results were returned", ErrInvalidQuery)
}

func historicalStreamError(err error) error {
	if errors.Is(err, ErrInvalidQuery) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrIntegrity, err)
}
