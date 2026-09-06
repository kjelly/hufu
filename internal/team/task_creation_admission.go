package team

import (
	"context"
	"fmt"
	"strings"
)

// validateTaskCreationAdmission is the final guard immediately before a
// durable task_created append. Creation callers may reserve IDs and perform
// admission separately, but persistence must never make a markerless or
// mismatched occurrence executable. The task is still absent from TodoList at
// this point, so validation is performed directly against the prospective
// projection rather than through validateTaskOccurrenceAdmission.
func (c *Coordinator) validateTaskCreationAdmission(ctx context.Context, item *TodoItem) error {
	if c == nil || item == nil || !c.requiresTaskCreationAdmission() {
		return nil
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		return fmt.Errorf("validate task %s admission: %w", item.ID, err)
	}
	if journal == nil {
		return fmt.Errorf("validate task %s admission: decision journal is unavailable", item.ID)
	}
	attempt := item.Retries + 1
	admission, found, err := loadDecisionAdmission(ctx, journal, item.ID, attempt)
	if err != nil {
		return fmt.Errorf("validate task %s admission: load marker: %w", item.ID, err)
	}
	if !found {
		return fmt.Errorf("task %s attempt %d has no decision admission", item.ID, attempt)
	}
	projection, err := newTaskOccurrenceProjection(item)
	if err != nil {
		return fmt.Errorf("validate task %s admission: build occurrence projection: %w", item.ID, err)
	}
	digest, err := decisionTaskInputDigest(projection)
	if err != nil {
		return fmt.Errorf("validate task %s admission: compute task digest: %w", item.ID, err)
	}
	if strings.TrimSpace(admission.TaskInputDigest) == "" || digest != admission.TaskInputDigest {
		return fmt.Errorf("task %s attempt %d decision admission does not match task input", item.ID, attempt)
	}
	return nil
}

// requiresTaskCreationAdmission identifies the live executable persistence
// boundary. Legacy direct projection tests and non-runtime callers can still
// use an injected journal without pretending that an execution run was
// admitted; production runs always have an execution identity before task
// creation begins.
func (c *Coordinator) requiresTaskCreationAdmission() bool {
	return c != nil && c.hasDurableEventJournal() && strings.TrimSpace(c.executionRunID) != ""
}
