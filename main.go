// Tapioca is a terminal chat client for local and hosted LLMs with
// configurable dashboards, multi-agent support, MCP tools and vim-powered
// prompt editing.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/mcp"
	"tapioca/internal/session"
	"tapioca/internal/tools"
	"tapioca/internal/ui"
)

func main() {
	var (
		configPath = flag.String("config", "", "path to config file (default ~/.config/tapioca/config.toml)")
		resumeLast = flag.Bool("r", false, "resume the most recent session")
		sessionID  = flag.String("session", "", "resume a specific session by id")
		list       = flag.Bool("list", false, "list saved sessions and exit")
	)
	flag.Parse()

	if *list {
		metas, err := session.List()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		if len(metas) == 0 {
			fmt.Println("no saved sessions")
			return
		}
		for _, m := range metas {
			name := m.Name
			if name == "" {
				name = "(unnamed)"
			}
			fmt.Printf("%s  %-40s  %d agents  %d msgs  %s\n",
				m.ID, name, m.Agents, m.Messages, m.UpdatedAt.Format("2006-01-02 15:04"))
		}
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	cwd, err := os.Getwd()
	if err != nil {
		cwd, _ = os.UserHomeDir()
	}
	exec := tools.NewExecutor(cwd, cfg.PermissionMode)

	reg := mcp.NewRegistry()
	defer reg.CloseAll()
	mgr := agent.NewManager(cfg, reg, exec)

	sessID := session.NewID()
	sessName := ""
	created := time.Now()

	resumeID := *sessionID
	if *resumeLast && resumeID == "" {
		id, err := session.LatestID()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		resumeID = id
	}
	if resumeID != "" {
		s, err := session.Load(resumeID)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		mgr.LoadSession(s)
		sessID = s.ID
		sessName = s.Name
		created = s.CreatedAt
	} else {
		mgr.NewAgent()
	}

	// Detect the terminal background before Bubble Tea takes over the TTY.
	ui.SetMarkdownDark(lipgloss.HasDarkBackground())

	app := ui.NewApp(cfg, mgr, sessID, sessName, created)
	p := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
