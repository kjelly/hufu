package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

type derivedRunEntryKey struct{}
type derivedRunEntry struct {
	segments []team.PromptSegment
	registry *team.TeamRegistry
	prompt   string
}

func derivedRunContext(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry) context.Context {
	if !opts.canonicalRun {
		return ctx
	}
	return context.WithValue(ctx, derivedRunEntryKey{}, &derivedRunEntry{segments: slices.Clone(segments), registry: registry})
}

// Runs before workspace recovery, lifecycle archival and provider startup.
func validateDerivedRunEntry(ctx context.Context, session *team.TeamSession, startsFresh bool) error {
	ref, err := team.ReadDerivedForkState(ctx, session.Workspace)
	if err != nil {
		return err
	}
	if ref == nil || !ref.AwaitingNewRequest {
		return nil
	}
	entry, _ := ctx.Value(derivedRunEntryKey{}).(*derivedRunEntry)
	if entry == nil || startsFresh || opts.intent != "execute" || opts.dryRun || opts.tempWorkspace || opts.autoTeam ||
		opts.defaultTeam || opts.resumeDecision != "" || !strings.EqualFold(opts.agentTeamName, ref.Team) || opts.routeMode == "fast" {
		return fmt.Errorf("derived branch is awaiting_new_request; use run --team %s with one new execute request and the coordinator route", ref.Team)
	}
	expanded, err := expandSegmentsWithAgents(entry.segments, map[string]*teamContext{
		opts.agentTeamName: {session: session},
	}, entry.registry)
	if err != nil {
		return err
	}
	if len(expanded) != 1 || expanded[0].Type != team.SegmentSwitchTeam || expanded[0].IsPiped ||
		!strings.EqualFold(expanded[0].Name, ref.Team) || strings.TrimSpace(expanded[0].Content) == "" {
		return fmt.Errorf("derived branch requires one nonempty request for its original team")
	}
	entry.prompt = expanded[0].Content
	opts.routeMode = "team"
	return nil
}

func validateDerivedCoordinatorStartup(ctx context.Context, coordinator *team.Coordinator) error {
	if !coordinator.AwaitingDerivedForkRequest() {
		return nil
	}
	entry, _ := ctx.Value(derivedRunEntryKey{}).(*derivedRunEntry)
	if entry == nil || entry.prompt == "" {
		return fmt.Errorf("derived request has no canonical execute entry")
	}
	return coordinator.ValidateDerivedForkRequest(ctx, entry.prompt)
}
