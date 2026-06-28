# Fork notes — `matthewdart-labs/webterm`

A fork of [`rcarmo/webterm`](https://github.com/rcarmo/webterm) that adds a
**tmux watch mode**: one live dashboard tile per window of a host tmux session.
Upstream's dynamic discovery only sees Docker containers (`--docker-watch`); this
fork generalises the same pattern to host tmux windows.

It is kept as a fork (rather than an upstream PR) deliberately: the
implementation was AI-assisted and is owned/reviewed here rather than pushed to
someone else's project.

## What diverges from upstream

The change is **one new file plus small, isolated wiring hunks** — designed to
rebase onto upstream with minimal conflict and to read as a near-mechanical twin
of the existing `docker_watcher.go`.

| File | Change |
|------|--------|
| `webterm/tmux_watcher.go` | **New.** `TmuxWatcher` — polls `tmux list-windows`, maps each window to a tile via the generic `SessionManager.AddApp/UpdateApp/RemoveApp`. Twin of `docker_watcher.go`. |
| `webterm/tmux_watcher_test.go` | **New.** Unit tests (parse, slug/label, tile-command shlex round-trip, add/update/remove reconcile, locale env, list-error tolerance, restart). |
| `webterm/cli.go` | `--tmux-watch`, `--tmux-session`, `--tmux-binary`, `--tmux-socket` flags (+ `WEBTERM_TMUX_*` env fallbacks); skip the default single-shell session when watching. |
| `webterm/constants.go` | `DefaultTmuxSession`, `DefaultTmuxBinary`, `WEBTERM_TMUX_*` env names. |
| `webterm/session_manager.go` | `UpdateApp(slug, name, theme)` — refresh a tile's label/theme in place (so a tmux rename doesn't tear down a live session). |
| `webterm/server.go` | `ServerOptions`/`LocalServer` tmux fields; `setupTmuxFeatures()`; dashboard gates (`dashboardTiles`, `showDashboard`) extended to the tmux-watch path; a `tmuxWatchMode` front-end flag for mode-correct empty/subtitle text. |

Nothing in the session/PTY/screenshot/replay core changes: a tmux tile is an
ordinary PTY running a tmux `attach` command, exactly like any other session.

## How it works

Each window becomes a tile whose command attaches a **private grouped session**
focused on that window by its stable id:

```
sh -lc 'tmux -S <sock> new-session -d -t <session> -s wt-<id> 2>/dev/null; \
        tmux -S <sock> select-window -t wt-<id>:@<id> 2>/dev/null; \
        exec tmux -S <sock> attach -t wt-<id>'
```

Grouped sessions share the target session's window list but keep an independent
current window, so tiles are independent and closing one never disturbs your own
client on the session. Discovery is by polling (default 2s); tmux has no event
socket like Docker's, and the window set changes rarely.

## Deploy gotchas (learned the hard way)

- **UTF-8 locale is mandatory in-container.** Under a C/POSIX locale (the default
  in minimal images) tmux sanitises the tab separators in `-F` output to `_`,
  making every line unparseable. The watcher forces `LC_ALL=C.UTF-8` on its tmux
  exec; also set `LANG=C.UTF-8` in the container so the interactive attach PTYs
  render UTF-8.
- **Matching the host tmux.** A containerised tmux *client* must speak the host
  tmux *server*'s protocol. The simplest robust approach is to not ship a client
  at all: bind-mount the host's `/nix/store` (or `/usr/bin/tmux`) read-only and
  point `--tmux-binary` at the host's own tmux. Mount the socket dir and run the
  container as the socket's owning uid.
- **Resize.** Attaching a small browser client to a shared window can shrink your
  real client's view under tmux's default `window-size latest`. `set -g
  window-size largest` makes the largest attached client win.

## Rebasing onto upstream

```
git fetch upstream
git rebase upstream/main           # tmux_watcher.go is additive → no conflict;
                                   # wiring hunks are small and localised
make check                         # lint + test + coverage
```

Upstream is not gofmt-clean (`gofmt -l` flags several of its files); this fork
matches the existing in-repo style and does not reformat upstream lines.
