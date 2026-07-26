package team

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// ShadowContextTrace contains diagnostics only. It deliberately excludes
// prompt content, which may contain credentials or other sensitive material.
type ShadowContextTrace struct {
	ID                string    `json:"id"`
	Kind              string    `json:"kind"`
	CreatedAt         time.Time `json:"created_at"`
	Fingerprint       string    `json:"fingerprint,omitempty"`
	LegacyTokens      int       `json:"legacy_tokens"`
	CanonicalTokens   int       `json:"canonical_tokens"`
	BudgetTokens      int       `json:"budget_tokens"`
	SelectedItems     int       `json:"selected_items"`
	DuplicateRatio    float64   `json:"duplicate_ratio"`
	MissingAnchors    []string  `json:"missing_anchors,omitempty"`
	LegacyOnlyData    []string  `json:"legacy_only_data,omitempty"`
	CanonicalOnlyData []string  `json:"canonical_only_data,omitempty"`
	CriticalCoverage  []string  `json:"critical_coverage,omitempty"`
	Error             string    `json:"error,omitempty"`
}

var shadowTraceMu sync.Mutex
var shadowAnchorRE = regexp.MustCompile(`(?i)(?:\b(?:[a-f0-9]{7,40}|[A-Za-z0-9_./-]+\.(?:go|md|yaml|yml|json)|[A-Za-z_][A-Za-z0-9_]*\s*=|exit code\s+\d+)\b)`)

func (c *Coordinator) compileShadowCoordinator(ctx context.Context, input CoordinatorContextInput, legacy string) {
	compiled, err := c.ContextCompiler().CompileCoordinatorContext(ctx, input)
	c.recordShadowTrace(ctx, "coordinator", legacy, input.ModelContext, compiled, err)
}

func (c *Coordinator) compileShadowWorker(ctx context.Context, input WorkerContextInput, legacy string) {
	compiled, err := c.ContextCompiler().CompileWorkerContext(ctx, input)
	c.recordShadowTrace(ctx, "worker", legacy, input.ModelContext, compiled, err)
}

func (c *Coordinator) recordShadowTrace(ctx context.Context, kind, legacy string, spec ModelContextSpec, compiled CompiledContext, compileErr error) {
	legacyTokens, _ := defaultCounter.CountText(ctx, spec.ModelID, legacy)
	trace := ShadowContextTrace{ID: newShadowTraceID(), Kind: kind, CreatedAt: time.Now().UTC(), LegacyTokens: legacyTokens}
	if compileErr != nil {
		trace.Error = compileErr.Error()
	} else {
		trace.Fingerprint, trace.CanonicalTokens = compiled.Fingerprint, compiled.UsedTokens
		trace.SelectedItems = len(compiled.IncludedItems)
		trace.BudgetTokens = CalculateContextBudget(spec, 0, 0).Available
		if n := len(compiled.IncludedItems) + len(compiled.OmittedItems); n > 0 {
			trace.DuplicateRatio = float64(n-len(DeduplicateContextItems(append(append([]ContextItem(nil), compiled.IncludedItems...), compiled.OmittedItems...)))) / float64(n)
		}
		canonical := compiled.Prompt
		trace.LegacyOnlyData = shadowOnlyLines(legacy, canonical)
		trace.CanonicalOnlyData = shadowOnlyLines(canonical, legacy)
		for _, anchor := range shadowAnchorRE.FindAllString(legacy, -1) {
			if !containsAnchor(canonical, anchor) {
				trace.MissingAnchors = append(trace.MissingAnchors, anchor)
			} else {
				trace.CriticalCoverage = append(trace.CriticalCoverage, anchor)
			}
		}
	}
	c.persistShadowTrace(trace)
}

func shadowOnlyLines(source, other string) []string {
	var only []string
	for _, line := range strings.Split(source, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.Contains(other, line) {
			only = append(only, line)
		}
	}
	return only
}

func containsAnchor(s, anchor string) bool {
	return regexp.MustCompile(regexp.QuoteMeta(anchor)).FindStringIndex(s) != nil
}

func (c *Coordinator) persistShadowTrace(trace ShadowContextTrace) {
	if c == nil || c.session == nil || c.session.Workspace == "" {
		return
	}
	b, err := json.Marshal(trace)
	if err != nil {
		log.Printf("warning: marshal context shadow trace: %v", err)
		return
	}
	shadowTraceMu.Lock()
	defer shadowTraceMu.Unlock()
	path := filepath.Join(c.session.Workspace, "context-shadow-traces.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("warning: open context shadow trace log: %v", err)
		return
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		log.Printf("warning: write context shadow trace: %v", err)
	}
}

func newShadowTraceID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "trace-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return "trace-" + hex.EncodeToString(b)
}
