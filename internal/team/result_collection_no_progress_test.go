package team

import (
	"errors"
	"testing"
)

func TestResultCollectionStopsIdenticalRepairsWithoutRepeatingWork(t *testing.T) {
	c, item, ctx, checkpoint := beginCollectionFixture(t)
	original := checkpoint.Work
	for turn := range 2 {
		reserved, err := c.reserveResultCollection(ctx, checkpoint, "collector-"+string(rune('a'+turn)), []byte(`{"tools":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		response := []byte(`{"status":"success","summary":"unchanged","unexpected":true}`)
		if turn == 1 {
			response = []byte(`{ "unexpected": true,"summary":"unchanged","status":"success" }`)
		}
		received, err := c.recordCollectionResponse(ctx, reserved, response)
		if err != nil {
			t.Fatal(err)
		}
		next, err := c.recordCollectionFormatFailure(ctx, received)
		if turn == 1 {
			if !errors.Is(err, errResultCollectionNoProgress) || next == nil {
				t.Fatalf("no progress was not classified: %v", err)
			}
			if next.LastRejectedTurn != 2 {
				t.Fatal("stopping did not checkpoint the repeated response")
			}
			if _, err = c.recordCollectionFormatFailure(ctx, next); err != nil {
				t.Fatalf("same durable turn was counted twice: %v", err)
			}
			if next.TurnsUsed != 2 || next.Work != original {
				t.Fatal("repair reset work or budget")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		// Revalidation of the same durable turn is not a new model response.
		checkpoint, err = c.recordCollectionFormatFailure(ctx, next)
		if err != nil {
			t.Fatal(err)
		}
		events, err := c.EventJournal().ReadEvents(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var replayed *TodoItem
		for _, task := range ReduceToTodoList(events) {
			if task.ID == item.ID {
				replayed = task
			}
		}
		if replayed == nil || currentResultCollection(replayed).LastRejectedSHA256 != checkpoint.LastRejectedSHA256 {
			t.Fatal("replay lost rejection fingerprint")
		}
	}
}
