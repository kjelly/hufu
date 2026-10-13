package team

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/embedding"
)

const (
	semanticTraceFile    = "semantic-retrieval-traces.jsonl"
	semanticTraceKeyFile = "semantic-trace.key"
)

// semanticTraceOptions returns the content-free trace hasher and JSONL sink
// for semantic retrieval. Traces hold only HMAC-keyed IDs, counts, model and
// generation identity, and the fallback reason; never query or memory text.
// The HMAC key stays in the workspace so hashes are comparable across runs
// of one workspace but meaningless outside it.
func (c *Coordinator) semanticTraceOptions() (contextstore.TraceHasher, func(contextstore.SemanticRetrievalTrace), error) {
	if c == nil || c.session == nil || c.session.Workspace == "" {
		return nil, nil, nil
	}
	key, err := loadOrCreateSemanticTraceKey(c.session.Workspace)
	if err != nil {
		return nil, nil, err
	}
	hasher, err := contextstore.NewHMACTraceHasher(key)
	if err != nil {
		return nil, nil, err
	}
	path := filepath.Join(c.session.Workspace, semanticTraceFile)
	sink := func(trace contextstore.SemanticRetrievalTrace) {
		data, marshalErr := json.Marshal(trace)
		if marshalErr != nil {
			return
		}
		shadowTraceMu.Lock()
		defer shadowTraceMu.Unlock()
		f, openErr := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if openErr != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.Write(append(data, '\n'))
	}
	return hasher, sink, nil
}

func loadOrCreateSemanticTraceKey(workspace string) ([]byte, error) {
	path := filepath.Join(workspace, semanticTraceKeyFile)
	if data, err := os.ReadFile(path); err == nil {
		key, decodeErr := hex.DecodeString(strings.TrimSpace(string(data)))
		if decodeErr != nil || len(key) < 32 {
			return nil, fmt.Errorf("semantic trace key %s is malformed", path)
		}
		return key, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("read semantic trace key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("create semantic trace key: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, fs.ErrExist) {
		// Another process created it first; use theirs.
		return loadOrCreateSemanticTraceKey(workspace)
	}
	if err != nil {
		return nil, fmt.Errorf("create semantic trace key: %w", err)
	}
	defer func() { _ = f.Close() }()
	if _, err = f.WriteString(hex.EncodeToString(key) + "\n"); err != nil {
		return nil, fmt.Errorf("write semantic trace key: %w", err)
	}
	return key, nil
}

// Sources recorded in semantic traces: the ranking that issued the query.
const (
	semanticSourceSharedPersistent = "shared_persistent"
	semanticSourceWorkerPrivate    = "worker_private"
)

// SemanticTraceAttribution links a content-free semantic trace to the run,
// task, and context request that issued it.
type SemanticTraceAttribution struct {
	RunID            string
	TaskID           string
	Attempt          int
	ContextRequestID string
	Phase            string
	Trigger          string
	Role             string
	Agent            string
	Source           string
}

// semanticTraceAttributionFor attributes a ranking run for request.
func semanticTraceAttributionFor(request ContextRequest, source string) SemanticTraceAttribution {
	return SemanticTraceAttribution{
		RunID: request.RunID, TaskID: request.TaskID, Attempt: request.Attempt, ContextRequestID: request.RequestID,
		Phase: string(request.Phase), Trigger: string(request.Trigger), Role: request.AgentRole, Agent: request.AgentName,
		Source: source,
	}
}

func (a SemanticTraceAttribution) apply(trace *contextstore.SemanticRetrievalTrace, route string, share float64) {
	trace.RunID, trace.TaskID, trace.Attempt, trace.ContextRequestID = a.RunID, a.TaskID, a.Attempt, a.ContextRequestID
	trace.Phase, trace.Trigger, trace.Role, trace.Agent, trace.Source = a.Phase, a.Trigger, a.Role, a.Agent, a.Source
	trace.Route, trace.RouteShare = route, roundRouteShare(share)
}

// roundRouteShare keeps two decimals: enough to calibrate the auto threshold
// without fingerprinting the text.
func roundRouteShare(share float64) float64 {
	return float64(int(share*100+0.5)) / 100
}

// attributedTraceSink stamps every trace of one request with its attribution
// and route before writing it.
func attributedTraceSink(sink func(contextstore.SemanticRetrievalTrace), attribution SemanticTraceAttribution, text string) func(contextstore.SemanticRetrievalTrace) {
	if sink == nil {
		return nil
	}
	share := embedding.LexicalDropShare(text)
	return func(trace contextstore.SemanticRetrievalTrace) {
		attribution.apply(&trace, contextstore.SemanticRouteSemantic, share)
		sink(trace)
	}
}

// writeLexicalRouteTrace records a request auto routing kept lexical, so the
// trace file shows every routing decision, not only the embedded ones.
func writeLexicalRouteTrace(hasher contextstore.TraceHasher, sink func(contextstore.SemanticRetrievalTrace), attribution SemanticTraceAttribution, query, text string) {
	if hasher == nil || sink == nil {
		return
	}
	trace, err := contextstore.NewLexicalRouteTrace(hasher, query)
	if err != nil {
		return
	}
	attribution.apply(&trace, contextstore.SemanticRouteLexical, embedding.LexicalDropShare(text))
	sink(trace)
}
