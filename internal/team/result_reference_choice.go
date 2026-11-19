package team

import "errors"

// This marker records failed namespace authentication, unlike the provisional
// source classification used by ordinary model claim admission.
type resultReferenceNamespaceError struct {
	cause error
}

func (e *resultReferenceNamespaceError) Error() string { return e.cause.Error() }
func (e *resultReferenceNamespaceError) Unwrap() error { return e.cause }

func rejectResultReferenceSource(err error) error {
	return rejectLocalResult(RepairFailureSource, &resultReferenceNamespaceError{cause: err})
}

// A model-selected value from an independently authenticated finite namespace.
// Source integrity errors must never implement this classification.
type resultReferenceChoiceFailure interface {
	error
	resultReferenceChoice()
}

func (*resultEvidenceChoiceError) resultReferenceChoice()    {}
func (*resultWorksetPathChoiceError) resultReferenceChoice() {}

func isResultReferenceChoiceError(err error) bool {
	if _, invalid := errors.AsType[*resultReferenceNamespaceError](err); invalid {
		return false
	}
	_, ok := errors.AsType[resultReferenceChoiceFailure](err)
	return ok
}
