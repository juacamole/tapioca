package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"tapioca/internal/config"
)

// TestStdioHelperProcess is not a real test: re-executed as a child, it acts
// as an MCP server on stdio so the transport is exercised against a genuine
// process rather than a stub.
func TestStdioHelperProcess(t *testing.T) {
	if os.Getenv("TAPIOCA_MCP_HELPER") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req rpcMsg
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result string
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2024-11-05","capabilities":{}}`
		case "tools/list":
			result = `{"tools":[{"name":"local_echo","description":"d","inputSchema":{"type":"object"}}]}`
		case "tools/call":
			result = `{"content":[{"type":"text","text":"from stdio"}]}`
		default:
			result = `{}`
		}
		fmt.Printf("{\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":%s}\n", req.ID, result)
	}
	os.Exit(0)
}

func TestStdioTransportStillWorks(t *testing.T) {
	c, err := Start(context.Background(), config.MCPServerConfig{
		Name:    "local",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestStdioHelperProcess"},
		Env:     map[string]string{"TAPIOCA_MCP_HELPER": "1"},
	})
	if err != nil {
		t.Fatalf("starting stdio server: %v", err)
	}
	defer c.Close()

	if len(c.Tools()) != 1 || c.Tools()[0].Name != "local_echo" {
		t.Fatalf("tools not listed over stdio: %+v", c.Tools())
	}
	out, isErr, err := c.CallTool(context.Background(), "local_echo", nil)
	if err != nil || isErr || out != "from stdio" {
		t.Fatalf("CallTool = %q, %v, %v", out, isErr, err)
	}
	if !c.Alive() {
		t.Error("client reports dead while the process runs")
	}
}
