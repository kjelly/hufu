package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/embedding"
	"github.com/kjelly/hufu/internal/semantic"
)

// SemanticReplay is the read-only semantic lane of explain-memory and
// ranking-replay. It loads the configured encoder and reads the active
// generation as the runtime does, but never refreshes the index or writes
// traces. Vector results are memoized per request, so every ranking of one
// query in a replay sees the same lane output, fallbacks included.
type SemanticReplay struct {
	runtime  *SemanticRetrievalRuntime
	settings semantic.Settings
	mu       sync.Mutex
	cache    map[string]semanticReplayResult
}

type semanticReplayResult struct {
	results []contextstore.SearchResult
	err     error
}

// OpenSemanticReplay starts a read-only lane for settings and waits up to
// the load timeout for the encoder. It returns nil when the mode is off. An
// encoder that does not become ready is not an error: requests fall back as
// the runtime's would and report the reason.
func OpenSemanticReplay(ctx context.Context, settings semantic.Settings, repo contextstore.Repository, projectID string) (*SemanticReplay, error) {
	if !settings.Enabled() {
		return nil, nil
	}
	runtime, err := newSemanticRuntime(settings, repo, projectID, false, time.Hour, true)
	if err != nil {
		return nil, fmt.Errorf("open semantic replay: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, settings.LoadTimeout)
	defer cancel()
	if err := runtime.WaitReady(waitCtx); err != nil && ctx.Err() != nil {
		runtime.Close()
		return nil, fmt.Errorf("open semantic replay: %w", ctx.Err())
	}
	return &SemanticReplay{runtime: runtime, settings: settings, cache: map[string]semanticReplayResult{}}, nil
}

// Close stops the encoder runtime.
func (s *SemanticReplay) Close() {
	if s != nil {
		s.runtime.Close()
	}
}

// Settings are the settings the lane was opened with.
func (s *SemanticReplay) Settings() semantic.Settings { return s.settings }

// Status is the encoder runtime's status.
func (s *SemanticReplay) Status() SemanticRuntimeStatus { return s.runtime.Status() }

// SearchVector implements contextstore.VectorSearcher with memoization. A
// caller cancellation is returned and never cached.
func (s *SemanticReplay) SearchVector(ctx context.Context, req contextstore.SearchRequest) ([]contextstore.SearchResult, error) {
	key := semanticReplayKey(req)
	s.mu.Lock()
	cached, ok := s.cache[key]
	s.mu.Unlock()
	if ok {
		return slices.Clone(cached.results), cached.err
	}
	results, err := s.runtime.SearchVector(ctx, req)
	if err != nil && ctx.Err() != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[key] = semanticReplayResult{results: slices.Clone(results), err: err}
	s.mu.Unlock()
	return results, err
}

// SemanticTraceIdentity implements the trace identity provider.
func (s *SemanticReplay) SemanticTraceIdentity() (embedding.ModelIdentity, string, int64) {
	return s.runtime.SemanticTraceIdentity()
}

// lane is the persistent ranking lane for mode with fusion for requests
// that use it; nil for off. Auto routing here is stateless and uncounted.
func (s *SemanticReplay) lane(mode semantic.Mode, fusion contextstore.FusionMode) *persistentSemanticLane {
	if s == nil {
		return nil
	}
	lane := &persistentSemanticLane{Vector: s, Fusion: fusion}
	switch mode {
	case semantic.ModeShadow:
		lane.Mode = contextstore.RetrievalShadow
	case semantic.ModeActive:
		lane.Mode = contextstore.RetrievalActive
	case semantic.ModeAuto:
		lane.Mode, lane.Router = contextstore.RetrievalActive, scriptRouter{}
	default:
		return nil
	}
	return lane
}

// scriptRouter is auto routing without the runtime's counters.
type scriptRouter struct{}

func (scriptRouter) RouteSemantic(text string) bool { return semantic.RoutesSemantic(text) }

func semanticReplayKey(req contextstore.SearchRequest) string {
	sum := sha256.Sum256([]byte(strings.Join(req.AllowedItemIDs, "\x00")))
	return fmt.Sprintf("%s\x00%+v\x00%d\x00%t\x00%s", req.Query, req.Scope, req.Limit, req.AllowedItemIDs != nil, hex.EncodeToString(sum[:]))
}

// semanticReplayFusion is the lane fusion of a replayed ranking: override
// when set, else the configured semantic fusion.
func semanticReplayFusion(replay *SemanticReplay, override contextstore.FusionMode) contextstore.FusionMode {
	if override != "" {
		return override
	}
	if replay == nil {
		return ""
	}
	return contextstore.FusionMode(replay.settings.Fusion)
}

// SemanticReplayView is the content-free semantic part of an explanation or
// replay: the mode, the encoder state, and the active generation.
type SemanticReplayView struct {
	Mode         semantic.Mode           `json:"mode"`
	Fusion       contextstore.FusionMode `json:"fusion"`
	State        string                  `json:"state"`
	Failure      string                  `json:"failure,omitempty"`
	Model        string                  `json:"model"`
	GenerationID string                  `json:"generation_id,omitempty"`
}

func (s *SemanticReplay) view(mode semantic.Mode, fusion contextstore.FusionMode) SemanticReplayView {
	status := s.Status()
	_, generationID, _ := s.SemanticTraceIdentity()
	return SemanticReplayView{Mode: mode, Fusion: fusion, State: status.State, Failure: status.Failure, Model: status.Model, GenerationID: generationID}
}
