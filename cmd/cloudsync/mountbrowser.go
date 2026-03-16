package main

// Interactive TUI for browsing active local mounts.
//
// Keys:
//   ↑/k   move up
//   ↓/j   move down
//   u      unmount (confirm with u, cancel with Esc)
//   r      browse remote for selected mount (opens ls-remote TUI inline)
//   q/Esc  quit

import (
	"fmt"
	"os"
	"strings"

	"github.com/cloudsync/cloudsync/internal/apiclient"
	"github.com/cloudsync/cloudsync/internal/ipc"
	"golang.org/x/term"
)

// ── mode ─────────────────────────────────────────────────────────────────────

type mountBrowserMode int

const (
	mbModeNormal        mountBrowserMode = iota
	mbModeUnmountConfirm                 // waiting for 'u' confirm or Esc cancel
)

// ── mount browser ─────────────────────────────────────────────────────────────

type mountBrowser struct {
	apiClient *apiclient.Client
	mounts    []ipc.MountRecord

	cursor int
	offset int
	height int
	width  int

	mode        mountBrowserMode
	pendingIdx  int    // index targeted by current u operation
	status      string // status bar message
	actionMount *ipc.MountRecord // set when user requests remote browse (exit signal)
}

func newMountBrowser(client *apiclient.Client, mounts []ipc.MountRecord) *mountBrowser {
	return &mountBrowser{
		apiClient:  client,
		mounts:     mounts,
		pendingIdx: -1,
	}
}

func (b *mountBrowser) selectedMount() *ipc.MountRecord {
	if len(b.mounts) == 0 || b.cursor < 0 || b.cursor >= len(b.mounts) {
		return nil
	}
	return &b.mounts[b.cursor]
}

func (b *mountBrowser) moveUp() {
	if b.cursor > 0 {
		b.cursor--
	}
}

func (b *mountBrowser) moveDown() {
	if b.cursor < len(b.mounts)-1 {
		b.cursor++
	}
}

func (b *mountBrowser) clampCursor() {
	if b.cursor >= len(b.mounts) {
		b.cursor = len(b.mounts) - 1
	}
	if b.cursor < 0 {
		b.cursor = 0
	}
}

// executeUnmount calls the daemon to remove the mount at pendingIdx.
func (b *mountBrowser) executeUnmount() {
	idx := b.pendingIdx
	b.pendingIdx = -1
	b.mode = mbModeNormal

	if idx < 0 || idx >= len(b.mounts) {
		return
	}
	m := b.mounts[idx]
	if err := b.apiClient.RemoveMount(m.LocalPath, false); err != nil {
		b.status = "Unmount error: " + err.Error()
		return
	}
	// Remove from local slice
	b.mounts = append(b.mounts[:idx], b.mounts[idx+1:]...)
	b.clampCursor()
	b.status = "Unmounted: " + m.LocalPath
}

// ── rendering ─────────────────────────────────────────────────────────────────

const (
	mbColorReset  = "\033[0m"
	mbColorBold   = "\033[1m"
	mbColorHiLine = "\033[7m"
	mbColorGray   = "\033[90m"
	mbColorYellow = "\033[33m"
	mbColorGreen  = "\033[32m"
	mbClearLine   = "\033[2K"
	mbClearScreen = "\033[2J\033[H"
	mbHideCursor  = "\033[?25l"
	mbShowCursor  = "\033[?25h"
)

func mbMoveTo(row, col int) string {
	return fmt.Sprintf("\033[%d;%dH", row, col)
}

func (b *mountBrowser) render() {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		w, h = 80, 24
	}
	b.width = w
	b.height = h - 3 // title + separator + status bar

	// Adjust scroll
	if b.cursor < b.offset {
		b.offset = b.cursor
	}
	if b.cursor >= b.offset+b.height {
		b.offset = b.cursor - b.height + 1
	}

	var sb strings.Builder
	sb.WriteString(mbHideCursor)
	sb.WriteString(mbClearScreen)

	// Title bar
	title := fmt.Sprintf(" CloudSync Mounts  (%d active)", len(b.mounts))
	sb.WriteString(mbMoveTo(1, 1))
	sb.WriteString(mbColorBold)
	mbPadRight(&sb, title, w)
	sb.WriteString(mbColorReset)

	// Content rows — two lines per mount entry
	rowsPerEntry := 2
	visibleEntries := b.height / rowsPerEntry
	if visibleEntries < 1 {
		visibleEntries = 1
	}

	if b.cursor < b.offset {
		b.offset = b.cursor
	}
	if b.cursor >= b.offset+visibleEntries {
		b.offset = b.cursor - visibleEntries + 1
	}

	screenRow := 2
	for i := b.offset; i < len(b.mounts) && screenRow < h-1; i++ {
		m := b.mounts[i]
		selected := i == b.cursor

		bucketStr := m.Bucket
		if bucketStr == "" {
			bucketStr = "(default)"
		}
		addedStr := m.AddedAt.Local().Format("2006-01-02 15:04")

		line1 := fmt.Sprintf("  %-42s → %-26s", truncateStr(m.LocalPath, 42), truncateStr(m.RemotePrefix, 26))
		line2 := fmt.Sprintf("    bucket: %-28s  id: %-10s  added: %s", bucketStr, m.ID, addedStr)

		sb.WriteString(mbMoveTo(screenRow, 1))
		if selected {
			sb.WriteString(mbColorHiLine)
		} else {
			sb.WriteString(mbColorGreen)
		}
		mbPadRight(&sb, line1, w)
		sb.WriteString(mbColorReset)

		screenRow++
		if screenRow >= h-1 {
			break
		}

		sb.WriteString(mbMoveTo(screenRow, 1))
		if selected {
			sb.WriteString(mbColorHiLine)
		} else {
			sb.WriteString(mbColorGray)
		}
		mbPadRight(&sb, line2, w)
		sb.WriteString(mbColorReset)

		screenRow++
	}

	// Clear remaining rows
	for screenRow < h-1 {
		sb.WriteString(mbMoveTo(screenRow, 1))
		sb.WriteString(mbClearLine)
		screenRow++
	}

	// Separator
	sb.WriteString(mbMoveTo(h-1, 1))
	sb.WriteString(mbColorGray)
	sb.WriteString(strings.Repeat("─", w))
	sb.WriteString(mbColorReset)

	// Status / help bar
	sb.WriteString(mbMoveTo(h, 1))
	switch b.mode {
	case mbModeUnmountConfirm:
		name := ""
		if b.pendingIdx >= 0 && b.pendingIdx < len(b.mounts) {
			name = b.mounts[b.pendingIdx].LocalPath
		}
		line := mbColorYellow + " Unmount \"" + name + "\"? Press u to confirm, Esc to cancel" + mbColorReset
		sb.WriteString(line)
	default:
		help := " ↑↓/jk move   u unmount   r browse remote   q quit"
		if b.status != "" {
			help = mbColorYellow + " " + b.status + mbColorReset
		}
		sb.WriteString(help)
	}

	fmt.Fprint(os.Stdout, sb.String())
}

func mbPadRight(sb *strings.Builder, s string, w int) {
	visible := mbVisibleLen(s)
	if visible > w {
		sb.WriteString(s[:w])
	} else {
		sb.WriteString(s)
		for i := visible; i < w; i++ {
			sb.WriteByte(' ')
		}
	}
}

func mbVisibleLen(s string) int {
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

func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return "…" + s[len(s)-(max-1):]
}

// ── run ───────────────────────────────────────────────────────────────────────

// run starts the TUI event loop. Returns a *ipc.MountRecord if the user
// pressed 'r' to browse a mount remotely (caller should open ls-remote).
func (b *mountBrowser) run() (*ipc.MountRecord, error) {
	enableVTMode()

	if len(b.mounts) == 0 {
		return nil, nil // nothing to show; caller prints message
	}

	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return nil, fmt.Errorf("raw mode: %w", err)
	}
	defer func() {
		_ = term.Restore(fd, oldState)
		fmt.Print(mbShowCursor)
	}()

	b.render()

	type keyMsg struct {
		data []byte
		err  error
	}
	keyCh := make(chan keyMsg, 1)
	readNext := func() {
		buf := make([]byte, 8)
		n, err := os.Stdin.Read(buf)
		if err != nil {
			keyCh <- keyMsg{err: err}
			return
		}
		keyCh <- keyMsg{data: buf[:n]}
	}
	go readNext()

	for {
		msg := <-keyCh
		if msg.err != nil {
			return nil, nil
		}
		key := msg.data

		switch b.mode {
		case mbModeUnmountConfirm:
			switch {
			case mbIsKey(key, 'u'):
				b.executeUnmount()
			case mbIsKey(key, 27), mbIsKey(key, 'q'):
				b.pendingIdx = -1
				b.mode = mbModeNormal
				b.status = "Cancelled"
			}

		default: // mbModeNormal
			switch {
			case mbIsKey(key, 'q'), mbIsKey(key, 27):
				return nil, nil
			case mbIsKey(key, 'j'), mbIsEscape(key, 'B'):
				b.moveDown()
				b.status = ""
			case mbIsKey(key, 'k'), mbIsEscape(key, 'A'):
				b.moveUp()
				b.status = ""
			case mbIsKey(key, 'u'):
				if sel := b.selectedMount(); sel != nil {
					b.pendingIdx = b.cursor
					b.mode = mbModeUnmountConfirm
					b.status = ""
				}
			case mbIsKey(key, 'r'), mbIsKey(key, 13):
				if sel := b.selectedMount(); sel != nil {
					// Copy so we can return after restoring terminal
					rec := *sel
					return &rec, nil
				}
			}
		}

		b.render()
		go readNext()
	}
}

// ── key helpers ───────────────────────────────────────────────────────────────

func mbIsKey(buf []byte, b byte) bool {
	return len(buf) == 1 && buf[0] == b
}

func mbIsEscape(buf []byte, code byte) bool {
	return len(buf) == 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == code
}
