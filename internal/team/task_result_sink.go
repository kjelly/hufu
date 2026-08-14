package team

import (
	"context"
	"fmt"
)

// coordinatorTaskResultSink is the Hufu-owned sink used by local Fantasy
// tools. It records attempt evidence only; lifecycle, verification, retry,
// and run acceptance remain coordinator responsibilities.
type coordinatorTaskResultSink struct{ coordinator *Coordinator }

func (s coordinatorTaskResultSink) Submit(_ context.Context, taskID string, result TaskResult) error {
	if s.coordinator == nil {
		return fmt.Errorf("task result sink has no coordinator")
	}
	if taskID == "" {
		return fmt.Errorf("task result has no task id")
	}
	copy := result
	copy.TaskID = taskID
	s.coordinator.storeSubmittedTaskResult(taskID, &copy)
	return nil
}

var _ TaskResultSink = coordinatorTaskResultSink{}
