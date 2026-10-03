package team

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

// inheritedObservationKeySeparator joins a source observation's idempotency
// key to the record that inherits it, so each copy is applied exactly once.
const inheritedObservationKeySeparator = "\x1finherited\x1f"

// InheritExperience returns observations plus, for every lineage link, a copy
// of each observation of the link's source re-keyed to its target. A
// persistent record promoted from a session record thereby carries the
// exposures, uses, and verified outcomes that record earned, under the same
// task occurrences, so its independent-task count is not inflated. Copies are
// made from the input observations only, so a copy is never copied again.
func InheritExperience(observations []contextstore.ExperienceObservation, lineage []contextstore.ExperienceLineage) []contextstore.ExperienceObservation {
	if len(lineage) == 0 {
		return observations
	}
	targets := experienceLineageTargets(lineage)
	out := append(make([]contextstore.ExperienceObservation, 0, len(observations)), observations...)
	for _, observation := range observations {
		for _, target := range targets[observation.ContextItemID] {
			out = append(out, inheritedObservation(observation, target))
		}
	}
	return out
}

// ReplayExperienceObservations converts the memory ledger to reducer inputs,
// including the evidence promoted records inherit. Rebuilds and consistency
// checks use it so their result matches the live projection.
func ReplayExperienceObservations(ctx context.Context, events []RunEvent, policy agent.MemoryLearningPolicy, repo interface {
	ListExperienceLineage(context.Context, string) ([]contextstore.ExperienceLineage, error)
}) ([]contextstore.ExperienceObservation, error) {
	observations := ExperienceObservationsFromEvents(events, policy)
	if repo == nil {
		return observations, nil
	}
	lineage, err := repo.ListExperienceLineage(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("load experience lineage: %w", err)
	}
	return InheritExperience(observations, lineage), nil
}

func inheritedObservation(observation contextstore.ExperienceObservation, target string) contextstore.ExperienceObservation {
	observation.IdempotencyKey += inheritedObservationKeySeparator + target
	observation.ContextItemID = target
	return observation
}

func experienceLineageTargets(lineage []contextstore.ExperienceLineage) map[string][]string {
	targets := make(map[string][]string, len(lineage))
	for _, link := range lineage {
		targets[link.SourceID] = append(targets[link.SourceID], link.TargetID)
	}
	return targets
}

type experienceLineageLister interface {
	ListExperienceLineage(context.Context, string) ([]contextstore.ExperienceLineage, error)
}

// experienceLineageCache maps a source record to the persistent records that
// inherit its evidence. It is loaded on first use and reloaded when a run's
// candidates are confirmed, the only time links appear.
type experienceLineageCache struct {
	mu      sync.Mutex
	loaded  bool
	targets map[string][]string
}

func (c *Coordinator) experienceLineageTargets(sourceID string) []string {
	if c == nil {
		return nil
	}
	c.experienceLineage.mu.Lock()
	loaded := c.experienceLineage.loaded
	c.experienceLineage.mu.Unlock()
	if !loaded {
		c.reloadExperienceLineage(context.Background())
	}
	c.experienceLineage.mu.Lock()
	defer c.experienceLineage.mu.Unlock()
	return c.experienceLineage.targets[sourceID]
}

func (c *Coordinator) reloadExperienceLineage(ctx context.Context) []contextstore.ExperienceLineage {
	lister, ok := c.contextRepo.(experienceLineageLister)
	if !ok {
		return nil
	}
	lineage, err := lister.ListExperienceLineage(ctx, c.contextScope().ProjectID)
	if err != nil {
		log.Printf("warning: experience lineage unavailable: %v", err)
		return nil
	}
	c.experienceLineage.mu.Lock()
	c.experienceLineage.loaded = true
	c.experienceLineage.targets = experienceLineageTargets(lineage)
	c.experienceLineage.mu.Unlock()
	return lineage
}

// inheritPromotedExperience gives records confirmed by an accepted run the
// evidence their source session records already earned. Later evidence on a
// source reaches its targets through reduceMemoryEvent.
func (c *Coordinator) inheritPromotedExperience(ctx context.Context, confirmed []contextstore.ContextItem) {
	repo, ok := c.contextRepo.(contextstore.ExperienceRepository)
	if !ok || c.eventStore == nil || len(confirmed) == 0 {
		return
	}
	lineage := c.reloadExperienceLineage(ctx)
	confirmedIDs := make(map[string]bool, len(confirmed))
	for _, item := range confirmed {
		confirmedIDs[item.ID] = true
	}
	var links []contextstore.ExperienceLineage
	for _, link := range lineage {
		if confirmedIDs[link.TargetID] {
			links = append(links, link)
		}
	}
	if len(links) == 0 {
		return
	}
	events, err := c.eventStore.ReadEvents()
	if err != nil {
		log.Printf("warning: read events for promoted experience: %v", err)
		return
	}
	observations := ExperienceObservationsFromEvents(events, c.session.Config.MemoryLearning)
	inherited := InheritExperience(observations, links)[len(observations):]
	counts := make(map[string]int)
	for _, observation := range inherited {
		if _, err := repo.ApplyExperienceObservation(ctx, observation); err != nil {
			c.recordLearningGap(RunEvent{Type: "memory_aggregate_repair", IdempotencyKey: observation.IdempotencyKey}, err)
			continue
		}
		counts[observation.ContextItemID]++
	}
	for _, link := range links {
		_ = c.emitEvent("memory_experience_inherited", "coordinator", "", map[string]interface{}{
			"context_item_id": link.TargetID, "source_context_item_id": link.SourceID, "observations": counts[link.TargetID],
		})
	}
}
