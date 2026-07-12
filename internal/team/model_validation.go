package team

// Startup validation of configured model names. A typo like
// "ollama/glm-5.2cloud" (missing colon) previously surfaced only mid-run as a
// silently-lost request; this checks every configured model against the
// provider's /models list before the first delegation.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// collectConfiguredModels gathers every model ID the run can use: per-agent
// models, the escalation model list, and the sidecar model.
func (c *Coordinator) collectConfiguredModels() []string {
	seen := make(map[string]bool)
	var ids []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	for _, def := range c.session.Agents {
		add(def.Generation.Model)
		for _, extra := range def.ExtraModels {
			add(extra)
		}
	}
	for _, m := range c.modelList {
		add(m.ID)
	}
	add(c.sidecarModel)
	add(c.session.Config.Generation.Model)
	sort.Strings(ids)
	return ids
}

// ValidateConfiguredModels checks each configured model against its
// provider's available-model list and returns an error naming every missing
// model with nearby suggestions. Callers may choose to treat the result as a
// warning. Providers whose /models endpoint cannot be queried are skipped —
// unreachable is "cannot validate", not "missing".
func (c *Coordinator) ValidateConfiguredModels(ctx context.Context) error {
	ids := c.collectConfiguredModels()
	if len(ids) == 0 {
		return nil
	}

	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	type providerModels struct {
		names map[string]bool
		ok    bool
	}
	cache := make(map[string]*providerModels)
	var problems []string

	for _, id := range ids {
		p := c.providerManager.GetProvider(id)
		key := p.Name()
		pm, cached := cache[key]
		if !cached {
			pm = &providerModels{}
			if names, err := p.ListModelNames(listCtx); err == nil {
				pm.ok = true
				pm.names = make(map[string]bool, len(names))
				for _, n := range names {
					pm.names[n] = true
				}
			}
			cache[key] = pm
		}
		if !pm.ok {
			continue
		}
		if modelAvailableOnProvider(id, key, pm.names) {
			continue
		}
		bare := strings.TrimPrefix(id, key+"/")
		problem := fmt.Sprintf("model %q not found on provider %q", id, key)
		if suggestions := nearestModelNames(bare, pm.names, 3); len(suggestions) > 0 {
			problem += fmt.Sprintf(" (did you mean: %s?)", strings.Join(suggestions, ", "))
		}
		problems = append(problems, problem)
	}

	if len(problems) > 0 {
		return fmt.Errorf("model validation failed:\n- %s", strings.Join(problems, "\n- "))
	}
	return nil
}

// modelAvailableOnProvider checks whether a configured model ID matches one of
// the provider-reported names, accepting either bare names or provider-prefixed
// names such as "ollama/qwen3:8b".
func modelAvailableOnProvider(modelID, providerName string, available map[string]bool) bool {
	bare := strings.TrimPrefix(modelID, providerName+"/")
	if available[modelID] || available[bare] {
		return true
	}
	for name := range available {
		if strings.TrimPrefix(name, providerName+"/") == bare {
			return true
		}
	}
	return false
}

// nearestModelNames returns up to max available names close to the missing
// one. Comparison ignores the tag colon so "glm-5.2cloud" matches
// "glm-5.2:cloud", plus a short shared prefix to catch wrong tags — enough
// for common typos without a full edit-distance implementation.
func nearestModelNames(missing string, available map[string]bool, max int) []string {
	norm := func(s string) string {
		s = strings.ToLower(s)
		if idx := strings.IndexByte(s, '/'); idx >= 0 {
			s = s[idx+1:]
		}
		return strings.ReplaceAll(s, ":", "")
	}
	base := norm(missing)
	if len(base) < 2 {
		return nil
	}
	prefix := base
	if len(prefix) > 5 {
		prefix = prefix[:5]
	}
	var out []string
	for name := range available {
		n := norm(name)
		if n == base || strings.HasPrefix(n, prefix) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	if len(out) > max {
		out = out[:max]
	}
	return out
}
