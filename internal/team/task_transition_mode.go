package team

import "fmt"

// validateTaskTransitionMode distinguishes normal lifecycle updates, explicit
// worker resets and operator-authorized submission repair. The caller holds
// the projection lock and commits through the same event-first owner.
func (c *Coordinator) validateTaskTransitionMode(current *TodoItem, next TaskStatus, metadata map[string]any) (bool, *ResultRepairGrant, error) {
	reset, _ := metadata["reset_for_resume"].(bool)
	grant, _ := metadata["result_repair_grant"].(*ResultRepairGrant)
	if grant != nil && !c.validResultRepairGrant(current, next, grant) {
		return false, nil, fmt.Errorf("commit task transition: invalid operator result repair grant")
	}
	if reset && next != TaskPending {
		return false, nil, fmt.Errorf("commit task transition: task %s resume reset must target pending", current.ID)
	}
	if !reset && grant == nil && !CanTransition(current.Status, next) {
		return false, nil, fmt.Errorf("commit task transition: invalid %s -> %s for task %s", current.Status, next, current.ID)
	}
	return reset, grant, nil
}
