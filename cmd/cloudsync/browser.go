package main

// Interactive TUI for browsing a COS bucket, similar to lazygit.
//
// Keys:
//   ↑/k   move up
//   ↓/j   move down
//   Enter  expand directory / no-op on file
//   ←/h   collapse / go to parent
//   d      delete (confirm with d, cancel with Esc)
//   s      sync directory (input local path, Enter to confirm, Esc to cancel)
//   q/Esc  quit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cloudsync/cloudsync/internal/apiclient"
	"github.com/cloudsync/cloudsync/internal/ipc"
	"github.com/cloudsync/cloudsync/internal/storage"
	"golang.org/x/term"
)

// ── browser mode state machine ────────────────────────────────────────────────

type browserMode int

const (
	modeNormal        browserMode = iota
	modeDeleteConfirm             // waiting for 'd' confirm or Esc cancel
	modeSyncInput                 // inline path input active
	modeBusy                      // executing async operation (directory delete)
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
	cos    *storage.COSClient
	root   []*node // top-level entries
	flat   []*node // flattened visible list
	cursor int
	offset int // scroll offset
	height int // terminal rows available for content
	width  int
	bucket string
	prefix string
	status string // status bar message

	// mode state machine
	mode        browserMode
	pendingNode *node // node targeted by current d/s operation

	// s (sync) input state
	inputBuf    []rune // current user input, []rune for O(1) backspace
	defaultPath string // pre-computed default local path
	inputPrompt string // full prompt string for render

	// daemon integration
	apiClient *apiclient.Client // nil = daemon not running
	mounts    []ipc.MountRecord // loaded once at startup

	// background operation results (buffered size 1; sender never blocks)
	bgResult chan string
}

func newBrowser(cos *storage.COSClient, bucket, prefix string, apiClient *apiclient.Client) *browser {
	b := &browser{
		cos:       cos,
		bucket:    bucket,
		prefix:    prefix,
		apiClient: apiClient,
		bgResult:  make(chan string, 1),
	}
	// Best-effort: load mounts at startup. Failure is silently ignored.
	if apiClient != nil {
		b.mounts, _ = apiClient.ListMounts()
	}
	return b
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

// ── new helper methods ────────────────────────────────────────────────────────

// selectedNode returns the node at the current cursor position, or nil if list is empty.
func (b *browser) selectedNode() *node {
	if len(b.flat) == 0 || b.cursor < 0 || b.cursor >= len(b.flat) {
		return nil
	}
	return b.flat[b.cursor]
}

// findMountForKey returns the first MountRecord whose RemotePrefix is a prefix of key.
func (b *browser) findMountForKey(key string) *ipc.MountRecord {
	for i := range b.mounts {
		if strings.HasPrefix(key, b.mounts[i].RemotePrefix) {
			return &b.mounts[i]
		}
	}
	return nil
}

// localPathForKey returns the local path that maps to key via a mount, if any.
func (b *browser) localPathForKey(key string) (string, bool) {
	m := b.findMountForKey(key)
	if m == nil {
		return "", false
	}
	rel := strings.TrimPrefix(key, m.RemotePrefix)
	return filepath.Join(m.LocalPath, filepath.FromSlash(rel)), true
}

// findParent recursively searches b.root for the parent of target.
// Returns nil if target is a top-level node.
func (b *browser) findParent(target *node) *node {
	return findParentIn(b.root, target)
}

func findParentIn(nodes []*node, target *node) *node {
	for _, n := range nodes {
		for _, child := range n.children {
			if child == target {
				return n
			}
		}
		if found := findParentIn(n.children, target); found != nil {
			return found
		}
	}
	return nil
}

// removeNodeFromTree removes n from the tree (root or parent.children), then
// rebuilds the flat list and clamps the cursor.
func (b *browser) removeNodeFromTree(n *node) {
	parent := b.findParent(n)
	if parent != nil {
		parent.children = removeFromSlice(parent.children, n)
	} else {
		b.root = removeFromSlice(b.root, n)
	}
	b.rebuild()
	b.clampCursor()
}

func removeFromSlice(nodes []*node, target *node) []*node {
	out := nodes[:0]
	for _, n := range nodes {
		if n != target {
			out = append(out, n)
		}
	}
	return out
}

// executeDelete delegates deletion to the daemon via DeleteObjects.
// The daemon handles all COS and local-file I/O asynchronously.
func (b *browser) executeDelete() {
	n := b.pendingNode
	b.pendingNode = nil
	b.mode = modeNormal

	if n == nil {
		return
	}

	if b.apiClient == nil {
		b.status = "Error: daemon not running"
		return
	}

	var remoteKey string
	if n.entry.IsDir {
		remoteKey = n.entry.Prefix
	} else {
		remoteKey = n.entry.Key
	}

	// Fire-and-forget: send the request to the daemon and return immediately.
	// Remove the node from the tree optimistically; the status bar confirms.
	go func() {
		err := b.apiClient.DeleteObjects(remoteKey)
		var msg string
		if err != nil {
			msg = "Delete error: " + err.Error()
		} else {
			msg = "Deleting (background): " + remoteKey
		}
		b.bgResult <- msg
	}()

	b.removeNodeFromTree(n)
	b.status = "Deleting (background): " + remoteKey
}

// enterSyncMode validates the selection and switches to modeSyncInput.
func (b *browser) enterSyncMode() {
	n := b.selectedNode()
	if n == nil || !n.entry.IsDir {
		b.status = "s: directories only"
		return
	}
	if b.apiClient == nil {
		b.status = "Error: daemon not running"
		return
	}
	home, _ := os.UserHomeDir()
	trimmed := strings.TrimRight(n.entry.Prefix, "/")
	b.defaultPath = filepath.Join(home, filepath.FromSlash(trimmed))
	b.inputPrompt = "Sync → local path (default: " + b.defaultPath + "): "
	b.inputBuf = b.inputBuf[:0]
	b.pendingNode = n
	b.mode = modeSyncInput
}

// executeSync calls AddMount via the daemon in a background goroutine.
// The daemon creates the local directory and pulls remote files before
// starting the watcher — no filesystem or COS I/O happens in the TUI process.
func (b *browser) executeSync() {
	localPath := strings.TrimSpace(string(b.inputBuf))
	if localPath == "" {
		localPath = b.defaultPath
	}
	n := b.pendingNode

	b.inputBuf = b.inputBuf[:0]
	b.pendingNode = nil
	b.mode = modeNormal

	if n == nil {
		return
	}

	prefix := n.entry.Prefix
	b.status = "Syncing " + prefix + " → " + localPath + " (background…)"

	go func() {
		// downloadFirst=true: daemon pulls existing remote files before the
		// upload watcher starts. Directory creation is also handled by the daemon.
		_, err := b.apiClient.AddMount(localPath, prefix, true, "")
		var msg string
		if err != nil {
			msg = "Sync error: " + err.Error()
		} else {
			msg = "Syncing: " + localPath + " ↔ " + prefix
		}
		b.bgResult <- msg
	}()
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

	// Status / help bar (last line) — content depends on mode
	sb.WriteString(moveTo(h, 1))
	switch b.mode {
	case modeDeleteConfirm:
		name := ""
		if b.pendingNode != nil {
			if b.pendingNode.entry.IsDir {
				name = dirName(b.pendingNode.entry.Prefix) + "/"
			} else {
				name = fileName(b.pendingNode.entry.Key)
			}
		}
		line := colorYellow + " Delete \"" + name + "\"? Press d to confirm, Esc to cancel" + colorReset
		sb.WriteString(line)

	case modeSyncInput:
		line := b.inputPrompt + string(b.inputBuf) + "█"
		// Truncate from left if too wide
		if visibleLen(line) > w {
			// Keep right portion that fits
			runes := []rune(line)
			for visibleLen(string(runes)) > w && len(runes) > 0 {
				runes = runes[1:]
			}
			line = string(runes)
		}
		sb.WriteString(line)

	case modeBusy:
		sb.WriteString(colorYellow + " " + b.status + colorReset)

	default: // modeNormal
		help := " ↑↓/jk move   Enter expand   ←/h collapse   d delete   s sync   q quit"
		if b.status != "" {
			help = colorYellow + " " + b.status + colorReset
		}
		sb.WriteString(help)
	}

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

	// Enable ANSI VT escape code processing (required on Windows 10+).
	enableVTMode()

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

	type keyMsg struct {
		data []byte
		err  error
	}
	keyCh := make(chan keyMsg, 1)

	// Read stdin in a dedicated goroutine so the main loop can also select on
	// bgResult (background AddMount completions) without blocking.
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
		select {
		case msg := <-keyCh:
			if msg.err != nil {
				return nil
			}

			// In modeBusy: discard all input and just re-render
			if b.mode == modeBusy {
				b.render()
				go readNext()
				continue
			}

			key := msg.data

			switch b.mode {
			case modeDeleteConfirm:
				switch {
				case isKey(key, 'd'):
					b.executeDelete()
				case isKey(key, 27), isKey(key, 'q'): // Esc or q = cancel
					b.pendingNode = nil
					b.mode = modeNormal
					b.status = "Cancelled"
				}

			case modeSyncInput:
				switch {
				case isKey(key, 13): // Enter
					b.executeSync()
				case isKey(key, 27): // Esc = cancel
					b.inputBuf = b.inputBuf[:0]
					b.pendingNode = nil
					b.mode = modeNormal
					b.status = "Cancelled"
				case isKey(key, 127), isKey(key, 8): // Backspace / DEL
					if len(b.inputBuf) > 0 {
						b.inputBuf = b.inputBuf[:len(b.inputBuf)-1]
					}
				default:
					// Accept printable ASCII
					if len(key) == 1 && key[0] >= 32 {
						b.inputBuf = append(b.inputBuf, rune(key[0]))
					}
				}

			default: // modeNormal
				switch {
				case isKey(key, 'q'), isKey(key, 27): // q or Esc → quit
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
				case isKey(key, 'd'): // d → delete
					if sel := b.selectedNode(); sel != nil {
						b.pendingNode = sel
						b.mode = modeDeleteConfirm
						b.status = ""
					}
				case isKey(key, 's'): // s → sync
					b.enterSyncMode()
				}
			}

			b.render()
			go readNext()

		case msg := <-b.bgResult:
			b.status = msg
			b.render()
		}
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
