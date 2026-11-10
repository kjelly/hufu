package team

import "sync"

// SDK callbacks cannot erase a failure after a completed tool. Retain it until
// the worker boundary returns so result repair cannot turn StopTurn into success.
type toolMediaFailureState struct {
	mu  sync.Mutex
	err error
}

func (s *toolMediaFailureState) record(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = withFailureClassOverride(err, FailurePolicy)
	}
}

func (s *toolMediaFailureState) failure() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}
