package acp

import (
	"context"
	"encoding/json"
	"strings"

	"tapioca/internal/agent"
	"tapioca/internal/tools"
)

// A permission request arriving over ACP is the case the deny rules exist for.
// The call is about to run in a process Tapioca did not write and cannot
// inspect, on the working tree it shares — so it goes through the same gate a
// local call does: the configured rules first (a deny holds under bypass), then
// the mode, then the session's grants, then the user. Nothing here can approve
// what tools.Approve would have refused; all this code does is work out which
// of Tapioca's tools the request is asking to be judged as.

// builtinFor maps ACP's tool kinds onto the tool names permission rules are
// written against, so deny = ["bash(rm *)"] catches an external agent's shell
// command exactly as it catches Tapioca's own. A delete and a move answer to
// write_file: they are mutations of a path, and that is the rule a user writes
// to protect one. Kinds with no counterpart here get an acp: key of their own
// instead, so they are still addressable — and, having no counterpart, are
// treated as mutating and asked about.
var builtinFor = map[string]string{
	"execute": "bash",
	"read":    "read_file",
	"edit":    "edit_file",
	"delete":  "write_file",
	"move":    "write_file",
	"search":  "grep",
	"fetch":   "web_fetch",
}

// toolNameFor names a call for the transcript and the tool panel. External
// work shows up under the same names as local work, because it answers to the
// same rules.
func toolNameFor(kind string) string {
	if name, ok := builtinFor[kind]; ok {
		return name
	}
	if strings.TrimSpace(kind) == "" {
		kind = "other"
	}
	return "acp:" + kind
}

// subjectKeys are where agents put the thing a rule matches on, per tool kind.
// ACP does not specify rawInput, so the field names are the ones agents
// actually use; the title is the last resort, and it is usually the command or
// the path spelled out for a human anyway.
var subjectKeys = map[string][]string{
	"bash":       {"command", "cmd", "script"},
	"read_file":  {"path", "file_path", "abs_path", "filePath", "absPath"},
	"edit_file":  {"path", "file_path", "abs_path", "filePath", "absPath"},
	"write_file": {"path", "file_path", "abs_path", "filePath", "absPath"},
	"grep":       {"pattern", "query", "path"},
	"web_fetch":  {"url", "uri"},
}

// permissionRequest is what the agent sends to ask.
type permissionRequest struct {
	SessionID string         `json:"sessionId"`
	ToolCall  toolCallUpdate `json:"toolCall"`
	Options   []struct {
		ID   string `json:"optionId"`
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"options"`
}

// handlePermission answers one session/request_permission. It runs off the
// read loop, because it blocks on a person and the connection must keep
// reading while it does.
func (c *Client) handlePermission(ctx context.Context, id, params json.RawMessage) {
	var req permissionRequest
	if json.Unmarshal(params, &req) != nil {
		c.conn.fail(id, -32602, "malformed permission request")
		return
	}
	denial, allowed := c.decide(ctx, req)
	if allowed {
		// Allow once, even when the user answered "always". That memory is
		// kept here, in the executor, where a deny rule still outranks it;
		// telling the agent to stop asking would move it into a process the
		// rules cannot reach. Only an agent offering no allow-once at all
		// gets the standing answer.
		if opt, ok := req.option("allow_once", "allow_always"); ok {
			c.conn.respond(id, map[string]any{"outcome": map[string]any{
				"outcome": "selected", "optionId": opt,
			}})
			return
		}
		c.conn.fail(id, -32602, "no allow option was offered")
		return
	}
	if opt, ok := req.option("reject_once", "reject_always"); ok {
		c.conn.respond(id, map[string]any{"outcome": map[string]any{
			"outcome": "selected", "optionId": opt,
		}})
	} else {
		// An agent that offered no way to say no is told the request is off
		// the table, which is the protocol's other way of not doing it.
		c.conn.respond(id, map[string]any{"outcome": map[string]any{"outcome": "cancelled"}})
	}
	c.emit(agent.Event{Kind: agent.EvNotice, Text: c.Name + ": " + denial})
}

// decide runs the request through Tapioca's gate.
func (c *Client) decide(ctx context.Context, req permissionRequest) (denial string, allowed bool) {
	if c.gate == nil {
		// No executor means no rules, no mode and no record of what was
		// granted. There is nothing to judge the request with, so it is
		// refused rather than waved through.
		return "no permission gate is configured", false
	}
	name, subject := requestSubject(req.ToolCall)
	external := strings.HasPrefix(name, "acp:")
	// What the gate and the hooks are handed: the arguments a built-in of this
	// name would have been called with, or the agent's own for a tool with no
	// counterpart here.
	raw := subjectJSON(name, subject)
	if external && len(req.ToolCall.Raw) > 0 {
		raw = req.ToolCall.Raw
	}
	ask := c.asker(ctx)
	if external {
		denial, allowed = c.gate.ApproveExternal(name, subject, ask)
	} else {
		denial, allowed = c.gate.Approve(name, raw, ask)
	}
	if !allowed {
		return denial, false
	}
	// A pre_tool hook is the user's own last word on a call, and leaving it
	// out for the one agent Tapioca did not write would be the hole it exists
	// to close. There is no post_tool counterpart: this call runs somewhere
	// else, and a note for the tool result has nowhere to go.
	if reason, blocked := c.gate.RunPreToolHooks(ctx, name, raw); blocked {
		return reason, false
	}
	return "", true
}

// asker puts the prompt in front of the user through the tab. The tool name
// goes through untouched: the prompt already names the tab it came from, and
// the affordances behind it — always-allow this path, this host, this bash
// prefix — key off the plain name. An external agent's request is meant to be
// the same prompt as a local one, not one that looks like it.
//
// The context bounds the wait: a cancelled turn, or an agent that died
// mid-question, releases the prompt as a refusal.
func (c *Client) asker(ctx context.Context) tools.AskFunc {
	a := c.tab()
	return func(tool, summary string) tools.Decision {
		if a == nil {
			return tools.Decision{}
		}
		return a.Ask(ctx, tool, summary)
	}
}

// requestSubject works out which tool the request should be judged as, and
// what the rules should match against.
func requestSubject(call toolCallUpdate) (name, subject string) {
	name = toolNameFor(call.Kind)
	if strings.HasPrefix(name, "acp:") {
		// Nothing of Tapioca's does this, so the rules see the arguments
		// whole, as they do for an MCP tool.
		if len(call.Raw) > 0 {
			return name, string(call.Raw)
		}
		return name, call.Title
	}
	if s := firstField(call.Raw, subjectKeys[name]...); s != "" {
		return name, s
	}
	// A file tool's locations say what it is about when its arguments do not.
	if len(call.Locations) > 0 && call.Locations[0].Path != "" {
		return name, call.Locations[0].Path
	}
	return name, strings.TrimSpace(call.Title)
}

// subjectJSON rebuilds the arguments the built-in gate reads, so a mapped call
// is matched, prompted and checkpointed as the tool it stands in for.
func subjectJSON(name, subject string) json.RawMessage {
	key := "path"
	switch name {
	case "bash":
		key = "command"
	case "web_fetch":
		key = "url"
	case "grep":
		key = "pattern"
	}
	return json.RawMessage(`{"` + key + `":` + quoteJSON(subject) + `}`)
}

// firstField returns the first named string field carrying anything.
func firstField(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 || len(keys) == 0 {
		return ""
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return ""
	}
	for _, k := range keys {
		var s string
		if v, ok := fields[k]; ok && json.Unmarshal(v, &s) == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// option finds an option of one of the given kinds, preferring the first named.
func (r permissionRequest) option(kinds ...string) (string, bool) {
	for _, want := range kinds {
		for _, o := range r.Options {
			if o.Kind == want && o.ID != "" {
				return o.ID, true
			}
		}
	}
	return "", false
}
