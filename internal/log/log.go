// Package log provides a small, centralized stderr logger that respects the
// CLI's --quiet and --json flags.
//
// Why a dedicated package? Many code paths scattered across `cmd/hufu` and the
// internal/* packages used to write directly to `os.Stderr` with
// `fmt.Fprintf(os.Stderr, ...)`, which bypasses the quiet/JSON settings and
// the TUI active-screen check. Funnelling those writes through `log.Print` /
// `log.Printf` keeps user-facing chatter suppressed in script mode and
// prevents status lines from garbling the Bubble Tea altscreen.
package log

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
)

// quiet is the master switch; set via SetQuiet. When true, Print/Printf
// become no-ops. JSON output mode also sets this.
var quiet atomic.Bool

// tuiActive prevents status writes from garbling the TUI altscreen.
var tuiActive atomic.Bool

// SetQuiet toggles suppression of stderr output. JSON output mode should
// also call this with true.
func SetQuiet(v bool) { quiet.Store(v) }

// SetTUIActive tells the logger whether the Bubble Tea TUI is currently
// drawing. While active, Print/Printf become no-ops to avoid garbling
// the altscreen with progress lines.
func SetTUIActive(v bool) { tuiActive.Store(v) }

// IsQuiet returns the current quiet state.
func IsQuiet() bool { return quiet.Load() }

// Writer returns the underlying stderr writer. The Print functions always
// pass through this; tests can swap it via SetWriter.
var writer atomic.Pointer[io.Writer]

func init() {
	w := io.Writer(os.Stderr)
	writer.Store(&w)
}

// SetWriter replaces the destination of Print/Printf. Primarily useful in
// tests; pass nil to restore the default (os.Stderr).
func SetWriter(w io.Writer) {
	if w == nil {
		stderr := io.Writer(os.Stderr)
		writer.Store(&stderr)
		return
	}
	writer.Store(&w)
}

// Print writes to stderr unless the logger is muted by --quiet, --json,
// or an active TUI. It mirrors log.Print's variadic signature but does
// NOT append a newline.
func Print(args ...any) {
	if quiet.Load() || tuiActive.Load() {
		return
	}
	w := writer.Load()
	if w == nil {
		return
	}
	_, _ = fmt.Fprint(*w, args...)
}

// Printf writes a formatted line to stderr unless muted. It does NOT
// append a newline; include \n in the format string if needed.
func Printf(format string, args ...any) {
	if quiet.Load() || tuiActive.Load() {
		return
	}
	w := writer.Load()
	if w == nil {
		return
	}
	_, _ = fmt.Fprintf(*w, format, args...)
}

// Println writes args to stderr separated by spaces and followed by a newline,
// unless muted.
func Println(args ...any) {
	if quiet.Load() || tuiActive.Load() {
		return
	}
	w := writer.Load()
	if w == nil {
		return
	}
	_, _ = fmt.Fprintln(*w, args...)
}
