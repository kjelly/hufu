package tui

// Overlay is the kind of modal / non-column view currently active on top
// of the column dashboard. The View() and Update() priority chains
// dispatch on this enum; the underlying 8 booleans (inAskUser, inHelp,
// etc.) remain as the source of truth so existing code that reads
// them continues to work.
//
// The numeric ordering is significant: lower-numbered overlays are
// drawn and updated first (top of the stack). When the model would
// otherwise have multiple overlays active, currentOverlay returns
// the highest-priority one.
type Overlay int

const (
	OverlayNone Overlay = iota
	OverlayAskUser
	OverlayHelp
	OverlayInfo
	OverlaySearch
	OverlayPromptInput
	OverlayConfirm
	OverlayDetail
	OverlayMemory
	OverlayActivityLog
)

// String returns a stable, lowercase name for each overlay. Useful for
// logging and tests.
func (o Overlay) String() string {
	switch o {
	case OverlayNone:
		return "none"
	case OverlayAskUser:
		return "ask_user"
	case OverlayHelp:
		return "help"
	case OverlayInfo:
		return "info"
	case OverlaySearch:
		return "search"
	case OverlayPromptInput:
		return "prompt_input"
	case OverlayConfirm:
		return "confirm"
	case OverlayDetail:
		return "detail"
	case OverlayMemory:
		return "memory"
	case OverlayActivityLog:
		return "activity_log"
	}
	return "unknown"
}

// currentOverlay returns the highest-priority overlay currently active
// on the model. Priority order matches the View() chain in tui.go.
// Only one overlay is "active" at a time even if multiple bools
// happen to be set (e.g. due to ordering bugs); the higher-priority
// overlay wins.
func (m *Model) currentOverlay() Overlay {
	switch {
	case m.inAskUser:
		return OverlayAskUser
	case m.inHelp:
		return OverlayHelp
	case m.inInfo:
		return OverlayInfo
	case m.inSearch:
		return OverlaySearch
	case m.inPromptInput:
		return OverlayPromptInput
	case m.inConfirm:
		return OverlayConfirm
	case m.inDetail:
		return OverlayDetail
	case m.inMemory:
		return OverlayMemory
	case m.inActivityLog:
		return OverlayActivityLog
	}
	return OverlayNone
}

// setOverlay activates the given overlay. It is mutually exclusive:
// activating one overlay clears all the others. Returns the
// previous overlay so callers can chain actions (e.g. setOverlay
// returns the overlay that was active before).
//
// The 8 underlying bools are kept as the source of truth so that
// existing per-overlay setup/teardown code (which reads/writes the
// bools directly) still works.
func (m *Model) setOverlay(o Overlay) Overlay {
	prev := m.currentOverlay()
	if prev == o {
		return prev
	}
	m.clearAllOverlays()
	switch o {
	case OverlayAskUser:
		m.inAskUser = true
	case OverlayHelp:
		m.inHelp = true
	case OverlayInfo:
		m.inInfo = true
	case OverlaySearch:
		m.inSearch = true
	case OverlayPromptInput:
		m.inPromptInput = true
	case OverlayConfirm:
		m.inConfirm = true
	case OverlayDetail:
		m.inDetail = true
	case OverlayMemory:
		m.inMemory = true
	case OverlayActivityLog:
		m.inActivityLog = true
	case OverlayNone:
		// no-op; bools are already cleared
	default:
		panic("tui: unknown overlay " + o.String())
	}
	return prev
}

// clearAllOverlays sets every overlay bool to false. Used by
// setOverlay to enforce mutual exclusion.
func (m *Model) clearAllOverlays() {
	m.inAskUser = false
	m.inHelp = false
	m.inInfo = false
	m.inSearch = false
	m.inPromptInput = false
	m.inConfirm = false
	m.inDetail = false
	m.inMemory = false
	m.inActivityLog = false
}
