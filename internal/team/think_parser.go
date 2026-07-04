package team

import "strings"

// ThinkParser parses streaming text chunks, extracting blocks bounded by <think>...</think>.
// Text inside the blocks is routed to onReasoning; text outside is routed to onText.
type ThinkParser struct {
	inThink bool
	buf     string
}

func (p *ThinkParser) Process(chunk string, onText func(string), onReasoning func(string)) {
	p.buf += chunk

	for {
		if !p.inThink {
			idx := strings.Index(p.buf, "<think>")
			if idx >= 0 {
				if idx > 0 {
					onText(p.buf[:idx])
				}
				p.inThink = true
				p.buf = p.buf[idx+7:] // len("<think>")
			} else {
				// Prevent partial flush of "<think>" tag boundaries
				flushIdx := len(p.buf)
				for i := 1; i <= 6 && i <= len(p.buf); i++ {
					if strings.HasPrefix("<think>", p.buf[len(p.buf)-i:]) {
						flushIdx = len(p.buf) - i
						break
					}
				}
				if flushIdx > 0 {
					onText(p.buf[:flushIdx])
					p.buf = p.buf[flushIdx:]
				}
				break
			}
		} else {
			idx := strings.Index(p.buf, "</think>")
			if idx >= 0 {
				if idx > 0 {
					onReasoning(p.buf[:idx])
				}
				p.inThink = false
				p.buf = p.buf[idx+8:] // len("</think>")
			} else {
				// Prevent partial flush of "</think>" tag boundaries
				flushIdx := len(p.buf)
				for i := 1; i <= 7 && i <= len(p.buf); i++ {
					if strings.HasPrefix("</think>", p.buf[len(p.buf)-i:]) {
						flushIdx = len(p.buf) - i
						break
					}
				}
				if flushIdx > 0 {
					onReasoning(p.buf[:flushIdx])
					p.buf = p.buf[flushIdx:]
				}
				break
			}
		}
	}
}

func (p *ThinkParser) Flush(onText func(string), onReasoning func(string)) {
	if p.buf != "" {
		if p.inThink {
			onReasoning(p.buf)
		} else {
			onText(p.buf)
		}
		p.buf = ""
	}
}
