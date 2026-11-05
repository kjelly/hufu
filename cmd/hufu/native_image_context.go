package main

import (
	"bytes"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Context files remain UTF-8 text. Sniffing binary file signatures does not
// interpret task prose or grant image access; native ingress owns that grant.
func binaryContextContent(data []byte) bool {
	return !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 || !strings.HasPrefix(http.DetectContentType(data), "text/")
}
