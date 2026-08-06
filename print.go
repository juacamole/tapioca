package main

import (
	"context"
	"encoding/json"
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
				res.InputTokens += ev.Usage.InputTokens
				res.OutputTokens += ev.Usage.OutputTokens
				res.CacheReadTokens += ev.Usage.CacheReadTokens
				res.CacheWriteTokens += ev.Usage.CacheWriteTokens
			}
		case agent.EvToolStart:
			res.ToolCalls++
			if ev.Tool != nil && streaming {
				fmt.Fprintf(os.Stderr, "[tool] %s %s\n", ev.Tool.Name, ev.Tool.Args)
			}
		case agent.EvPermission:
			if ev.Perm != nil {
				fmt.Fprintf(os.Stderr, "[denied] %s — use --permission-mode auto|bypass for non-interactive tool runs\n", ev.Perm.Tool)
				ev.Perm.Reply <- tools.Decision{}
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
			fmt.Fprintln(os.Stderr, "error:", res.Error)
		}
		return 1
	}
	return 0
}
