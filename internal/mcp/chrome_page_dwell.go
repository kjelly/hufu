package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/kjelly/hufu/internal/waitbudget"
)

// ChromePageDwellConfig is an operator-owned delay after the first page capture
// following successful navigation.
// The model cannot choose or shorten the delay through tool arguments.
type ChromePageDwellConfig struct {
	MinSeconds int `json:"min-seconds" yaml:"min-seconds"`
	MaxSeconds int `json:"max-seconds" yaml:"max-seconds"`
}

func (d *ChromePageDwellConfig) UnmarshalJSON(data []byte) error {
	type plain ChromePageDwellConfig
	return json.Unmarshal(data, (*plain)(d), json.RejectUnknownMembers(true))
}

func (d *ChromePageDwellConfig) UnmarshalYAML(node *yaml.Node) error {
	var value map[string]any
	if err := node.Decode(&value); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return d.UnmarshalJSON(data)
}

func (d *ChromePageDwellConfig) validate() error {
	if d != nil && (d.MinSeconds < 1 || d.MaxSeconds < d.MinSeconds || d.MaxSeconds > 3600) {
		return errors.New("page-dwell requires 1 <= min-seconds <= max-seconds <= 3600")
	}
	return nil
}

func (d *ChromePageDwellConfig) duration() time.Duration {
	if d == nil {
		return 0
	}
	seconds := d.MinSeconds + rand.IntN(d.MaxSeconds-d.MinSeconds+1)
	return time.Duration(seconds) * time.Second
}

// wait excludes only the intentional dwell from working-time budgets. The
// caller's cancellation and external deadlines remain authoritative. Shared
// clock counts overlapping provider, human and page waits only once.
func (d *ChromePageDwellConfig) wait(ctx context.Context) (time.Duration, error) {
	duration := d.duration()
	if duration == 0 {
		return 0, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	resume := waitbudget.PauseRunIdle(ctx)
	defer resume()
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-timer.C:
		return duration, ctx.Err()
	}
}

// snapshotChromeWithDwell captures the source before any intentional idle.
func snapshotChromeWithDwell(ctx context.Context, snapshot func(context.Context) (string, error), dwell *ChromePageDwellConfig) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	captured, err := snapshot(ctx)
	if err != nil {
		return "", err
	}
	elapsed, err := dwell.wait(ctx)
	if err != nil {
		return "", err
	}
	if elapsed > 0 {
		captured += fmt.Sprintf("\nSnapshot captured before configured page dwell completed: %d seconds. Content and button references describe capture time; take a fresh snapshot if the page changed.\n", int(elapsed.Seconds()))
	}
	return captured, nil
}
