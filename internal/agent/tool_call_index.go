package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// withToolCallIndexes returns a copy of client whose streamed chat
// completions give every distinct tool call its own index. The OpenAI stream
// protocol identifies a tool call by its index, and fantasy merges deltas that
// share one. Ollama 0.35.0 streams parallel tool calls in one chunk that all
// carry index 0 with distinct ids, so the second call's arguments were
// appended to the first call, its name was lost, and the tool rejected the
// concatenated JSON. Streams that already give each call its own index pass
// through unchanged, so this follows the protocol rather than one vendor.
func withToolCallIndexes(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{Transport: toolCallIndexTransport{base: http.DefaultTransport}}
	}
	wrapped := *client
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	wrapped.Transport = toolCallIndexTransport{base: base}
	return &wrapped
}

type toolCallIndexTransport struct {
	base http.RoundTripper
}

func (t toolCallIndexTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || resp == nil || resp.Body == nil || !isEventStream(resp.Header) {
		return resp, err
	}
	resp.Body = &toolCallIndexBody{source: resp.Body, reader: bufio.NewReader(resp.Body), choices: map[int64]*toolCallIndexes{}}
	resp.ContentLength = -1
	resp.Header.Del("Content-Length")
	return resp, nil
}

func isEventStream(header http.Header) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(header.Get("Content-Type"))), "text/event-stream")
}

// toolCallIndexBody rewrites the SSE data lines of one stream. Lines without
// tool calls, and any line it cannot parse, are passed through byte for byte.
type toolCallIndexBody struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	pending []byte
	err     error
	choices map[int64]*toolCallIndexes
}

func (b *toolCallIndexBody) Read(p []byte) (int, error) {
	for len(b.pending) == 0 {
		if b.err != nil {
			return 0, b.err
		}
		line, err := b.reader.ReadBytes('\n')
		b.pending = b.rewriteLine(line)
		b.err = err
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *toolCallIndexBody) Close() error {
	return b.source.Close()
}

func (b *toolCallIndexBody) rewriteLine(line []byte) []byte {
	content := bytes.TrimRight(line, "\r\n")
	payload, ok := bytes.CutPrefix(content, []byte("data:"))
	if !ok || !bytes.Contains(payload, []byte(`"tool_calls"`)) {
		return line
	}
	prefix := len(content) - len(payload)
	if len(payload) > 0 && payload[0] == ' ' {
		prefix++
	}
	rewritten, changed := b.renumber(content[prefix:])
	if !changed {
		return line
	}
	out := make([]byte, 0, prefix+len(rewritten)+len(line)-len(content))
	out = append(out, content[:prefix]...)
	out = append(out, rewritten...)
	return append(out, line[len(content):]...)
}

func (b *toolCallIndexBody) renumber(data []byte) ([]byte, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var chunk map[string]any
	if decoder.Decode(&chunk) != nil || decoder.More() {
		return nil, false
	}
	choices, _ := chunk["choices"].([]any)
	changed := false
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		calls, _ := delta["tool_calls"].([]any)
		if len(calls) == 0 {
			continue
		}
		choiceIndex := jsonInt64(choice["index"])
		state := b.choices[choiceIndex]
		if state == nil {
			state = &toolCallIndexes{byID: map[string]int64{}, byUpstream: map[int64]int64{}}
			b.choices[choiceIndex] = state
		}
		for _, rawCall := range calls {
			call, ok := rawCall.(map[string]any)
			if !ok {
				continue
			}
			id, _ := call["id"].(string)
			upstream := jsonInt64(call["index"])
			assigned := state.assign(id, upstream)
			if _, present := call["index"]; !present || assigned != upstream {
				call["index"] = assigned
				changed = true
			}
		}
	}
	if !changed {
		return nil, false
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(chunk) != nil {
		return nil, false
	}
	return bytes.TrimRight(out.Bytes(), "\n"), true
}

// toolCallIndexes assigns indexes for one choice of one stream. A delta with
// an id names its call; a delta without one continues the latest call that
// used its upstream index, or starts a new call when none did.
type toolCallIndexes struct {
	byID       map[string]int64
	byUpstream map[int64]int64
	next       int64
}

func (s *toolCallIndexes) assign(id string, upstream int64) int64 {
	if id != "" {
		if index, ok := s.byID[id]; ok {
			s.byUpstream[upstream] = index
			return index
		}
		index := s.next
		s.next++
		s.byID[id] = index
		s.byUpstream[upstream] = index
		return index
	}
	if index, ok := s.byUpstream[upstream]; ok {
		return index
	}
	index := s.next
	s.next++
	s.byUpstream[upstream] = index
	return index
}

func jsonInt64(value any) int64 {
	number, ok := value.(json.Number)
	if !ok {
		return 0
	}
	index, err := number.Int64()
	if err != nil {
		return 0
	}
	return index
}
