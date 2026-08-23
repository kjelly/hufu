package team

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

// ErrDegenerateRepetitionLoop indicates that the LLM output has degenerated
// into an infinite loop of repeating single runes or repeating n-gram phrases.
var ErrDegenerateRepetitionLoop = errors.New("model output trapped in degenerate repetition loop")

const (
	// DefaultMaxConsecutiveRunes is the maximum number of times a single rune
	// may repeat consecutively in streaming output before triggering an abort.
	DefaultMaxConsecutiveRunes = 100

	// DefaultMaxRollingBufferRunes is the maximum history kept for n-gram cycle detection.
	DefaultMaxRollingBufferRunes = 1024
)

// StreamRepetitionDetector tracks streaming output chunks to detect degenerate
// repetition loops (e.g. single-character cascades like "////..." or repeating phrases).
type StreamRepetitionDetector struct {
	mu                  sync.Mutex
	maxConsecutiveRunes int
	maxBufferRunes      int
	lastRune            rune
	consecutiveCount    int
	buffer              []rune
}

// NewStreamRepetitionDetector creates a detector with default thresholds.
func NewStreamRepetitionDetector() *StreamRepetitionDetector {
	return &StreamRepetitionDetector{
		maxConsecutiveRunes: DefaultMaxConsecutiveRunes,
		maxBufferRunes:      DefaultMaxRollingBufferRunes,
	}
}

// Process analyzes an incoming streaming chunk. If a degenerate loop is detected,
// it returns ErrDegenerateRepetitionLoop.
func (d *StreamRepetitionDetector) Process(chunk string) error {
	if d == nil || chunk == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	runes := []rune(chunk)
	for _, r := range runes {
		if r == d.lastRune {
			d.consecutiveCount++
			if d.consecutiveCount >= d.maxConsecutiveRunes {
				return fmt.Errorf("%w: rune %q repeated %d times", ErrDegenerateRepetitionLoop, r, d.consecutiveCount)
			}
		} else {
			d.lastRune = r
			d.consecutiveCount = 1
		}
	}

	// Append to rolling buffer
	d.buffer = append(d.buffer, runes...)
	if len(d.buffer) > d.maxBufferRunes {
		d.buffer = d.buffer[len(d.buffer)-d.maxBufferRunes:]
	}

	// Pattern repetition (n-gram) check
	bufLen := len(d.buffer)
	maxPatternLen := 64
	if maxPatternLen > bufLen/2 {
		maxPatternLen = bufLen / 2
	}

	for patternLen := 2; patternLen <= maxPatternLen; patternLen++ {
		pattern := d.buffer[bufLen-patternLen:]
		repeats := 1
		for i := bufLen - 2*patternLen; i >= 0; i -= patternLen {
			if slices.Equal(d.buffer[i:i+patternLen], pattern) {
				repeats++
			} else {
				break
			}
		}

		// Thresholds for repeating patterns:
		// 2-4 runes (e.g. "//" or "a "): >= 40 repeats (e.g. 80-160 chars)
		// 5-16 runes (e.g. "I am sorry"): >= 15 repeats (e.g. 75-240 chars)
		// 17-64 runes (long phrase): >= 8 repeats (e.g. 136-512 chars)
		if patternLen >= 2 && patternLen <= 4 && repeats >= 40 {
			return fmt.Errorf("%w: pattern %q (%d runes) repeated %d times", ErrDegenerateRepetitionLoop, string(pattern), patternLen, repeats)
		}
		if patternLen >= 5 && patternLen <= 16 && repeats >= 15 {
			return fmt.Errorf("%w: pattern %q (%d runes) repeated %d times", ErrDegenerateRepetitionLoop, string(pattern), patternLen, repeats)
		}
		if patternLen >= 17 && patternLen <= 64 && repeats >= 8 {
			return fmt.Errorf("%w: pattern %q (%d runes) repeated %d times", ErrDegenerateRepetitionLoop, string(pattern), patternLen, repeats)
		}
	}

	return nil
}
