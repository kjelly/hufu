// Package ollamaquota coordinates account-wide Ollama Cloud cooldowns.
package ollamaquota

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/kjelly/hufu/internal/waitbudget"
)

const balanceURL = "https://ollama.com/api/balance"
const balanceTimeout = 15 * time.Second

type limit struct {
	Remaining *float64  `json:"remaining_percent"`
	ResetsAt  time.Time `json:"resets_at"`
}

type balance struct {
	Included struct {
		Session *limit   `json:"session"`
		Weekly  *limit   `json:"weekly"`
		Balance *float64 `json:"balance_usd"`
		Period  struct {
			Until time.Time `json:"until"`
		} `json:"period"`
	} `json:"included"`
	Purchased struct {
		Balance *float64 `json:"balance_usd"`
	} `json:"purchased"`
}

// reset returns the latest reset among exhausted limits. Never infer a reset
// from a five-hour duration: both session and weekly windows can block a call.
func (b balance) reset(now time.Time) (time.Time, error) {
	var reset time.Time
	if b.Included.Session != nil || b.Included.Weekly != nil {
		for _, limit := range []*limit{b.Included.Session, b.Included.Weekly} {
			if limit == nil || limit.Remaining == nil || math.IsNaN(*limit.Remaining) || *limit.Remaining < 0 || *limit.Remaining > 100 || limit.ResetsAt.IsZero() {
				return reset, errors.New("invalid Ollama balance limit")
			}
			if *limit.Remaining == 0 && limit.ResetsAt.After(reset) {
				reset = limit.ResetsAt
			}
		}
	} else {
		if b.Included.Balance == nil || b.Purchased.Balance == nil || b.Included.Period.Until.IsZero() {
			return reset, errors.New("missing Ollama balance")
		}
		if *b.Included.Balance <= 0 && *b.Purchased.Balance <= 0 {
			reset = b.Included.Period.Until
		}
	}
	if reset.After(now.Add(366 * 24 * time.Hour)) {
		return time.Time{}, errors.New("invalid Ollama reset time")
	}
	if !reset.IsZero() && !reset.After(now) {
		reset = now.Add(10 * time.Minute)
	}
	return reset, nil
}

func retryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil && seconds > 0 && seconds <= 366*24*60*60 {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) && !date.After(now.Add(366*24*time.Hour)) {
		return date
	}
	return time.Time{}
}

func (e *Engine) query(ctx context.Context, key string) (time.Time, error) {
	if key == "" {
		return time.Time{}, errors.New("ollama balance API key unavailable")
	}
	ctx, cancel := waitbudget.WithWallDeadline(ctx, time.Now().Add(balanceTimeout))
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, balanceURL, nil)
	if err != nil {
		return time.Time{}, errors.New("create Ollama balance request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := e.client.Do(request)
	if err != nil {
		return time.Time{}, errors.New("ollama balance unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusTooManyRequests {
		return retryAfter(response.Header.Get("Retry-After"), time.Now()), errors.New("ollama balance rate limited")
	}
	if response.StatusCode != http.StatusOK {
		return time.Time{}, errors.New("ollama balance unavailable")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return time.Time{}, errors.New("invalid Ollama balance response")
	}
	var body balance
	if err := json.Unmarshal(data, &body); err != nil {
		return time.Time{}, errors.New("invalid Ollama balance response")
	}
	return body.reset(time.Now())
}
