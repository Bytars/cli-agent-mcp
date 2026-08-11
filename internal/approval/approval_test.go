package approval

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callApprove drives the endpoint the way Claude Code does: connect over
// streamable HTTP, call the permission tool, read the JSON verdict out of the
// text block.
func callApprove(t *testing.T, url, toolName string, input map[string]any) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "fake-worker", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect to the approval endpoint: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: map[string]any{"tool_name": "Bash", "input": input},
	})
	if err != nil {
		t.Fatalf("calling the approval tool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("the approval tool returned no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected a text block, got %T", res.Content[0])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatalf("the verdict must be JSON a permission prompt tool understands, got %q: %v", text.Text, err)
	}
	return decoded
}

func TestApproveAllowsAndEchoesTheInput(t *testing.T) {
	var seen Request
	b, err := Start(t.TempDir(), func(_ context.Context, r Request) Decision {
		seen = r
		return Decision{Allow: true}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Close()

	g, err := b.NewGrant("task-42")
	if err != nil {
		t.Fatalf("NewGrant: %v", err)
	}
	defer g.Close()

	url := grantURL(t, g)
	got := callApprove(t, url, g.ToolName, map[string]any{"command": "go test ./..."})

	if got["behavior"] != "allow" {
		t.Errorf("behavior = %v, want allow", got["behavior"])
	}
	// An allow must echo the arguments back. Returning nothing there is read as
	// "allow, with no arguments", which turns the approved command into a
	// different one.
	updated, _ := got["updatedInput"].(map[string]any)
	if updated["command"] != "go test ./..." {
		t.Errorf("updatedInput = %v, want the original arguments echoed back", got["updatedInput"])
	}
	if seen.TaskID != "task-42" || seen.ToolName != "Bash" {
		t.Errorf("the decider saw %+v, want the asking task and tool", seen)
	}
	if seen.Input["command"] != "go test ./..." {
		t.Errorf("the decider saw input %v, want the command the agent asked to run", seen.Input)
	}
}

// Claude Code sends tool_use_id alongside tool_name and input, and a schema
// generated from a Go struct rejects it as an unexpected property. The failure
// that produces is genuinely misleading: the agent reports "Error calling tool
// (Bash): unexpected additional properties [tool_use_id]" and concludes the Bash
// tool is broken, while nothing at all points at the permission tool. This is
// the exact payload, so the schema can never quietly tighten again.
func TestApproveAcceptsTheFieldsClaudeCodeActuallySends(t *testing.T) {
	var seen Request
	b, err := Start(t.TempDir(), func(_ context.Context, r Request) Decision {
		seen = r
		return Decision{Allow: true}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Close()

	g, _ := b.NewGrant("task-1")
	defer g.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "fake-worker", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: grantURL(t, g)}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: g.ToolName,
		Arguments: map[string]any{
			"tool_name":   "Bash",
			"input":       map[string]any{"command": "mkdir -p out", "description": "make a directory"},
			"tool_use_id": "toolu_01abc",
			// A field we do not know about yet must not break the call either.
			"future_field": "whatever",
		},
	})
	if err != nil {
		t.Fatalf("the payload Claude Code sends was rejected: %v", err)
	}
	if res.IsError {
		t.Fatalf("the call came back as an error: %s", res.Content)
	}
	if seen.ToolUseID != "toolu_01abc" {
		t.Errorf("ToolUseID = %q, want the agent's own call id", seen.ToolUseID)
	}
	if seen.Input["command"] != "mkdir -p out" {
		t.Errorf("Input = %v, want the command the agent asked to run", seen.Input)
	}
}

func TestApproveDeniesWithAReason(t *testing.T) {
	b, err := Start(t.TempDir(), func(context.Context, Request) Decision {
		return Decision{Message: "the user said no"}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Close()

	g, _ := b.NewGrant("task-1")
	defer g.Close()

	got := callApprove(t, grantURL(t, g), g.ToolName, map[string]any{"command": "rm -rf /"})
	if got["behavior"] != "deny" {
		t.Fatalf("behavior = %v, want deny", got["behavior"])
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "the user said no") {
		t.Errorf("message = %q, want the reason passed through to the agent", msg)
	}
}

// The grant is the credential. Anything that can reach the endpoint can approve
// commands on this machine, so a revoked token must approve nothing — and a
// worker that outlives its turn must not be able to keep asking.
func TestRevokedGrantApprovesNothing(t *testing.T) {
	b, err := Start(t.TempDir(), func(context.Context, Request) Decision {
		return Decision{Allow: true}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Close()

	g, _ := b.NewGrant("task-1")
	url := grantURL(t, g)
	g.Close()

	got := callApprove(t, url, g.ToolName, map[string]any{"command": "whoami"})
	if got["behavior"] != "deny" {
		t.Errorf("a revoked grant returned %v; it must approve nothing", got["behavior"])
	}
}

func TestUnknownTokenApprovesNothing(t *testing.T) {
	b, err := Start(t.TempDir(), func(context.Context, Request) Decision {
		return Decision{Allow: true}
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Close()

	got := callApprove(t, b.Addr()+"/t/0000000000000000", toolName, map[string]any{"command": "whoami"})
	if got["behavior"] != "deny" {
		t.Errorf("an unknown token returned %v; it must approve nothing", got["behavior"])
	}
}

// The config file is what the agent is launched with, so its shape is part of
// the contract with Claude Code rather than an internal detail.
func TestGrantWritesTheConfigTheAgentIsLaunchedWith(t *testing.T) {
	b, err := Start(t.TempDir(), func(context.Context, Request) Decision { return Decision{} })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer b.Close()

	g, err := b.NewGrant("task-1")
	if err != nil {
		t.Fatalf("NewGrant: %v", err)
	}

	var cfg mcpConfig
	readJSON(t, g.ConfigPath, &cfg)
	entry, ok := cfg.MCPServers[g.ServerName]
	if !ok {
		t.Fatalf("config has no entry for %q: %+v", g.ServerName, cfg.MCPServers)
	}
	if entry.Type != "http" || !strings.HasPrefix(entry.URL, "http://127.0.0.1:") {
		t.Errorf("entry = %+v, want an http server on loopback", entry)
	}
	if want := "mcp__" + g.ServerName + "__" + g.ToolName; g.PermissionTool() != want {
		t.Errorf("PermissionTool() = %q, want %q", g.PermissionTool(), want)
	}

	// Closing must take the file with it: it holds the token.
	g.Close()
	if fileExists(g.ConfigPath) {
		t.Error("the config file outlived its grant, leaving a live token on disk")
	}
}

// grantURL reads back the endpoint the agent would be pointed at, from the very
// config file it would be launched with.
func grantURL(t *testing.T, g *Grant) string {
	t.Helper()
	var cfg mcpConfig
	readJSON(t, g.ConfigPath, &cfg)
	return cfg.MCPServers[g.ServerName].URL
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := json.Unmarshal(buf, v); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
