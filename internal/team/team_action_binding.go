package team

import (
	"fmt"
	"slices"
	"strings"
)

// CatalogActionBinding links a dispatched catalog task to the catalog entry,
// canonical arguments, linked proposals, and durable invocation that produced
// it. It is runtime-owned: the coordinator names only a catalog ID and
// arguments, and the runtime compiles everything else from the frozen catalog.
type CatalogActionBinding struct {
	ActionID      string   `json:"action_id"`
	EntryHash     string   `json:"entry_hash"`
	CatalogHash   string   `json:"catalog_hash"`
	ArgumentsHash string   `json:"arguments_hash"`
	InvocationID  string   `json:"invocation_id"`
	ProposalIDs   []string `json:"proposal_ids,omitempty"`
}

// clone returns a deep copy. An empty ProposalIDs is normalized to nil:
// occurrence comparison uses reflect.DeepEqual, where an empty slice and nil
// differ.
func (b *CatalogActionBinding) clone() *CatalogActionBinding {
	if b == nil {
		return nil
	}
	clone := *b
	clone.ProposalIDs = nil
	if len(b.ProposalIDs) > 0 {
		clone.ProposalIDs = slices.Clone(b.ProposalIDs)
	}
	return &clone
}

// validateCatalogActionIntegrity re-checks a catalog task against the frozen
// catalog before its provider starts. Policy snapshot drift normally stops a
// changed catalog earlier; this is the last defense, and its errors are
// validation errors so the provider is never started or retried.
func (c *Coordinator) validateCatalogActionIntegrity(task TaskDef) error {
	binding := task.CatalogAction
	if binding == nil {
		return nil
	}
	capability := ""
	if task.Action != nil {
		capability = normalizeCapability(task.Action.Capability)
	}
	var catalog *ActionCatalogSnapshot
	if c != nil && c.session != nil {
		catalog = c.session.ActionCatalog
	}
	entry, ok := catalog.Lookup(binding.ActionID)
	switch {
	case task.Action == nil:
		return ActionValidationError{Capability: capability, Cause: fmt.Errorf("team_action_catalog_drift: catalog task %q has no action", binding.ActionID)}
	case !ok || entry.Hash != binding.EntryHash:
		return ActionValidationError{Capability: capability, Cause: fmt.Errorf("team_action_catalog_drift: catalog entry %q no longer matches the dispatched entry", binding.ActionID)}
	case capability != entry.Capability || strings.TrimSpace(task.Action.Type) != entry.Type:
		return ActionValidationError{Capability: capability, Cause: fmt.Errorf("team_action_catalog_drift: action differs from catalog entry %q", binding.ActionID)}
	case runInputHash([]byte(task.Action.Payload)) != binding.ArgumentsHash:
		return ActionValidationError{Capability: capability, Cause: fmt.Errorf("team_action_arguments_drift: action payload does not match the dispatched arguments of %q", binding.ActionID)}
	}
	return nil
}

// catalogInvocationID is the durable invocation ID of a catalog task, or "".
func catalogInvocationID(task TaskDef) string {
	if task.CatalogAction == nil {
		return ""
	}
	return task.CatalogAction.InvocationID
}

// CatalogRuntimeFields identifies the catalog action behind an action
// receipt, lifecycle event, or execution receipt. Every field is omitted for
// an action that is not a catalog task, so those records keep their shape.
type CatalogRuntimeFields struct {
	CatalogActionID     string `json:"catalog_action_id,omitempty"`
	CatalogEntryHash    string `json:"catalog_entry_hash,omitempty"`
	ArgumentsHash       string `json:"arguments_hash,omitempty"`
	CatalogInvocationID string `json:"catalog_invocation_id,omitempty"`
}

func catalogRuntimeFields(binding *CatalogActionBinding) CatalogRuntimeFields {
	if binding == nil {
		return CatalogRuntimeFields{}
	}
	return CatalogRuntimeFields{
		CatalogActionID: binding.ActionID, CatalogEntryHash: binding.EntryHash,
		ArgumentsHash: binding.ArgumentsHash, CatalogInvocationID: binding.InvocationID,
	}
}

// catalogActionReceiptFields are the runtime action receipt's catalog fields:
// the shared identity plus linked proposals and the declared side effect.
type catalogActionReceiptFields struct {
	CatalogRuntimeFields
	ProposalIDs []string        `json:"proposal_ids,omitempty"`
	SideEffect  SideEffectClass `json:"side_effect,omitempty"`
}

func catalogReceiptFields(task TaskDef) catalogActionReceiptFields {
	if task.CatalogAction == nil {
		return catalogActionReceiptFields{}
	}
	return catalogActionReceiptFields{
		CatalogRuntimeFields: catalogRuntimeFields(task.CatalogAction),
		ProposalIDs:          slices.Clone(task.CatalogAction.ProposalIDs), SideEffect: task.SideEffect,
	}
}

// validateCatalogActionOutputs checks a catalog action's canonical outputs
// against its entry's output schema. The provider has already run, so a
// mismatch is a validation error: the task fails and is not retried.
func (c *Coordinator) validateCatalogActionOutputs(task TaskDef, outputs map[string]any) error {
	if task.CatalogAction == nil || task.Action == nil || c == nil || c.session == nil {
		return nil
	}
	entry, ok := c.session.ActionCatalog.Lookup(task.CatalogAction.ActionID)
	if !ok || entry.OutputSchema == nil {
		return nil
	}
	if err := validateCatalogOutputs(*entry.OutputSchema, outputs); err != nil {
		return ActionValidationError{Capability: normalizeCapability(task.Action.Capability), Cause: fmt.Errorf("team_action_output_invalid: %w", err)}
	}
	return nil
}
