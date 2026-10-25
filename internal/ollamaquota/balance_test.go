package ollamaquota

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/kjelly/hufu/internal/waitbudget"
)

func TestBalanceQueryTimeoutIsIndependentOfPausedModelBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := waitbudget.WithTimeout(t.Context(), time.Second)
		defer cancel()
		resume := waitbudget.Pause(ctx)
		defer resume()
		start := time.Now()
		e := New(roundTripFunc(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() }))
		if _, err := e.query(ctx, "key"); err == nil {
			t.Fatal("balance request did not time out")
		}
		if time.Since(start) != balanceTimeout || ctx.Err() != nil {
			t.Fatalf("elapsed=%s model timeout=%v", time.Since(start), ctx.Err())
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func balanceResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func legacyBalance(session, weekly int, sessionReset, weeklyReset time.Time) string {
	return fmt.Sprintf(`{"included":{"session":{"remaining_percent":%d,"resets_at":%q},"weekly":{"remaining_percent":%d,"resets_at":%q}}}`, session, sessionReset.Format(time.RFC3339), weekly, weeklyReset.Format(time.RFC3339))
}

func TestBalanceReset(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	session, weekly := now.Add(time.Hour), now.Add(48*time.Hour)
	for _, tt := range []struct {
		name, body string
		want       time.Time
		invalid    bool
	}{
		{"session", legacyBalance(0, 10, session, weekly), session, false},
		{"weekly", legacyBalance(10, 0, session, weekly), weekly, false},
		{"both", legacyBalance(0, 0, session, weekly), weekly, false},
		{"available", legacyBalance(1, 10, session, weekly), time.Time{}, false},
		{"stale", legacyBalance(0, 10, now.Add(-time.Hour), weekly), now.Add(10 * time.Minute), false},
		{"monthly", fmt.Sprintf(`{"included":{"balance_usd":0,"period":{"until":%q}},"purchased":{"balance_usd":0}}`, weekly.Format(time.RFC3339)), weekly, false},
		{"purchased", fmt.Sprintf(`{"included":{"balance_usd":0,"period":{"until":%q}},"purchased":{"balance_usd":1}}`, weekly.Format(time.RFC3339)), time.Time{}, false},
		{"missing", `{}`, time.Time{}, true},
		{"missing weekly", `{"included":{"session":{"remaining_percent":0,"resets_at":"2026-10-25T12:00:00Z"}}}`, time.Time{}, true},
		{"out of range", legacyBalance(101, 0, session, weekly), time.Time{}, true},
		{"invalid future", legacyBalance(0, 1, now.Add(400*24*time.Hour), weekly), time.Time{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var b balance
			if err := json.Unmarshal([]byte(tt.body), &b); err != nil {
				t.Fatal(err)
			}
			got, err := b.reset(now)
			if (err != nil) != tt.invalid {
				t.Fatalf("reset error=%v", err)
			}
			if !tt.invalid && !got.Equal(tt.want) {
				t.Fatalf("reset=%v want %v", got, tt.want)
			}
		})
	}
}

func TestBalanceQueryUsesOnlyOfficialHost(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		valid      bool
	}{
		{"valid", legacyBalance(75, 40, time.Now().Add(time.Hour), time.Now().Add(24*time.Hour)), 200, true},
		{"malformed", `not json`, 200, false},
		{"duplicate", `{"included":{},"included":{}}`, 200, false},
		{"large", strings.Repeat("x", 65537), 200, false},
		{"redirect", `secret`, 302, false},
		{"unauthorized", `secret`, 401, false},
		{"unavailable", `secret`, 503, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			e := New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.String() != balanceURL || r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test-key" {
					t.Fatal("incorrect balance request")
				}
				response := balanceResponse(tt.body)
				response.StatusCode = tt.status
				response.Header.Set("Location", "https://untrusted.example/")
				return response, nil
			}))
			_, err := e.query(t.Context(), "test-key")
			if (err == nil) != tt.valid || calls != 1 {
				t.Fatalf("query error=%v calls=%d", err, calls)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("response body leaked")
			}
		})
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tt := range []struct {
		value string
		want  time.Time
	}{
		{"600", now.Add(10 * time.Minute)},
		{now.Add(time.Hour).Format(http.TimeFormat), now.Add(time.Hour)},
		{"-1", time.Time{}}, {"0", time.Time{}}, {"garbage", time.Time{}}, {"99999999999999", time.Time{}},
	} {
		if got := retryAfter(tt.value, now); !got.Equal(tt.want) {
			t.Errorf("retryAfter(%q)=%v", tt.value, got)
		}
	}
}
