// Tapioca is an agentic coding TUI for local and hosted LLMs.
package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"tapioca/internal/acp"
	"tapioca/internal/agent"
	"tapioca/internal/catalog"
	"tapioca/internal/checkpoint"
	"tapioca/internal/config"
	"tapioca/internal/lsp"
	"tapioca/internal/mcp"
	"tapioca/internal/secretenv"
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
	sandbox        bool
	acp            bool
	forkSession    bool
	listSessions   bool
}

// progName is the name the binary was invoked under. The same program ships as
// both `tapioca` and `tapio`, and help that names the other one is confusing.
func progName() string {
	if len(os.Args) == 0 || os.Args[0] == "" {
		return "tapioca"
	}
	name := filepath.Base(os.Args[0])
	return strings.TrimSuffix(name, ".exe")
}

func usageText() string { return "Usage: " + progName() + usageBody }

const usageBody = ` [options]

Options:
  -c, --continue                     Continue the most recent session
  -r, --resume [id]                  Resume a session (picker when no id)
      --session-id <id>              Use a specific session id
      --fork-session                 Resume into a new session id
      --model [provider:]<model>     Model for this run
      --permission-mode <mode>       plan | manual | auto | bypass
      --dangerously-skip-permissions Run all tools without asking (bypass)
      --sandbox                      Confine bash to the working tree (bubblewrap)
      --settings <file>              Config file (default ~/.config/tapioca/config.toml)
      --system-prompt <text>         Replace the system prompt
      --append-system-prompt <text>  Append to the system prompt
      --add-dir <dir>                Announce an additional working directory (repeatable)
      --mcp-config <file>            Load MCP servers from this TOML file instead
  -p, --print <prompt>               Non-interactive: run one prompt, print, exit
      --output-format <fmt>          For -p: text (default) or json
      --max-turns <n>                For -p: cap agentic tool rounds
      --list-sessions                List saved sessions and exit
      --acp                          Serve the Agent Client Protocol on stdio (for editors)
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
		case "--sandbox":
			a.sandbox = true
		case "--acp":
			a.acp = true
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
			fmt.Println(progName() + " " + version)
			os.Exit(0)
		case "-h", "--help":
			fmt.Println(usageText())
			os.Exit(0)
		default:
			return nil, fmt.Errorf("unknown flag %q\n%s", arg, usageText())
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

// Main is the whole program. It lives here rather than in package main so
// that more than one binary name can share it: the Shopify tapioca gem also
// installs a `tapioca`, so this ships as `tapio` too.
func Main() {
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
			where := m.Cwd
			if where == "" {
				where = "(no project recorded)"
			}
			fmt.Printf("%s  %-32s  %d agents  %d msgs  %s  %s\n",
				m.ID, name, m.Agents, m.Messages, m.UpdatedAt.Format("2006-01-02 15:04"), where)
		}
		return
	}

	cfg, err := config.Load(args.settings)
	if err != nil {
		fail(err)
	}
	catalog.Load()
	if cfg.ModelCatalog {
		// One request to models.dev for pricing and context sizes; disable
		// with model_catalog = false for a fully offline run.
		go catalog.Refresh()
	}
	// Flag overrides are runtime-only: they apply to agents (or via presave
	// exclusion), never to the config file — a shift+tab mid-run must not
	// bake --model or a scratch MCP list into the user's config.
	// SetPresave takes one function, so the exclusions are composed here
	// rather than each flag overwriting the previous one's.
	var presaves []func(*config.Config)
	origMCP := cfg.MCP
	if args.mcpConfig != "" {
		var extra struct {
			MCP []config.MCPServerConfig `toml:"mcp"`
		}
		if _, err := toml.DecodeFile(args.mcpConfig, &extra); err != nil {
			fail(fmt.Errorf("parsing %s: %w", args.mcpConfig, err))
		}
		cfg.MCP = extra.MCP
		presaves = append(presaves, func(c *config.Config) { c.MCP = origMCP })
	}

	mode := cfg.PermissionMode
	if args.permMode != "" {
		mode = tools.NormalizeMode(args.permMode)
	}
	cwd, err := os.Getwd()
	if err != nil {
		cwd, _ = os.UserHomeDir()
	}
	secretenv.SetExtra(cfg.SecretEnv)
	exec := tools.NewExecutor(cwd, mode)
	exec.SetExtraDirs(args.addDirs)
	exec.SetBashPrefixes(cfg.BashAllow)
	exec.SetRules(cfg.Permissions.Allow, cfg.Permissions.Ask, cfg.Permissions.Deny)
	if args.sandbox && !cfg.Sandbox {
		cfg.Sandbox = true
		presaves = append(presaves, func(c *config.Config) { c.Sandbox = false })
	}
	if len(presaves) > 0 {
		cfg.SetPresave(func(c *config.Config) {
			for _, fn := range presaves {
				fn(c)
			}
		})
	}
	// ACP owns stdout as the protocol channel, so it runs before anything
	// that might print there, and builds its own per-session executors.
	if args.acp {
		if err := acp.Serve(cfg, os.Stdin, os.Stdout); err != nil {
			fail(err)
		}
		return
	}
	exec.SetSandbox(cfg.Sandbox)
	exec.SetSandboxNetwork(cfg.SandboxNetwork)
	exec.SetTimeout(time.Duration(cfg.BashTimeout) * time.Second)
	if cfg.Sandbox && !tools.SandboxAvailable() {
		fmt.Fprintln(os.Stderr, "warning: sandbox is enabled but bubblewrap (bwrap) was not found — bash calls will fail until it is installed")
	}
	if checkpoint.Available() {
		exec.SetCheckpoint(func(label string) {
			_, _ = checkpoint.Snapshot(exec.Cwd(), label)
		})
	} else {
		fmt.Fprintln(os.Stderr, "warning: git not found — file checkpoints and /rewind are disabled")
	}

	reg := mcp.NewRegistry()
	defer reg.CloseAll()
	defer exec.StopJobs() // nothing keeps running after the app exits
	if len(cfg.LSP) > 0 {
		lsps := lsp.NewRegistry(cfg.LSP, cwd)
		defer lsps.CloseAll()
		exec.SetDiagnostics(lsps.Check)
		go lsps.Warm() // load the workspace before the first edit needs it
	}
	mgr := agent.NewManager(cfg, reg, exec)
	// Only the TUI can run a subagent (it owns the tabs and their event
	// pumps), so only it advertises the tool. Headless runs below never do.
	mgr.AllowSpawn = args.printPrompt == ""

	sessID := session.NewID()
	sessName := ""
	created := time.Now()

	resumeID := args.resumeID
	// Both of these become a filename, so both get the same check.
	if strings.ContainsAny(resumeID, "/\\") {
		fail(fmt.Errorf("invalid session id %q", resumeID))
	}
	if args.continueLatest {
		id, err := session.LatestID(cwd)
		if err != nil {
			fail(err)
		}
		resumeID = id
	}
	if args.sessionID != "" {
		if strings.ContainsAny(args.sessionID, "/\\") {
			fail(fmt.Errorf("invalid session id %q", args.sessionID))
		}
		switch _, err := session.Load(args.sessionID); {
		case err == nil:
			resumeID = args.sessionID
		case errors.Is(err, os.ErrNotExist):
			sessID = args.sessionID
		default:
			// Corrupt-but-existing: refuse to silently overwrite it.
			fail(err)
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

	if args.model != "" {
		provName, model := cfg.DefaultProvider, args.model
		if p, rest, ok := strings.Cut(args.model, ":"); ok {
			if _, exists := cfg.Providers[p]; exists && rest != "" {
				provName, model = p, rest
			}
		}
		prov, err := mgr.ProviderFor(provName)
		if err != nil {
			fail(err)
		}
		for _, a := range mgr.Agents {
			a.Provider, a.ProviderName, a.ProviderErr, a.Model = prov, provName, "", model
		}
	}
	if args.systemPrompt != "" || args.appendSystem != "" {
		for _, a := range mgr.Agents {
			if args.systemPrompt != "" {
				a.SystemPrompt = args.systemPrompt
			}
			if args.appendSystem != "" {
				a.SystemPrompt += "\n\n" + args.appendSystem
			}
		}
	}

	if args.printPrompt != "" {
		code := runPrint(cfg, mgr, reg, printOpts{
			prompt:   args.printPrompt,
			format:   args.outputFormat,
			maxTurns: args.maxTurns,
			sessID:   sessID,
			sessName: sessName,
			created:  created,
		})
		// os.Exit skips deferred calls, which would leave background jobs and
		// MCP servers running after the process is gone.
		exec.StopJobs()
		reg.CloseAll()
		os.Exit(code)
	}

	ui.SetMarkdownDark(lipgloss.HasDarkBackground())
	cfg.Theme = ui.SetTheme(cfg.Theme, cfg.Colors)
	cfg.Glyphs = ui.SetGlyphs(cfg.Glyphs)

	app := ui.NewApp(cfg, mgr, sessID, sessName, created)
	if args.resumePicker {
		app.StartWithSessionPicker()
	}
	p := tea.NewProgram(app, tea.WithAltScreen(), tea.WithMouseCellMotion())
	if _, err := p.Run(); err != nil {
		fail(err)
	}
}
