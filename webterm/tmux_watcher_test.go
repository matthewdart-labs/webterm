package webterm

import (
	"strings"
	"testing"

	"github.com/google/shlex"
)

func TestParseTmuxWindows(t *testing.T) {
	// id \t index \t theme \t name — covers an empty theme and a name with spaces.
	out := "@70\t1\tnord\tbash\n" +
		"@71\t2\t\tclaude code\n" +
		"\n" + // blank line ignored
		"@82\t4\tgotham\tlogs\n"
	windows := parseTmuxWindows(out)
	if len(windows) != 3 {
		t.Fatalf("expected 3 windows, got %d: %+v", len(windows), windows)
	}
	if windows[0] != (tmuxWindow{ID: "@70", Index: "1", Theme: "nord", Name: "bash"}) {
		t.Fatalf("window 0 mismatch: %+v", windows[0])
	}
	if windows[1].Theme != "" || windows[1].Name != "claude code" {
		t.Fatalf("window 1 theme/name mismatch: %+v", windows[1])
	}
	if windows[2].ID != "@82" || windows[2].Index != "4" || windows[2].Theme != "gotham" {
		t.Fatalf("window 2 mismatch: %+v", windows[2])
	}
}

func TestTmuxSlugAndDisplayName(t *testing.T) {
	if got := tmuxSlug("@71"); got != "wt-71" {
		t.Fatalf("slug: got %q want wt-71", got)
	}
	if got := tmuxDisplayName(tmuxWindow{Index: "2", Name: "claude"}); got != "2: claude" {
		t.Fatalf("display: got %q want '2: claude'", got)
	}
	if got := tmuxDisplayName(tmuxWindow{Index: "3", Name: ""}); got != "3: window" {
		t.Fatalf("display fallback: got %q want '3: window'", got)
	}
}

func TestTmuxWatcherTileCommandShlex(t *testing.T) {
	w := NewTmuxWatcher(NewSessionManager(nil), TmuxWatchConfig{
		Session: "main",
		Binary:  "/nix/store/abc-tmux-3.6a/bin/tmux",
		Socket:  "/tmp/tmux-1001/default",
	}, nil, nil)

	cmd := w.tileCommand(tmuxWindow{ID: "@71", Index: "2", Name: "bash"})

	// Must split exactly into sh -lc <script> (this is how TerminalSession runs it).
	argv, err := shlex.Split(cmd)
	if err != nil {
		t.Fatalf("command is not shlex-splittable: %v\ncmd=%s", err, cmd)
	}
	if len(argv) != 3 || argv[0] != "sh" || argv[1] != "-lc" {
		t.Fatalf("expected [sh -lc <script>], got %#v", argv)
	}
	inner := argv[2]
	for _, want := range []string{
		`new-session -d -t "main" -s "wt-71"`,
		`select-window -t "wt-71":"@71"`,
		`attach -t "wt-71"`,
		`-S "/tmp/tmux-1001/default"`,
		`"/nix/store/abc-tmux-3.6a/bin/tmux"`,
	} {
		if !strings.Contains(inner, want) {
			t.Fatalf("inner script missing %q\ninner=%s", want, inner)
		}
	}
	if strings.Contains(inner, "'") {
		t.Fatalf("inner script must not contain a single quote (breaks sh round-trip)\ninner=%s", inner)
	}
}

func TestTmuxWatcherTileCommandNoSocket(t *testing.T) {
	w := NewTmuxWatcher(NewSessionManager(nil), TmuxWatchConfig{Binary: "tmux"}, nil, nil)
	cmd := w.tileCommand(tmuxWindow{ID: "@9", Index: "1", Name: "shell"})
	if strings.Contains(cmd, "-S ") {
		t.Fatalf("no socket configured, command should not pass -S: %s", cmd)
	}
	argv, err := shlex.Split(cmd)
	if err != nil || len(argv) != 3 {
		t.Fatalf("bad split: %v %#v", err, argv)
	}
}

func TestTmuxListFormat(t *testing.T) {
	// The theme option must be wrapped in #{...} so tmux expands it (a bare
	// "@webterm-theme" would emit the literal text), and fields must be
	// tab-separated to match the parser.
	if !strings.Contains(tmuxListFormat, "#{@webterm-theme}") {
		t.Fatalf("theme option not wrapped for expansion: %q", tmuxListFormat)
	}
	if strings.Count(tmuxListFormat, "\t") != 3 {
		t.Fatalf("expected 3 tab separators, got format %q", tmuxListFormat)
	}
}

func TestTmuxExecEnvForcesUTF8(t *testing.T) {
	// tmux sanitises the tab separators to "_" under a C locale, so the watcher
	// must run it with a UTF-8 locale and drop any conflicting inherited value.
	t.Setenv("LC_ALL", "C")
	t.Setenv("LANG", "POSIX")
	env := tmuxExecEnv()
	var lcAll, lang int
	for _, kv := range env {
		switch {
		case kv == "LC_ALL=C.UTF-8":
			lcAll++
		case kv == "LANG=C.UTF-8":
			lang++
		case strings.HasPrefix(kv, "LC_ALL=") || strings.HasPrefix(kv, "LANG="):
			t.Fatalf("conflicting locale left in env: %q", kv)
		}
	}
	if lcAll != 1 || lang != 1 {
		t.Fatalf("expected exactly one UTF-8 LC_ALL and LANG, got LC_ALL=%d LANG=%d", lcAll, lang)
	}
}

func TestTmuxWatcherReconcileAddUpdateRemove(t *testing.T) {
	sm := NewSessionManager(nil)
	var added, removed []string
	w := NewTmuxWatcher(sm, TmuxWatchConfig{Session: "main", Binary: "tmux"},
		func(slug, _, _ string) { added = append(added, slug) },
		func(slug string) { removed = append(removed, slug) },
	)

	current := []tmuxWindow{
		{ID: "@1", Index: "1", Name: "bash"},
		{ID: "@2", Index: "2", Name: "claude"},
	}
	w.listFn = func() ([]tmuxWindow, error) { return current, nil }

	// Initial discovery -> two tiles.
	w.reconcile()
	if len(sm.Apps()) != 2 {
		t.Fatalf("expected 2 apps after first reconcile, got %d", len(sm.Apps()))
	}
	if len(added) != 2 {
		t.Fatalf("expected 2 add callbacks, got %d", len(added))
	}
	if _, ok := sm.AppBySlug("wt-2"); !ok {
		t.Fatalf("expected app wt-2 to exist")
	}

	// Rename window @2 -> the same id, new name. Must UPDATE in place, not add.
	current[1].Name = "claude (working)"
	w.reconcile()
	if got := len(sm.Apps()); got != 2 {
		t.Fatalf("rename must not change app count, got %d", got)
	}
	if app, _ := sm.AppBySlug("wt-2"); app.Name != "2: claude (working)" {
		t.Fatalf("expected updated label, got %q", app.Name)
	}

	// Close window @1.
	current = current[1:]
	w.reconcile()
	apps := sm.Apps()
	if len(apps) != 1 || apps[0].Slug != "wt-2" {
		t.Fatalf("expected only wt-2 to remain, got %+v", apps)
	}
	if len(removed) != 1 || removed[0] != "wt-1" {
		t.Fatalf("expected wt-1 removed, got %+v", removed)
	}
}

func TestTmuxWatcherListErrorIsTolerated(t *testing.T) {
	sm := NewSessionManager(nil)
	w := NewTmuxWatcher(sm, TmuxWatchConfig{}, nil, nil)
	w.listFn = func() ([]tmuxWindow, error) { return nil, errFakeTmuxDown }
	w.reconcile() // must not panic; just logs and returns
	if len(sm.Apps()) != 0 {
		t.Fatalf("expected no apps when tmux is unavailable")
	}
}

func TestTmuxWatcherCanRestart(t *testing.T) {
	w := NewTmuxWatcher(NewSessionManager(nil), TmuxWatchConfig{}, nil, nil)
	w.listFn = func() ([]tmuxWindow, error) { return nil, nil } // never touch real tmux
	w.Start()
	w.Stop()
	w.Start()
	w.Stop()
}

var errFakeTmuxDown = stubError("tmux: no server running")

type stubError string

func (e stubError) Error() string { return string(e) }
