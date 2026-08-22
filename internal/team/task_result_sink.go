package team

import (
	"context"
	"errors"
	"fmt"
)

// coordinatorTaskResultSink is the Hufu-owned sink used by local Fantasy
// tools. It records attempt evidence only; lifecycle, verification, retry,
// and run acceptance remain coordinator responsibilities.
type coordinatorTaskResultSink struct{ coordinator *Coordinator }

func (s coordinatorTaskResultSink) Submit(ctx context.Context, taskID string, result TaskResult) error {
	if s.coordinator == nil {
		return fmt.Errorf("task result sink has no coordinator")
	}
	if taskID == "" {
		return fmt.Errorf("task result has no task id")
	}
	if err := validateSubmittedTaskResult(&result); err != nil {
		return err
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, s.coordinator, taskID)
	if err != nil {
		return err
	}
	tx, err := s.coordinator.beginTaskResultSubmission(identity)
	if err != nil {
		if errors.Is(err, errTaskResultDuplicate) && len(result.Artifacts) == 0 {
			// Preserve idempotent result-only retries. Artifact-bearing duplicates
			// remain errors so callers cannot mistake a rejected side effect for a
			// successful submission.
			return nil
		}
		return err
	}
	rollback := func(err error) error {
		if rollbackErr := tx.rollback(); rollbackErr != nil {
			return errors.Join(err, rollbackErr)
		}
		return err
	}
	if len(result.Artifacts) > 0 {
		if result.TaskID != identity.TaskID || result.Attempt != identity.Attempt || result.Agent != identity.Agent {
			return rollback(errTaskResultStale)
		}
		for i, artifact := range result.Artifacts {
			if artifact.RunID != identity.RunID || artifact.TaskID != identity.TaskID || artifact.Attempt != identity.Attempt || artifact.Agent != identity.Agent {
				return rollback(fmt.Errorf("artifact[%d] provenance does not match runtime occurrence", i))
			}
		}
		if err := tx.consumePending(result.Artifacts); err != nil {
			return rollback(err)
		}
	}
	if err := tx.commit(&result); err != nil {
		return rollback(err)
	}
	tx.finish()
	return nil
}

var _ TaskResultSink = coordinatorTaskResultSink{}
