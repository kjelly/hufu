package mcp

import (
	"encoding/json/v2"
	"errors"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/browser"
	"github.com/kjelly/hufu/internal/utils"
)

const chromeSourceAttemptLimit = 2

// chromeSourceAccess is an observation about availability, never page evidence.
// Both the text and structured MCP channels carry this same native outcome.
type chromeSourceAccess struct {
	Status       string    `json:"status"`
	URL          string    `json:"url"`
	Reason       string    `json:"reason"`
	Diagnostic   string    `json:"diagnostic"`
	Citable      bool      `json:"citable"`
	Executed     bool      `json:"executed"`
	ObservedAt   time.Time `json:"observed_at,omitzero"`
	Attempts     int       `json:"attempts"`
	AttemptLimit int       `json:"attempt_limit"`
	Exhausted    bool      `json:"exhausted"`
	NextAction   string    `json:"next_action"`
}

type chromeSourceAccessError struct{ outcome chromeSourceAccess }

// Only the availability outcome itself is recoverable. A wrapper or joined
// failure may also carry a policy, persistence or post-click failure.
func chromeSourceOutcome(err error) (*chromeSourceAccessError, bool) {
	outcome, ok := errors.AsType[*chromeSourceAccessError](err)
	return outcome, ok && err == outcome
}

func (e *chromeSourceAccessError) Error() string {
	data, _ := json.Marshal(e.outcome)
	return string(data)
}

func unavailableChromeSource(page browser.Snapshot) error {
	return &chromeSourceAccessError{outcome: chromeSourceAccess{
		Status: "source_unavailable", URL: page.URL, Reason: "access_error",
		Diagnostic: utils.TruncateRunes(page.Title+"\n"+page.Text, 2000),
		Executed:   true, ObservedAt: page.CapturedAt,
		AttemptLimit: chromeSourceAttemptLimit, NextAction: "use_another_source",
	}}
}

// Query strings and fragments cannot replenish a failed source's allowance.
// This deliberately groups URL variants conservatively by origin and path.
func chromeSourceKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
	}
	u.RawQuery, u.Fragment, u.RawFragment, u.RawPath = "", "", "", ""
	u.ForceQuery = false
	u.Path = path.Clean("/" + strings.TrimPrefix(u.Path, "/"))
	return u.String()
}

func (f *chromePageFlow) exhaustedSource(raw string) error {
	if prior := f.unavailableSources[chromeSourceKey(raw)]; prior != nil && prior.outcome.Exhausted {
		cached := *prior
		cached.outcome.Executed = false
		return &cached
	}
	return nil
}

func (f *chromePageFlow) recordSourceAccess(err error) error {
	unavailable, ok := chromeSourceOutcome(err)
	if !ok {
		return err
	}
	key := chromeSourceKey(unavailable.outcome.URL)
	if f.unavailableSources == nil {
		f.unavailableSources = make(map[string]*chromeSourceAccessError)
	}
	if len(f.unavailableSources) >= 1024 && f.unavailableSources[key] == nil {
		return errors.New("browser source allowance capacity reached; finalize collected work")
	}
	outcome := unavailable.outcome
	if prior := f.unavailableSources[key]; prior != nil {
		outcome.Attempts = prior.outcome.Attempts
	}
	outcome.Attempts = min(outcome.Attempts+1, chromeSourceAttemptLimit)
	outcome.Exhausted = outcome.Attempts >= chromeSourceAttemptLimit
	next := &chromeSourceAccessError{outcome: outcome}
	f.unavailableSources[key] = next
	// Bind requested and redirected URLs to one allowance, without trusting a
	// model-supplied URL on subsequent argument-free snapshot calls.
	if f.currentSource != "" {
		f.unavailableSources[chromeSourceKey(f.currentSource)] = next
	}
	f.currentSource = outcome.URL
	return next
}
