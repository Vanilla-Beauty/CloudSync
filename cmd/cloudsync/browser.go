package main

// Interactive TUI for browsing a COS bucket, similar to lazygit.
//
// Keys:
//   ↑/k   move up
//   ↓/j   move down
//   Enter  expand directory / no-op on file
//   ←/h   collapse / go to parent
//   q/Esc  quit

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cloudsync/cloudsync/internal/storage"
	"golang.org/x/term"
)

// ── tree node ────────────────────────────────────────────────────────────────

type node struct {
	entry    storage.DirEntry
	depth    int
	expanded bool
	children []*node // nil = not loaded yet; empty slice = loaded & empty
}

func (n *node) label() string {
	if n.entry.IsDir {
		name := dirName(n.entry.Prefix)
		if n.expanded {
			return "▼ " + name + "/"
		}
		return "▶ " + name + "/"
	}
	name := fileName(n.entry.Key)
	size := formatSize(n.entry.Size)
	mod := n.entry.LastModified
	if len(mod) > 10 {
		mod = mod[:10]
	}
	return fmt.Sprintf("  %-40s %8s  %s", name, size, mod)
}

// ── browser state ────────────────────────────────────────────────────────────

type browser struct {
	cos     *storage.COSClient
	root    []*node // top-level entries
	flat    []*node // flattened visible list
	cursor  int
	offset  int // scroll offset
	height  int // terminal rows available for content
	width   int
	bucket  string
	prefix  string
	status  string // status bar message
}

func newBrowser(cos *storage.COSClient, bucket, prefix string) *browser {
	return &browser{cos: cos, bucket: bucket, prefix: prefix}
}

func (b *browser) load(ctx context.Context) error {
	entries, err := b.cos.ListDir(ctx, b.prefix)
	if err != nil {
		return err
	}
	b.root = entriesToNodes(entries, 0)
	b.rebuild()
	return nil
}

func (b *browser) rebuild() {
	b.flat = b.flat[:0]
	b.flattenInto(b.root)
}

func (b *browser) flattenInto(nodes []*node) {
	for _, n := range nodes {
		b.flat = append(b.flat, n)
		if n.expanded && n.children != nil {
			b.flattenInto(n.children)
		}
	}
}

// expand loads children of the node at the cursor if not yet loaded.
func (b *browser) expand(ctx context.Context) {
	if len(b.flat) == 0 {
		return
	}
	n := b.flat[b.cursor]
	if !n.entry.IsDir {
		return
	}
	if !n.expanded {
		// Load children lazily
		if n.children == nil {
			b.status = "Loading..."
			b.render()
			entries, err := b.cos.ListDir(ctx, n.entry.Prefix)
			if err != nil {
				b.status = "Error: " + err.Error()
				return
			}
			n.children = entriesToNodes(entries, n.depth+1)
		}
		n.expanded = true
		b.status = ""
	}
	b.rebuild()
}

func (b *browser) collapse() {
	if len(b.flat) == 0 {
		return
	}
	n := b.flat[b.cursor]
	if n.entry.IsDir && n.expanded {
		n.expanded = false
		b.rebuild()
		return
	}
	// If already collapsed or on a file, jump to parent directory
	if n.depth == 0 {
		return
	}
	targetDepth := n.depth - 1
	for i := b.cursor - 1; i >= 0; i-- {
		if b.flat[i].depth == targetDepth && b.flat[i].entry.IsDir {
			b.flat[i].expanded = false
			b.cursor = i
			b.rebuild()
			b.clampCursor()
			return
		}
	}
}

func (b *browser) moveUp() {
	if b.cursor > 0 {
		b.cursor--
	}
}

func (b *browser) moveDown() {
	if b.cursor < len(b.flat)-1 {
		b.cursor++
	}
}

func (b *browser) clampCursor() {
	if b.cursor >= len(b.flat) {
		b.cursor = len(b.flat) - 1
	}
	if b.cursor < 0 {
		b.cursor = 0
	}
}

// ── rendering ────────────────────────────────────────────────────────────────

const (
	colorReset  = "\033[0m"
	colorBold   = "\033[1m"
	colorHiLine = "\033[7m" // reverse video for selected row
	colorDir    = "\033[34m" // blue
	colorGray   = "\033[90m"
	colorYellow = "\033[33m"

	clearLine   = "\033[2K"
	clearScreen = "\033[2J\033[H"
	hideCursor  = "\033[?25l"
	showCursor  = "\033[?25h"
)

func moveTo(row, col int) string {
	return fmt.Sprintf("\033[%d;%dH", row, col)
}

func (b *browser) render() {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		w, h = 80, 24
	}
	b.width = w
	b.height = h - 3 // title + status bar + border

	// Adjust scroll so cursor stays visible
	if b.cursor < b.offset {
		b.offset = b.cursor
	}
	if b.cursor >= b.offset+b.height {
		b.offset = b.cursor - b.height + 1
	}

	var sb strings.Builder
	sb.WriteString(hideCursor)
	sb.WriteString(clearScreen)

	// Title bar
	title := fmt.Sprintf(" COS  %s%s  (%d items)", b.bucket, "/"+b.prefix, len(b.flat))
	sb.WriteString(moveTo(1, 1))
	sb.WriteString(colorBold)
	padRight(&sb, title, w)
	sb.WriteString(colorReset)

	// Content rows
	for row := 0; row < b.height; row++ {
		idx := b.offset + row
		sb.WriteString(moveTo(row+2, 1))
		if idx >= len(b.flat) {
			sb.WriteString(clearLine)
			continue
		}
		n := b.flat[idx]
		selected := idx == b.cursor

		indent := strings.Repeat("  ", n.depth)
		lbl := indent + n.label()

		if selected {
			sb.WriteString(colorHiLine)
		} else if n.entry.IsDir {
			sb.WriteString(colorDir)
		}

		padRight(&sb, lbl, w)

		if selected || n.entry.IsDir {
			sb.WriteString(colorReset)
		}
	}

	// Separator
	sb.WriteString(moveTo(h-1, 1))
	sb.WriteString(colorGray)
	sb.WriteString(strings.Repeat("─", w))
	sb.WriteString(colorReset)

	// Status / help bar
	help := " ↑↓/jk move   Enter expand   ←/h collapse   q quit"
	if b.status != "" {
		help = colorYellow + " " + b.status + colorReset
	}
	sb.WriteString(moveTo(h, 1))
	sb.WriteString(help)

	fmt.Fprint(os.Stdout, sb.String())
}

func padRight(sb *strings.Builder, s string, w int) {
	// Strip ANSI for length calculation — simple visual truncate
	visible := visibleLen(s)
	if visible > w {
		// Truncate to w visible chars (approximate)
		sb.WriteString(s[:w])
	} else {
		sb.WriteString(s)
		for i := visible; i < w; i++ {
			sb.WriteByte(' ')
		}
	}
}

// visibleLen returns the number of visible (non-ANSI) characters.
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

// ── run ──────────────────────────────────────────────────────────────────────

func (b *browser) run() error {
	ctx := context.Background()

	// Initial load
	b.status = "Loading..."
	if err := b.load(ctx); err != nil {
		return fmt.Errorf("load %q: %w", b.prefix, err)
	}
	b.status = ""

	// Switch terminal to raw mode
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("raw mode: %w", err)
	}
	defer func() {
		_ = term.Restore(fd, oldState)
		fmt.Print(showCursor)
	}()

	b.render()

	buf := make([]byte, 8)
	for {
		n, err := os.Stdin.Read(buf)
		if err != nil {
			return nil
		}
		key := buf[:n]
		switch {
		case isKey(key, 'q'), isKey(key, 27): // q or Esc
			return nil
		case isKey(key, 'j'), isEscape(key, 'B'): // down / ↓
			b.moveDown()
		case isKey(key, 'k'), isEscape(key, 'A'): // up / ↑
			b.moveUp()
		case isKey(key, 13), isKey(key, ' '): // Enter / Space → expand
			b.expand(ctx)
		case isKey(key, 'h'), isEscape(key, 'D'): // h / ← → collapse
			b.collapse()
		case isKey(key, 'l'), isEscape(key, 'C'): // l / → → same as Enter
			b.expand(ctx)
		}
		b.render()
	}
}

// ── key helpers ──────────────────────────────────────────────────────────────

func isKey(buf []byte, b byte) bool {
	return len(buf) == 1 && buf[0] == b
}

// isEscape matches ESC [ <code> arrow sequences.
func isEscape(buf []byte, code byte) bool {
	return len(buf) == 3 && buf[0] == 27 && buf[1] == '[' && buf[2] == code
}

// ── helpers ──────────────────────────────────────────────────────────────────

func entriesToNodes(entries []storage.DirEntry, depth int) []*node {
	nodes := make([]*node, 0, len(entries))
	for _, e := range entries {
		nodes = append(nodes, &node{entry: e, depth: depth})
	}
	return nodes
}

func dirName(prefix string) string {
	s := strings.TrimSuffix(prefix, "/")
	idx := strings.LastIndex(s, "/")
	if idx >= 0 {
		return s[idx+1:]
	}
	return s
}

func fileName(key string) string {
	idx := strings.LastIndex(key, "/")
	if idx >= 0 {
		return key[idx+1:]
	}
	return key
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
