package team

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
	"github.com/anomalyco/hufu/internal/utils"
)

// CapabilityResult captures the outcome of a preflight probe.
type CapabilityResult struct {
	Name      string
	Probe     string
	Scope     string
	Available bool
	Reason    string
	Evidence  string
	CheckedAt time.Time
}

type capabilityBlockedError struct {
	Result CapabilityResult
}

func (e capabilityBlockedError) Error() string {
	if e.Result.Reason != "" {
		return e.Result.Reason
	}
	if e.Result.Name != "" {
		return fmt.Sprintf("capability %q unavailable", e.Result.Name)
	}
	return "required capability unavailable"
}

func isCapabilityBlockedError(err error) (CapabilityResult, bool) {
	var blocked capabilityBlockedError
	if errors.As(err, &blocked) {
		return blocked.Result, true
	}
	return CapabilityResult{}, false
}

func normalizeCapabilityName(name string) string {
	return normalizeTaskCacheKey(name)
}

func capabilityKey(req agent.CapabilityRequirement) string {
	return normalizeCapabilityName(req.Name) + "|" + normalizeTaskCacheKey(req.Probe) + "|" + normalizeCapabilityName(req.Scope)
}

func capabilityTimeout(req agent.CapabilityRequirement) time.Duration {
	if req.Timeout > 0 {
		return time.Duration(req.Timeout) * time.Second
	}
	return 10 * time.Second
}

func truncateProbeEvidence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	return utils.TruncateString(s, 500)
}

func (c *Coordinator) capabilityRequirementsByName() map[string]agent.CapabilityRequirement {
	result := make(map[string]agent.CapabilityRequirement)
	if c == nil || c.session == nil {
		return result
	}
	for _, req := range c.session.Config.Preflight {
		key := normalizeCapabilityName(req.Name)
		if key == "" {
			continue
		}
		result[key] = req
	}
	return result
}

func (c *Coordinator) taskCapabilityRequirements(names []string) ([]agent.CapabilityRequirement, []string) {
	if len(names) == 0 {
		return nil, nil
	}
	reqsByName := c.capabilityRequirementsByName()
	var reqs []agent.CapabilityRequirement
	var missing []string
	for _, name := range names {
		req, ok := reqsByName[normalizeCapabilityName(name)]
		if !ok {
			missing = append(missing, name)
			continue
		}
		reqs = append(reqs, req)
	}
	return reqs, missing
}

func (c *Coordinator) checkCapabilityRequirements(ctx context.Context, reqs []agent.CapabilityRequirement) ([]CapabilityResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	results := make([]CapabilityResult, 0, len(reqs))
	for _, req := range reqs {
		res := c.checkCapability(ctx, req)
		results = append(results, res)
		if !res.Available {
			return results, capabilityBlockedError{Result: res}
		}
	}
	return results, nil
}

func (c *Coordinator) checkCapability(ctx context.Context, req agent.CapabilityRequirement) CapabilityResult {
	req.Name = strings.TrimSpace(req.Name)
	req.Probe = strings.TrimSpace(req.Probe)
	req.Scope = strings.TrimSpace(req.Scope)
	key := capabilityKey(req)
	now := time.Now()

	c.capabilityCacheMu.Lock()
	if res, ok := c.capabilityCache[key]; ok {
		c.capabilityCacheMu.Unlock()
		return res
	}
	if ch, ok := c.capabilityInflight[key]; ok {
		c.capabilityCacheMu.Unlock()
		select {
		case res := <-ch:
			return res
		case <-ctx.Done():
			return CapabilityResult{
				Name:      req.Name,
				Probe:     req.Probe,
				Scope:     req.Scope,
				Available: false,
				Reason:    ctx.Err().Error(),
				CheckedAt: now,
			}
		}
	}
	ch := make(chan CapabilityResult, 1)
	c.capabilityInflight[key] = ch
	c.capabilityCacheMu.Unlock()

	res := c.probeCapability(ctx, req)
	c.capabilityCacheMu.Lock()
	c.capabilityCache[key] = res
	delete(c.capabilityInflight, key)
	c.capabilityCacheMu.Unlock()
	ch <- res
	return res
}

func (c *Coordinator) probeCapability(ctx context.Context, req agent.CapabilityRequirement) CapabilityResult {
	res := CapabilityResult{
		Name:      strings.TrimSpace(req.Name),
		Probe:     strings.TrimSpace(req.Probe),
		Scope:     strings.TrimSpace(req.Scope),
		CheckedAt: time.Now(),
	}
	if res.Name == "" {
		res.Name = normalizeTaskCacheKey(res.Probe)
	}
	if res.Probe == "" {
		res.Reason = fmt.Sprintf("capability %q has no probe command", res.Name)
		return res
	}

	shell := "sh"
	if c != nil && c.session != nil && c.session.Config.Shell != "" {
		shell = c.session.Config.Shell
	}
	timeout := capabilityTimeout(req)
	if c != nil && c.session != nil && c.session.Config.Timeout > 0 {
		maxTimeout := time.Duration(c.session.Config.Timeout) * time.Second
		if maxTimeout > 0 && timeout > maxTimeout {
			timeout = maxTimeout
		}
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, shell, "-c", req.Probe)
	if c != nil && c.projectDir != "" {
		cmd.Dir = c.projectDir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	res.CheckedAt = start
	evidence := strings.TrimSpace(strings.Join([]string{stdout.String(), stderr.String()}, "\n"))
	res.Evidence = truncateProbeEvidence(evidence)
	if err != nil {
		if runCtx.Err() != nil {
			res.Reason = fmt.Sprintf("capability %q probe timed out after %s", res.Name, timeout.Round(time.Second))
		} else {
			res.Reason = fmt.Sprintf("capability %q probe failed: %v", res.Name, err)
		}
		if res.Evidence != "" {
			res.Reason += ": " + res.Evidence
		}
		return res
	}
	res.Available = true
	if res.Evidence == "" {
		res.Evidence = "probe succeeded"
	}
	return res
}
