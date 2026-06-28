package webterm

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// tmuxThemeOption is the per-window tmux user option that selects a tile's
// theme (e.g. `tmux set -w @webterm-theme nord`) — the tmux analogue of the
// Docker `webterm-theme` label.
const tmuxThemeOption = "@webterm-theme"

// tmuxListFormat is the -F format for `list-windows`: id, index, theme, name,
// tab-separated. window_name is placed last so a stray tab inside a name cannot
// shift the earlier fields.
const tmuxListFormat = "#{window_id}\t#{window_index}\t#{" + tmuxThemeOption + "}\t#{window_name}"

const defaultTmuxPollInterval = 2 * time.Second

// TmuxWatchConfig configures a TmuxWatcher.
type TmuxWatchConfig struct {
	Session      string        // tmux session whose windows become tiles
	Binary       string        // path to the tmux binary (default: "tmux")
	Socket       string        // optional -S socket path ("" = default socket)
	PollInterval time.Duration // reconcile cadence (default 2s)
}

type tmuxWindow struct {
	ID    string // stable, unique window id, e.g. "@71"
	Index string // display index, e.g. "2" (can change)
	Theme string // value of @webterm-theme, or ""
	Name  string // window name (can change)
}

type managedWindow struct {
	slug    string
	display string // tile label currently registered
	theme   string
}

// TmuxWatcher mirrors DockerWatcher but discovers tiles from the windows of a
// tmux session instead of Docker containers. Each window becomes one tile whose
// command attaches a private grouped session focused on that window, so tiles
// are independent of each other and never disturb the user's own client on the
// session. Discovery is by polling `tmux list-windows`; tmux has no event
// stream analogous to the Docker event socket, and the window set changes
// rarely, so a short poll is both simplest and sufficient.
type TmuxWatcher struct {
	sessionManager *SessionManager
	session        string
	binary         string
	socket         string
	poll           time.Duration

	// listFn returns the current windows; overridable in tests.
	listFn func() ([]tmuxWindow, error)

	onWindowAdded   func(slug, name, command string)
	onWindowRemoved func(slug string)

	mu       sync.Mutex
	managed  map[string]managedWindow // keyed by window ID
	running  bool
	cancel   context.CancelFunc
	waitDone chan struct{}
}

func NewTmuxWatcher(
	sessionManager *SessionManager,
	cfg TmuxWatchConfig,
	onAdded func(slug, name, command string),
	onRemoved func(slug string),
) *TmuxWatcher {
	session := strings.TrimSpace(cfg.Session)
	if session == "" {
		session = DefaultTmuxSession
	}
	binary := strings.TrimSpace(cfg.Binary)
	if binary == "" {
		binary = DefaultTmuxBinary
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultTmuxPollInterval
	}
	w := &TmuxWatcher{
		sessionManager:  sessionManager,
		session:         session,
		binary:          binary,
		socket:          strings.TrimSpace(cfg.Socket),
		poll:            poll,
		onWindowAdded:   onAdded,
		onWindowRemoved: onRemoved,
		managed:         map[string]managedWindow{},
		waitDone:        make(chan struct{}),
	}
	w.listFn = w.listWindows
	return w
}

// listWindows runs `tmux list-windows` against the configured session/socket.
func (w *TmuxWatcher) listWindows() ([]tmuxWindow, error) {
	args := []string{}
	if w.socket != "" {
		args = append(args, "-S", w.socket)
	}
	args = append(args, "list-windows", "-t", w.session, "-F", tmuxListFormat)
	cmd := exec.Command(w.binary, args...)
	// Force a UTF-8 locale. Under a C/POSIX locale (the default in minimal
	// containers) tmux sanitises the tab field separators in -F output to "_",
	// which makes every line unparseable; with a UTF-8 locale it emits literal
	// tabs. This matters in-container — a host shell usually already has one.
	cmd.Env = tmuxExecEnv()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return parseTmuxWindows(string(out)), nil
}

// tmuxExecEnv returns the process environment with the locale forced to UTF-8
// (any inherited LANG/LC_* dropped first so there is no conflicting setting).
func tmuxExecEnv() []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		if strings.HasPrefix(kv, "LANG=") ||
			strings.HasPrefix(kv, "LC_ALL=") ||
			strings.HasPrefix(kv, "LC_CTYPE=") {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LANG=C.UTF-8", "LC_ALL=C.UTF-8")
}

func parseTmuxWindows(out string) []tmuxWindow {
	windows := []tmuxWindow{}
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		// id \t index \t theme \t name (name may itself contain tabs)
		fields := strings.SplitN(line, "\t", 4)
		if len(fields) < 4 {
			continue
		}
		id := strings.TrimSpace(fields[0])
		if id == "" {
			continue
		}
		windows = append(windows, tmuxWindow{
			ID:    id,
			Index: strings.TrimSpace(fields[1]),
			Theme: strings.TrimSpace(fields[2]),
			Name:  fields[3],
		})
	}
	return windows
}

func tmuxSlug(windowID string) string {
	return "wt-" + strings.TrimPrefix(windowID, "@")
}

func tmuxDisplayName(win tmuxWindow) string {
	name := strings.TrimSpace(win.Name)
	if name == "" {
		name = "window"
	}
	if win.Index != "" {
		return win.Index + ": " + name
	}
	return name
}

// tileCommand builds the PTY command for a window's tile: idempotently create a
// private detached session grouped with the target session, point it at this
// window by its stable id, then attach. The result shlex-splits to
// ["sh", "-lc", inner]; the inner script contains no single quote by
// construction, so the outer single-quoting round-trips cleanly.
func (w *TmuxWatcher) tileCommand(win tmuxWindow) string {
	bin := tmuxStripSingleQuotes(w.binary)
	sess := tmuxStripSingleQuotes(w.session)
	grouped := tmuxSlug(win.ID)

	invoke := tmuxDoubleQuote(bin)
	if w.socket != "" {
		invoke += " -S " + tmuxDoubleQuote(tmuxStripSingleQuotes(w.socket))
	}

	inner := fmt.Sprintf(
		`%s new-session -d -t %s -s %s 2>/dev/null; %s select-window -t %s:%s 2>/dev/null; exec %s attach -t %s`,
		invoke, tmuxDoubleQuote(sess), tmuxDoubleQuote(grouped),
		invoke, tmuxDoubleQuote(grouped), tmuxDoubleQuote(win.ID),
		invoke, tmuxDoubleQuote(grouped),
	)
	return "sh -lc " + tmuxSingleQuote(inner)
}

func (w *TmuxWatcher) reconcile() {
	windows, err := w.listFn()
	if err != nil {
		// tmux not up yet / transient — keep the dashboard alive and retry.
		log.Printf("tmux watch: list-windows failed session=%s err=%v", w.session, err)
		return
	}
	seen := make(map[string]bool, len(windows))
	for _, win := range windows {
		seen[win.ID] = true
		display := tmuxDisplayName(win)

		w.mu.Lock()
		existing, ok := w.managed[win.ID]
		w.mu.Unlock()

		switch {
		case !ok:
			w.addWindow(win, display)
		case existing.display != display || existing.theme != win.Theme:
			w.updateWindow(win, display)
		}
	}

	w.mu.Lock()
	gone := make([]string, 0)
	for id := range w.managed {
		if !seen[id] {
			gone = append(gone, id)
		}
	}
	w.mu.Unlock()
	for _, id := range gone {
		w.removeWindow(id)
	}
}

func (w *TmuxWatcher) addWindow(win tmuxWindow, display string) {
	slug := tmuxSlug(win.ID)
	command := w.tileCommand(win)

	w.mu.Lock()
	w.managed[win.ID] = managedWindow{slug: slug, display: display, theme: win.Theme}
	w.mu.Unlock()

	w.sessionManager.AddApp(display, command, slug, true, win.Theme)
	log.Printf("tmux watch: added window id=%s slug=%s name=%q", win.ID, slug, display)
	if w.onWindowAdded != nil {
		w.onWindowAdded(slug, display, command)
	}
}

// updateWindow refreshes a tile's label/theme in place — no session disruption —
// when a window is renamed, renumbered, or re-themed. The tile command keys on
// the window id, which is stable across all three, so it stays valid.
func (w *TmuxWatcher) updateWindow(win tmuxWindow, display string) {
	slug := tmuxSlug(win.ID)
	w.mu.Lock()
	w.managed[win.ID] = managedWindow{slug: slug, display: display, theme: win.Theme}
	w.mu.Unlock()

	w.sessionManager.UpdateApp(slug, display, win.Theme)
	if w.onWindowAdded != nil {
		w.onWindowAdded(slug, display, "")
	}
}

func (w *TmuxWatcher) removeWindow(id string) {
	w.mu.Lock()
	m, ok := w.managed[id]
	if ok {
		delete(w.managed, id)
	}
	w.mu.Unlock()
	if !ok {
		return
	}
	if sessionID, ok := w.sessionManager.GetSessionIDByRouteKey(m.slug); ok {
		w.sessionManager.CloseSession(sessionID)
	}
	w.sessionManager.RemoveApp(m.slug)
	log.Printf("tmux watch: removed window id=%s slug=%s", id, m.slug)
	if w.onWindowRemoved != nil {
		w.onWindowRemoved(m.slug)
	}
}

// ScanExisting performs one reconcile pass (parity with DockerWatcher).
func (w *TmuxWatcher) ScanExisting() { w.reconcile() }

func (w *TmuxWatcher) Start() {
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	waitDone := make(chan struct{})
	w.cancel = cancel
	w.waitDone = waitDone
	w.running = true
	w.mu.Unlock()
	log.Printf("tmux watcher started session=%s binary=%s socket=%s poll=%s", w.session, w.binary, w.socket, w.poll)
	go w.loop(ctx, waitDone)
}

func (w *TmuxWatcher) loop(ctx context.Context, waitDone chan struct{}) {
	defer close(waitDone)
	w.reconcile()
	ticker := time.NewTicker(w.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reconcile()
		}
	}
}

func (w *TmuxWatcher) Stop() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	w.running = false
	cancel := w.cancel
	waitDone := w.waitDone
	w.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if waitDone != nil {
		<-waitDone
	}
	log.Printf("tmux watcher stopped")
}

// tmuxDoubleQuote wraps s in sh double quotes, escaping the characters that are
// still special inside them.
func tmuxDoubleQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", `$`, `\$`)
	return `"` + r.Replace(s) + `"`
}

// tmuxSingleQuote wraps s in sh single quotes (POSIX-escaping any embedded
// single quote). Inner scripts are built without single quotes, so this is a
// clean wrap in practice; the escaping is defensive.
func tmuxSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// tmuxStripSingleQuotes removes single quotes from an interpolated config value
// so the single-quoted outer command is guaranteed to round-trip through shlex.
func tmuxStripSingleQuotes(s string) string {
	return strings.ReplaceAll(s, "'", "")
}
