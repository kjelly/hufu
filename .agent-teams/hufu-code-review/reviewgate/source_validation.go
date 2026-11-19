package main

import "fmt"

// A source error requires repair of accepted evidence that the synthesizer
// cannot rewrite.
type sourceReviewError struct{ cause error }

func (e *sourceReviewError) Error() string {
	return fmt.Sprintf("accepted review sources require repair before report synthesis: %v", e.cause)
}

func (e *sourceReviewError) Unwrap() error { return e.cause }
