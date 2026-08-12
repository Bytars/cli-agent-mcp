package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andresh0816/cli-agent-mcp/internal/agent"
	"github.com/andresh0816/cli-agent-mcp/internal/config"
	"github.com/andresh0816/cli-agent-mcp/internal/grants"
	"github.com/andresh0816/cli-agent-mcp/internal/task"
	"github.com/andresh0816/cli-agent-mcp/internal/ui"
)

// connect stands up the real server over an in-memory transport, so these tests
// assert what a host actually receives on the wire rather than what the local
// structs happen to hold.
func connect(t *testing.T) *mcp.ClientSession {
	t.Helper()

	caps := &mcp.ServerCapabilities{Logging: &mcp.LoggingCapabilities{}}
	caps.AddExtension(ui.ExtensionName, ui.Capability())

	srv := mcp.NewServer(&mcp.Implementation{Name: "cli-agent-mcp", Version: "test"},
		&mcp.ServerOptions{Instructions: instructions, Capabilities: caps})
	registerTools(srv, agent.NewRegistry(agent.NewMockAdapter()), task.NewManager(10), config.Config{DefaultAgent: "mock"}, newPermissionDesk(time.Minute), &grants.Store{})

	ct, st := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { ss.Close() })

	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-host", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// A host decides whether to render views from the initialize result alone, so
// the extension has to survive the SDK's capability inference.
func TestServerAdvertisesUIExtension(t *testing.T) {
	cs := connect(t)
	caps := cs.InitializeResult().Capabilities

	settings, ok := caps.Extensions[ui.ExtensionName].(map[string]any)
	if !ok {
		t.Fatalf("extension %q missing from capabilities: %#v", ui.ExtensionName, caps.Extensions)
	}
	mimes, ok := settings["mimeTypes"].([]any)
	if !ok || len(mimes) == 0 {
		t.Fatalf("mimeTypes missing or empty: %#v", settings)
	}
	if got := mimes[0]; got != ui.MIMEType {
		t.Errorf("mimeTypes[0] = %v, want %q", got, ui.MIMEType)
	}
	// Declaring capabilities replaces the SDK's default set; losing logging or
	// resources here would be a silent regression.
	if caps.Logging == nil {
		t.Error("logging capability was dropped when Capabilities was set")
	}
	if caps.Resources == nil {
		t.Error("resources capability missing; the host cannot fetch the board")
	}
}

// The board is useless if the host can't fetch it, or fetches something it
// refuses to render.
func TestBoardResourceIsServed(t *testing.T) {
	cs := connect(t)
	res, err := cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: ui.ResourceURI})
	if err != nil {
		t.Fatalf("read %s: %v", ui.ResourceURI, err)
	}
	if len(res.Contents) != 1 {
		t.Fatalf("got %d contents, want 1", len(res.Contents))
	}
	c := res.Contents[0]
	if c.MIMEType != ui.MIMEType {
		t.Errorf("mimeType = %q, want %q", c.MIMEType, ui.MIMEType)
	}
	if !strings.Contains(c.Text, "ui/notifications/initialized") {
		t.Error("served HTML does not contain the handshake; the view would never connect")
	}
	// A deny-by-default CSP blocks external origins, and no csp allowances are
	// declared, so any external reference would silently fail to load.
	for _, bad := range []string{`src="http`, `src='http`, `href="http`, "@import"} {
		if strings.Contains(c.Text, bad) {
			t.Errorf("view references an external origin (%q) but declares no csp", bad)
		}
	}
}

// The link from tool to view is the whole mechanism: without _meta.ui.resourceUri
// the host renders plain text and never opens the board.
func TestBoardToolPointsAtTheView(t *testing.T) {
	cs := connect(t)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var found *mcp.Tool
	for _, tool := range tools.Tools {
		if tool.Name == "agent_task_board" {
			found = tool
		}
	}
	if found == nil {
		t.Fatal("agent_task_board is not registered")
	}
	uiMeta, ok := found.Meta["ui"].(map[string]any)
	if !ok {
		t.Fatalf("_meta.ui missing: %#v", found.Meta)
	}
	if got := uiMeta["resourceUri"]; got != ui.ResourceURI {
		t.Errorf("resourceUri = %v, want %q", got, ui.ResourceURI)
	}
}

// Bounding the watch is only half the fix. If the model is not told that an
// early return means "call me again", it reads running=true as the end of the
// story and the task goes unwatched — the same failure the bound was meant to
// remove.
func TestWatchGuidanceTellsTheModelToResume(t *testing.T) {
	cs := connect(t)
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var watch *mcp.Tool
	for _, tool := range tools.Tools {
		if tool.Name == "agent_watch" {
			watch = tool
		}
	}
	if watch == nil {
		t.Fatal("agent_watch is not registered")
	}
	for _, want := range []string{"running=true", "call agent_watch again", "until running is false"} {
		if !strings.Contains(watch.Description, want) {
			t.Errorf("agent_watch description never says %q:\n%s", want, watch.Description)
		}
	}
	for _, want := range []string{"running=true", "Call agent_watch again", "until running is false"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("server instructions never say %q", want)
		}
	}
	// The old guidance actively told the model not to come back. Leaving it in
	// place would override everything above.
	if strings.Contains(instructions, "Do NOT poll it in a loop") {
		t.Error("instructions still tell the model not to re-call agent_watch")
	}
}

// Hosts without view support show this text, so it has to carry the status on
// its own.
func TestBoardTextStandsAlone(t *testing.T) {
	if got := boardText(nil); got != "No tasks yet." {
		t.Errorf("empty board = %q", got)
	}
	exit := 1
	got := boardText([]task.Snapshot{
		{ID: "t1", Agent: "mock", Status: task.StatusRunning, TotalLines: 7, Prompts: []string{"fix the build"}},
		{ID: "t2", Agent: "mock", Status: task.StatusFailed, ExitCode: &exit, Error: "boom"},
	})
	for _, want := range []string{"2 task(s), 1 running", "t1", "running", "fix the build", "t2", "exit_code=1", "boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("board text missing %q:\n%s", want, got)
		}
	}
}
