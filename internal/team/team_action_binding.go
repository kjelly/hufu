package team

import "slices"

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
