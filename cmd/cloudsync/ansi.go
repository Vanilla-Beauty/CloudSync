package main

import (
	"fmt"
	"strings"
)

// ansi.go — shared ANSI/VT escape sequence constants and helpers for TUI code.
//
// Why not an external library (e.g. chalk)?
//   Chalk covers only text colors/styles; it has no support for cursor
//   positioning, clear-screen/line, or cursor visibility — all of which our
//   TUI needs.  A single shared file gives us semantic names for the full set
//   of sequences without adding a dependency.

// ── Text styles ───────────────────────────────────────────────────────────────

const (
	ansiReset    = "\033[0m"
	ansiBold     = "\033[1m"
	ansiReverse  = "\033[7m"  // reverse-video — used for the selected row
	ansiGray     = "\033[90m" // bright black (dark gray)
	ansiBlue     = "\033[34m"
	ansiGreen    = "\033[32m"
	ansiYellow   = "\033[33m"
)

// ── Cursor & screen control ───────────────────────────────────────────────────

const (
	ansiClearLine   = "\033[2K"
	ansiClearScreen = "\033[2J\033[H"
	ansiHideCursor  = "\033[?25l"
	ansiShowCursor  = "\033[?25h"
)

// ansiMoveTo returns a VT sequence that moves the cursor to (row, col),
// both 1-based, matching the convention used by terminal emulators.
func ansiMoveTo(row, col int) string {
	return fmt.Sprintf("\033[%d;%dH", row, col)
}

// ── Semantic aliases ──────────────────────────────────────────────────────────
// These names describe intent rather than raw colour values, making call-sites
// in browser.go and mountbrowser.go self-documenting.

const (
	ansiSelected  = ansiReverse  // highlighted / cursor row
	ansiDirColor  = ansiBlue     // directory entries in the remote browser
	ansiMountRow  = ansiGreen    // mount entries in the local mount browser
	ansiMeta      = ansiGray     // secondary / metadata text
	ansiWarning   = ansiYellow   // confirmations, status warnings
	ansiSeparator = ansiGray     // horizontal rule
)

// ── visibleLen ────────────────────────────────────────────────────────────────

// visibleLen returns the number of visible (non-ANSI) rune positions in s.
// It handles the subset of ANSI sequences used here: ESC [ ... m  (SGR only).
func visibleLen(s string) int {
	n := 0
	inEsc := false
	for _, c := range s {
		if inEsc {
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		if c == '\033' {
			inEsc = true
			continue
		}
		n++
	}
	return n
}

// ── padRight ──────────────────────────────────────────────────────────────────

// padRight writes s to sb, padded or truncated to exactly w visible columns.
func padRight(sb *strings.Builder, s string, w int) {
	visible := visibleLen(s)
	if visible > w {
		sb.WriteString(s[:w]) // approximate: fine for ASCII-only paths
	} else {
		sb.WriteString(s)
		for i := visible; i < w; i++ {
			sb.WriteByte(' ')
		}
	}
}
