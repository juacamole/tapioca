// Tapioca is an agentic coding TUI for local and hosted LLMs.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tapioca/internal/agent"
	"tapioca/internal/catalog"
	"tapioca/internal/checkpoint"
	"tapioca/internal/config"
	"tapioca/internal/mcp"
	"tapioca/internal/session"
	"tapioca/internal/tools"
	"tapioca/internal/ui"
)

const version = "0.1.0"

type cliArgs struct {
	settings     string
	model        string
	permMode     string
	systemPrompt string
	appendSystem string
	mcpConfig    string
	sessionID    string
	resumeID     string
	printPrompt  string
	outputFormat string
	addDirs      []string
	maxTurns     int

	continueLatest bool
	resumePicker   bool
	forkSession    bool
	listSessions   bool
}

const usage = `Usage: tapioca [options]

Options:
  -c, --continue                     Continue the most recent session
  -r, --resume [id]                  Resume a session (picker when no id)
      --session-id <id>              Use a specific session id
      --fork-session                 Resume into a new session id
      --model [provider:]<model>     Model for this run
      --permission-mode <mode>       plan | manual | auto | bypass
      --dangerously-skip-permissions Run all tools without asking (bypass)
      --settings <file>              Config file (default ~/.config/tapioca/config.toml)
      --system-prompt <text>         Replace the system prompt
      --append-system-prompt <text>  Append to the system prompt
      --add-dir <dir>                Announce an additional working directory (repeatable)
      --mcp-config <file>            Load MCP servers from this TOML file instead
  -p, --print <prompt>               Non-interactive: run one prompt, print, exit
      --output-format <fmt>          For -p: text (default) or json
      --max-turns <n>                For -p: cap agentic tool rounds
      --list-sessions                List saved sessions and exit
  -v, --version                      Print version
  -h, --help                         Show this help`

func parseArgs(argv []string) (*cliArgs, error) {
	a := &cliArgs{outputFormat: "text"}
	i := 0
	next := func(flag string) (string, error) {
		i++
		if i >= len(argv) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		return argv[i], nil
	}
	for ; i < len(argv); i++ {
		arg := argv[i]
		val := ""
		if j := strings.IndexByte(arg, '='); j >= 0 && strings.HasPrefix(arg, "--") {
			arg, val = arg[:j], arg[j+1:]
		}
		get := func() (string, error) {
			if val != "" {
				return val, nil
			}
			return next(arg)
		}
		var err error
		switch arg {
		case "-c", "--continue":
			a.continueLatest = true
		case "-r", "--resume":
			if val != "" {
				a.resumeID = val
			} else if i+1 < len(argv) && !strings.HasPrefix(argv[i+1], "-") {
				i++
				a.resumeID = argv[i]
			} else {
				a.resumePicker = true
			}
		case "--session-id":
			a.sessionID, err = get()
		case "--fork-session":
			a.forkSession = true
		case "--model":
			a.model, err = get()
		case "--permission-mode":
			a.permMode, err = get()
		case "--dangerously-skip-permissions":
			a.permMode = tools.ModeBypass
		case "--settings", "-config", "--config":
			a.settings, err = get()
		case "--system-prompt":
			a.systemPrompt, err = get()
		case "--append-system-prompt":
			a.appendSystem, err = get()
		case "--add-dir":
			var d string
			d, err = get()
			a.addDirs = append(a.addDirs, d)
		case "--mcp-config":
			a.mcpConfig, err = get()
		case "-p", "--print":
			a.printPrompt, err = get()
		case "--output-format":
			a.outputFormat, err = get()
		case "--max-turns":
			var n string
			n, err = get()
			if err == nil {
				a.maxTurns, err = strconv.Atoi(n)
			}
		case "--list-sessions", "-list":
			a.listSessions = true
		case "-v", "--version":
			fmt.Println("tapioca " + version)
			os.Exit(0)
		case "-h", "--help":
			fmt.Println(usage)
			os.Exit(0)
		default:
			return nil, fmt.Errorf("unknown flag %q\n%s", arg, usage)
		}
		if err != nil {
			return nil, err
		}
	}
	if a.outputFormat != "text" && a.outputFormat != "json" {
		return nil, fmt.Errorf("--output-format must be text or json")
	}
	return a, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

func main() {
	args, err := parseArgs(os.Args[1:])
	if err != nil {
		fail(err)
	}

	if args.listSessions {
		metas, err := session.List()
		if err != nil {
			fail(err)
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

	cfg, err := config.Load(args.settings)
	if err != nil {
		fail(err)
	}
	catalog.Load()
	go catalog.Refresh()
	if args.model != "" {
		provName, model := cfg.DefaultProvider, args.model
		if p, rest, ok := strings.Cut(args.model, ":"); ok {
			if _, exists := cfg.Providers[p]; exists && rest != "" {
				provName, model = p, rest
			}
		}
		cfg.DefaultProvider, cfg.DefaultModel = provName, model
	}
	if args.systemPrompt != "" {
		cfg.SystemPrompt = args.systemPrompt
	}
	if args.appendSystem != "" {
		cfg.SystemPrompt += "\n\n" + args.appendSystem
	}
	if args.mcpConfig != "" {
		var extra struct {
			MCP []config.MCPServerConfig `toml:"mcp"`
		}
		if _, err := toml.DecodeFile(args.mcpConfig, &extra); err != nil {
			fail(fmt.Errorf("parsing %s: %w", args.mcpConfig, err))
		}
		cfg.MCP = extra.MCP
	}

	mode := cfg.PermissionMode
	if args.permMode != "" {
		mode = tools.NormalizeMode(args.permMode)
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd, _ = os.UserHomeDir()
	}
	exec := tools.NewExecutor(cwd, mode)
	exec.SetExtraDirs(args.addDirs)
	exec.SetCheckpoint(func(label string) {
		_, _ = checkpoint.Snapshot(exec.Cwd(), label)
	})

	reg := mcp.NewRegistry()
	defer reg.CloseAll()
	mgr := agent.NewManager(cfg, reg, exec)

	sessID := session.NewID()
	sessName := ""
	created := time.Now()

	resumeID := args.resumeID
	if args.continueLatest {
		id, err := session.LatestID()
		if err != nil {
			fail(err)
		}
		resumeID = id
	}
	if args.sessionID != "" {
		if strings.ContainsAny(args.sessionID, "/\\") {
			fail(fmt.Errorf("invalid session id %q", args.sessionID))
		}
		if _, err := session.Load(args.sessionID); err == nil {
			resumeID = args.sessionID
		} else {
			sessID = args.sessionID
		}
	}
	if resumeID != "" {
		s, err := session.Load(resumeID)
		if err != nil {
			fail(err)
		}
		mgr.LoadSession(s)
		sessID, sessName, created = s.ID, s.Name, s.CreatedAt
		if args.forkSession {
			sessID = session.NewID()
		}
	} else {
		mgr.NewAgent()
	}

	if args.printPrompt != "" {
		os.Exit(runPrint(cfg, mgr, reg, printOpts{
			prompt:   args.printPrompt,
			format:   args.outputFormat,
			maxTurns: args.maxTurns,
			sessID:   sessID,
			sessName: sessName,
			created:  created,
		}))
	}

	ui.SetMarkdownDark(lipgloss.HasDarkBackground())

	app := ui.NewApp(cfg, mgr, sessID, sessName, created)
	if args.resumePicker {
		app.StartWithSessionPicker()
	}
	p := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fail(err)
	}
}
