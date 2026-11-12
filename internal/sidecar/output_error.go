package sidecar

import (
	"errors"
	"fmt"
)

var ErrIncompleteOutput = errors.New("sidecar output is incomplete")

// IncompleteOutputError reports provider token-limit completion without
// publishing a partial response as an accepted result.
type IncompleteOutputError struct {
	ReservedTokens int64
	OutputTokens   int64
	ResponseBytes  int
}

func (e *IncompleteOutputError) Error() string {
	return fmt.Sprintf("sidecar output truncated by provider token limit (reserved=%d, output_tokens=%d, response_bytes=%d)", e.ReservedTokens, e.OutputTokens, e.ResponseBytes)
}

func (e *IncompleteOutputError) Unwrap() error { return ErrIncompleteOutput }
