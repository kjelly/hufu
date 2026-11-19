package main

import "github.com/kjelly/hufu/internal/team"

type runtimeValidationStatus struct {
	ID    string `json:"id"`
	Scope string `json:"scope"`
	Phase string `json:"phase"`
	Check string `json:"check,omitempty"`
}

func validationStatusIdentity(event team.StatusEvent) *runtimeValidationStatus {
	if event.Type != "runtime_validation" {
		return nil
	}
	id, _ := event.Data["validation_id"].(string)
	scope, _ := event.Data["scope"].(string)
	phase, _ := event.Data["phase"].(string)
	check, _ := event.Data["check"].(string)
	return &runtimeValidationStatus{ID: id, Scope: scope, Phase: phase, Check: check}
}
