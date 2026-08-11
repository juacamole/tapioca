package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"tapioca/internal/agent"
	"tapioca/internal/config"
	"tapioca/internal/mcp"
	"tapioca/internal/provider"
	"tapioca/internal/tools"
)

type printOpts struct {
	prompt   string
	format   string
	maxTurns int
	sessID   string
	sessName string
	created  time.Time
}

type printResult struct {
	Result           string `json:"result"`
	SessionID        string `json:"session_id"`
	InputTokens      int    `json:"input_tokens"`
	OutputTokens     int    `json:"output_tokens"`
	CacheReadTokens  int    `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int    `json:"cache_write_tokens,omitempty"`
	ToolCalls        int    `json:"tool_calls"`
	DurationMS       int64  `json:"duration_ms"`
	Error            string `json:"error,omitempty"`
}

// runPrint executes one prompt headlessly: stream to stdout (text) or emit a
// JSON summary, save the session, exit 0/1.
// plain strips what a terminal would act on. --print writes provider and model
// output straight to a terminal, so tool names, arguments and error bodies get
// the same treatment the TUI gives them.
func plain(s string) string {
	return strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return -1
		}
		return r
	}, s)
}

func runPrint(cfg *config.Config, mgr *agent.Manager, reg *mcp.Registry, opts printOpts) int {
	for _, sc := range cfg.MCP {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		c, err := mcp.Start(ctx, sc)
		cancel()
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcp %s: %v\n", sc.Name, err)
			continue
		}
		reg.Add(c)
	}

	a := mgr.ActiveAgent()
	if a == nil {
		fmt.Fprintln(os.Stderr, "error: no agent")
		return 1
	}
	if opts.maxTurns > 0 {
		a.MaxToolRounds = opts.maxTurns
	}
	if a.Model == "" && a.Provider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if models, err := a.Provider.ListModels(ctx); err == nil && len(models) > 0 {
			a.Model = models[0]
		}
		cancel()
	}
	if a.Model == "" {
		fmt.Fprintln(os.Stderr, "error: no model available — set default_model or --model")
		return 1
	}

	a.Messages = append(a.Messages, provider.TextMessage("user", opts.prompt))
	if opts.sessName == "" {
		opts.sessName = opts.prompt
		if len(opts.sessName) > 48 {
			opts.sessName = opts.sessName[:48]
		}
	}
	history := make([]provider.Message, len(a.Messages))
	copy(history, a.Messages)
	start := time.Now()
	a.Send(history)

	res := printResult{SessionID: opts.sessID}
	var out strings.Builder
	streaming := opts.format == "text"

loop:
	for ev := range a.Events {
		switch ev.Kind {
		case agent.EvTextDelta:
			out.WriteString(ev.Text)
			if streaming {
				fmt.Print(ev.Text)
			}
		case agent.EvMessage:
			if ev.Message != nil {
				a.Messages = append(a.Messages, *ev.Message)
			}
		case agent.EvUsage:
			if ev.Usage != nil {
				u := ev.Usage
				res.InputTokens += u.InputTokens
				res.OutputTokens += u.OutputTokens
				res.CacheReadTokens += u.CacheReadTokens
				res.CacheWriteTokens += u.CacheWriteTokens
				a.Stats.AddRequest(ev.Provider, ev.Model, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, ev.Dur)
				a.CtxTokens = u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheWriteTokens
			}
		case agent.EvToolStart:
			res.ToolCalls++
			if ev.Tool != nil && streaming {
				// Names and arguments are the model's; -p usually runs in a
				// terminal, so control sequences are stripped rather than
				// handed to it.
				fmt.Fprintf(os.Stderr, "[tool] %s %s\n", plain(ev.Tool.Name), plain(ev.Tool.Args))
			}
		case agent.EvPermission:
			if ev.Perm != nil {
				fmt.Fprintf(os.Stderr, "[denied] %s — use --permission-mode auto|bypass for non-interactive tool runs\n", ev.Perm.Tool)
				ev.Perm.Reply <- tools.Decision{}
			}
		case agent.EvSpawn:
			// Subagents need a frontend to run them; without an answer here
			// the delegating agent would block forever.
			if ev.Spawn != nil {
				ev.Spawn.Reply <- agent.SpawnResult{
					Err: errors.New("delegation is not available in headless mode; do the work yourself"),
				}
			}
		case agent.EvError:
			if ev.Err != nil {
				res.Error = ev.Err.Error()
			}
		case agent.EvDone:
			break loop
		}
	}
	res.Result = out.String()
	res.DurationMS = time.Since(start).Milliseconds()

	if cfg.Autosave {
		if err := mgr.ToSession(opts.sessID, opts.sessName, opts.created).Save(); err != nil {
			fmt.Fprintf(os.Stderr, "saving session: %v\n", err)
		}
	}

	switch opts.format {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	default:
		if !strings.HasSuffix(res.Result, "\n") {
			fmt.Println()
		}
	}
	if res.Error != "" {
		if opts.format == "text" {
			fmt.Fprintln(os.Stderr, "error:", plain(res.Error))
		}
		return 1
	}
	return 0
}
