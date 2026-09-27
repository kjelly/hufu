package context

import (
	"errors"
	"fmt"
)

var (
	// ErrReservedSourceType means a generic repository method was asked to
	// create, confirm, bind, or reject an item whose source type is owned by a
	// dedicated review workflow.
	ErrReservedSourceType = errors.New("context source type is reserved for a dedicated review workflow")
	// ErrLifecycleTransition means a generic lifecycle mutation was asked for a
	// transition other than candidate to rejected.
	ErrLifecycleTransition = errors.New("invalid context lifecycle transition")
	// ErrCandidateIdentityConflict means identical content already exists in
	// the same scope and kind under a different source type, so refreshing it
	// would hand it to another workflow.
	ErrCandidateIdentityConflict = errors.New("candidate content already exists under another source")
	// ErrOperatorRejected means an operator rejected identical content, so a
	// later proposal must not reopen it.
	ErrOperatorRejected = errors.New("identical content was rejected by an operator")
)

// IsReservedSourceType reports whether only a dedicated workflow may create or
// change the lifecycle of items with this source type.
func IsReservedSourceType(sourceType string) bool {
	return sourceType == SourceTypeConsolidationProposal
}

func reservedSourceError(id string) error {
	return fmt.Errorf("%w: context item %q belongs to a consolidation proposal; use hufu context consolidation approve or reject", ErrReservedSourceType, id)
}

func candidateIdentityConflict(existingID, existingSourceType string) error {
	return fmt.Errorf("%w: item %q has source type %q", ErrCandidateIdentityConflict, existingID, existingSourceType)
}

func operatorRejected(existingID string) error {
	return fmt.Errorf("%w: item %q", ErrOperatorRejected, existingID)
}

// hasOperatorRejection reports whether an operator, rather than a failed run,
// rejected item.
func hasOperatorRejection(item ContextItem) bool {
	if item.Lifecycle != LifecycleRejected {
		return false
	}
	for _, evidence := range item.Evidence {
		if evidence.Type == EvidenceTypeOperatorRejection {
			return true
		}
	}
	return false
}

func refuseReservedItems(items []ContextItem) error {
	for _, item := range items {
		if IsReservedSourceType(item.Source.Type) {
			return fmt.Errorf("%w: source type %q cannot be written through a generic append", ErrReservedSourceType, item.Source.Type)
		}
	}
	return nil
}
