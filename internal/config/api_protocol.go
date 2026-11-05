package config

import (
	"fmt"
	"strings"
)

const (
	APIProtocolChatCompletions = "chat-completions"
	APIProtocolResponses       = "responses"
)

// NormalizeAPIProtocol preserves the empty legacy value. Callers must not
// replace it with explicit Chat before computing configuration identity.
func NormalizeAPIProtocol(raw string) (string, error) {
	protocol := strings.ToLower(strings.TrimSpace(raw))
	switch protocol {
	case "", APIProtocolChatCompletions, APIProtocolResponses:
		return protocol, nil
	default:
		return "", fmt.Errorf("unsupported api-protocol %q (want chat-completions or responses)", raw)
	}
}
