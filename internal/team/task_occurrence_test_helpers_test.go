package team

import "context"

func occurrenceTestContext(c *Coordinator, taskID string, attempt int) context.Context {
	identity, ok := c.activeTaskResultOccurrence(taskID)
	if !ok {
		c.setCurrentTaskAttempt(taskID, attempt)
		identity, ok = c.activeTaskResultOccurrence(taskID)
	} else if identity.Attempt == attempt {
		return withSubmitResultRuntimeIdentity(context.Background(), identity)
	} else {
		// A delayed worker context must retain its original attempt without
		// reopening the controller for the current attempt.
		identity.Attempt = attempt
	}
	if !ok {
		panic("test occurrence was not opened")
	}
	return withSubmitResultRuntimeIdentity(context.Background(), identity)
}
