package team

import (
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/semantic"
)

// semanticRouter is auto mode's per-request decision. The runtime implements
// it so every decision is counted in its status.
type semanticRouter interface {
	RouteSemantic(text string) bool
}

// semanticRequestRoute is the retrieval one request runs with: the lane mode
// (off means lexical only, no embedding request) and the fusion.
type semanticRequestRoute struct {
	Mode   contextstore.RetrievalMode
	Fusion contextstore.FusionMode
}

// resolveSemanticRoute applies auto routing and the semantic fusion. A
// request routed lexical runs exactly as mode off with baseFusion; shadow
// keeps baseFusion so its selection stays identical to off; active fuses with
// laneFusion when one is set.
func resolveSemanticRoute(mode contextstore.RetrievalMode, router semanticRouter, laneFusion, baseFusion contextstore.FusionMode, text string) semanticRequestRoute {
	if router != nil && !router.RouteSemantic(text) {
		return semanticRequestRoute{Mode: contextstore.RetrievalOff, Fusion: baseFusion}
	}
	if mode == contextstore.RetrievalActive && laneFusion != "" {
		return semanticRequestRoute{Mode: mode, Fusion: laneFusion}
	}
	return semanticRequestRoute{Mode: mode, Fusion: baseFusion}
}

// RouteSemantic decides whether one auto-mode request uses the semantic lane
// and counts the decision.
func (r *SemanticRetrievalRuntime) RouteSemantic(text string) bool {
	routed := semantic.RoutesSemantic(text)
	r.mu.Lock()
	defer r.mu.Unlock()
	if routed {
		r.stats.RoutedSemantic++
	} else {
		r.stats.RoutedLexical++
	}
	return routed
}

// router is the runtime as a semanticRouter in auto mode, nil otherwise.
func (r *SemanticRetrievalRuntime) router() semanticRouter {
	if r == nil || r.settings.Mode != semantic.ModeAuto {
		return nil
	}
	return r
}

// Fusion is how requests that use the semantic lane fuse it.
func (r *SemanticRetrievalRuntime) Fusion() contextstore.FusionMode {
	if r == nil {
		return ""
	}
	return contextstore.FusionMode(r.settings.Fusion)
}
