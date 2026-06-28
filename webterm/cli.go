package webterm

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

func RunCLI(args []string) error {
	fs := flag.NewFlagSet("webterm", flag.ContinueOnError)
	fs.SetOutput(os.Stdout)

	port := DefaultPort
	host := DefaultHost
	landingManifest := ""
	composeManifest := ""
	dockerWatch := false
	tmuxWatch := false
	tmuxSession := DefaultTmuxSession
	tmuxBinary := ""
	tmuxSocket := ""
	theme := DefaultTheme
	fontFamily := ""
	fontSize := DefaultFontSize
	showVersion := false

	fs.IntVar(&port, "port", DefaultPort, "Port for server.")
	fs.IntVar(&port, "p", DefaultPort, "Port for server.")
	fs.StringVar(&host, "host", DefaultHost, "Host for server.")
	fs.StringVar(&host, "H", DefaultHost, "Host for server.")
	fs.StringVar(&landingManifest, "landing-manifest", "", "YAML manifest describing landing page tiles.")
	fs.StringVar(&landingManifest, "L", "", "YAML manifest describing landing page tiles.")
	fs.StringVar(&composeManifest, "compose-manifest", "", "Docker compose YAML; services with label \"webterm-command\" become landing tiles.")
	fs.StringVar(&composeManifest, "C", "", "Docker compose YAML; services with label \"webterm-command\" become landing tiles.")
	fs.BoolVar(&dockerWatch, "docker-watch", false, "Watch Docker for containers with labels and add/remove sessions dynamically.")
	fs.BoolVar(&dockerWatch, "D", false, "Watch Docker for containers with labels and add/remove sessions dynamically.")
	fs.BoolVar(&tmuxWatch, "tmux-watch", false, "Watch a tmux session and add/remove one tile per window dynamically.")
	fs.StringVar(&tmuxSession, "tmux-session", DefaultTmuxSession, "tmux session whose windows become tiles (with --tmux-watch).")
	fs.StringVar(&tmuxBinary, "tmux-binary", "", "Path to the tmux binary (default: tmux on PATH; env WEBTERM_TMUX_BINARY).")
	fs.StringVar(&tmuxSocket, "tmux-socket", "", "tmux socket path passed as -S (default: tmux default socket; env WEBTERM_TMUX_SOCKET).")
	fs.StringVar(&theme, "theme", DefaultTheme, "Terminal color theme.")
	fs.StringVar(&theme, "t", DefaultTheme, "Terminal color theme.")
	fs.StringVar(&fontFamily, "font-family", "", "Terminal font family (CSS font stack).")
	fs.StringVar(&fontFamily, "f", "", "Terminal font family (CSS font stack).")
	fs.IntVar(&fontSize, "font-size", DefaultFontSize, "Terminal font size in pixels.")
	fs.IntVar(&fontSize, "s", DefaultFontSize, "Terminal font size in pixels.")
	fs.BoolVar(&showVersion, "version", false, "Print version and exit.")
	fs.BoolVar(&showVersion, "v", false, "Print version and exit.")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if showVersion {
		_, _ = fmt.Fprintln(os.Stdout, Version)
		return nil
	}

	command := strings.TrimSpace(strings.Join(fs.Args(), " "))
	config := DefaultConfig()
	landingApps := []App{}
	composeMode := false
	composeProject := ""

	if landingManifest != "" {
		apps, err := LoadLandingYAML(landingManifest)
		if err != nil {
			return err
		}
		landingApps = apps
	}
	if composeManifest != "" {
		apps, project, err := LoadComposeManifest(composeManifest)
		if err != nil {
			return err
		}
		landingApps = apps
		composeMode = true
		composeProject = project
	}
	if composeProject == "" && composeManifest != "" {
		composeProject = filepath.Base(filepath.Dir(composeManifest))
	}

	// Env fallbacks for the deploy-time tmux values (handy when the binary and
	// socket are fixed by the container, not the launch command).
	if tmuxBinary == "" {
		tmuxBinary = os.Getenv(TmuxBinaryEnv)
	}
	if tmuxSocket == "" {
		tmuxSocket = os.Getenv(TmuxSocketEnv)
	}
	if tmuxSession == DefaultTmuxSession {
		if env := strings.TrimSpace(os.Getenv(TmuxSessionEnv)); env != "" {
			tmuxSession = env
		}
	}

	server := NewLocalServer(config, ServerOptions{
		Host:           host,
		Port:           port,
		Theme:          theme,
		FontFamily:     fontFamily,
		FontSize:       fontSize,
		LandingApps:    landingApps,
		ComposeMode:    composeMode,
		ComposeProject: composeProject,
		DockerWatch:    dockerWatch,
		TmuxWatch:      tmuxWatch,
		TmuxSession:    tmuxSession,
		TmuxBinary:     tmuxBinary,
		TmuxSocket:     tmuxSocket,
	})

	if command != "" {
		server.sessionManager.AddApp("Terminal", command, "", true, "")
	} else if !dockerWatch && !tmuxWatch && len(landingApps) == 0 {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		server.sessionManager.AddApp("Terminal", shell, "", true, "")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return server.Run(ctx)
}
