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
