// Package approval closes the loop that makes a headless worker stall.
//
// A coding agent run with -p executes what it is allowed to and asks about the
// rest. Headless, there is nobody at the terminal to answer, so the run either
// hangs until something kills it or the operator pre-approves everything up
// front — trading the ability to say no for the ability to finish. Both are bad
// answers to a question that has a good one: there IS a person present, one
// conversation upstream, and MCP has a way to reach them.
//
// Claude Code's --permission-prompt-tool names an MCP tool it will call instead
// of prompting. This package is that tool. It serves a tiny MCP endpoint on
// loopback, hands the worker a generated config pointing at it, and turns each
// permission request into an elicitation on the session that delegated the task
// — so the user approves it where they already are.
//
// Two properties are deliberate. Each run gets a single-use URL with an
// unguessable token, revoked the moment the turn ends, because anything that
// can reach this endpoint can approve commands on this machine. And when the
// orchestrating client cannot elicit, requests are denied rather than allowed:
// a worker told "no" reports that it was blocked, which is recoverable, while a
// worker silently granted everything is not.
package approval

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Decision is the answer to one permission request.
type Decision struct {
	Allow   bool
	Message string // why, when denied — the worker sees this and can adapt
}

// Request is one tool call the worker wants to make.
type Request struct {
	TaskID   string
	ToolName string
	Input    map[string]any
}

// Decider answers a permission request. It is called off the broker's lock and
// may block for as long as its context allows.
type Decider func(ctx context.Context, req Request) Decision

// Broker serves the approval endpoint and tracks the grants that may use it.
type Broker struct {
	decide    Decider
	configDir string

	listener net.Listener
	srv      *http.Server
	baseURL  string

	mu     sync.Mutex
	grants map[string]string // token -> task id
}

// Grant is one run's permission to ask. It carries the path of the MCP config
// file the agent must be pointed at, and is void once closed.
type Grant struct {
	broker     *Broker
	token      string
	ConfigPath string
	ServerName string
	ToolName   string
}

// PermissionTool is the value for --permission-prompt-tool that matches the
// config this package writes.
func (g *Grant) PermissionTool() string {
	return "mcp__" + g.ServerName + "__" + g.ToolName
}

const (
	serverName = "cli_agent_approval"
	toolName   = "approve"
)

// Start brings up the approval endpoint on loopback and returns a broker.
//
// It binds to an ephemeral port on 127.0.0.1 specifically: the endpoint grants
// the power to run commands on this machine, so it must not be reachable from
// anywhere the machine itself is not.
func Start(configDir string, decide Decider) (*Broker, error) {
	if decide == nil {
		return nil, fmt.Errorf("approval: no decider")
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return nil, fmt.Errorf("approval: config dir %s: %w", configDir, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("approval: listen on loopback: %w", err)
	}

	b := &Broker{
		decide:    decide,
		configDir: configDir,
		listener:  ln,
		baseURL:   "http://" + ln.Addr().String(),
		grants:    map[string]string{},
	}

	handler := mcp.NewStreamableHTTPHandler(b.serverFor, &mcp.StreamableHTTPOptions{
		// The worker only ever calls a tool and reads the answer; there is
		// nothing for this server to push back, so a session adds no value and
		// one more thing to get wrong.
		Stateless: true,
	})

	mux := http.NewServeMux()
	mux.Handle("/t/", handler)

	b.srv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := b.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("approval endpoint stopped: %v", err)
		}
	}()
	return b, nil
}

// Addr reports where the endpoint is listening, for logging.
func (b *Broker) Addr() string { return b.baseURL }

// Close shuts the endpoint down.
func (b *Broker) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = b.srv.Shutdown(ctx)
}

// tokenFromPath extracts the grant token from /t/<token>[/...]. Routing on the
// path rather than a header is what lets the whole credential live in the one
// URL the generated config carries.
func tokenFromPath(p string) string {
	rest := strings.TrimPrefix(p, "/t/")
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

// serverFor builds the MCP server the worker talks to, bound to whichever task
// the request's token belongs to. An unknown or revoked token yields a server
// that refuses everything, so a stale config cannot approve anything.
func (b *Broker) serverFor(r *http.Request) *mcp.Server {
	token := tokenFromPath(r.URL.Path)

	b.mu.Lock()
	taskID, ok := b.grants[token]
	b.mu.Unlock()

	srv := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: "1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolName,
		Description: "Ask the human running this task whether the agent may perform a tool call.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in promptInput) (*mcp.CallToolResult, any, error) {
		if !ok {
			return decisionResult(Decision{
				Message: "this approval link is no longer valid (the task it belonged to has ended)",
			}, in.Input), nil, nil
		}
		d := b.decide(ctx, Request{TaskID: taskID, ToolName: in.ToolName, Input: in.Input})
		return decisionResult(d, in.Input), nil, nil
	})
	return srv
}

// promptInput is the shape Claude Code sends to a permission prompt tool.
type promptInput struct {
	ToolName string         `json:"tool_name" jsonschema:"The tool the agent wants to use."`
	Input    map[string]any `json:"input,omitempty" jsonschema:"The arguments it wants to call that tool with."`
}

// decisionResult encodes an answer the way a permission prompt tool must: a
// single text block holding the JSON verdict.
//
// An allow has to echo the input back as updatedInput. Returning nothing there
// is read as "allow, with no arguments", which turns an approved command into a
// different one.
func decisionResult(d Decision, input map[string]any) *mcp.CallToolResult {
	payload := map[string]any{"behavior": "deny", "message": d.Message}
	if payload["message"] == "" {
		payload["message"] = "denied"
	}
	if d.Allow {
		if input == nil {
			input = map[string]any{}
		}
		payload = map[string]any{"behavior": "allow", "updatedInput": input}
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		buf = []byte(`{"behavior":"deny","message":"could not encode the decision"}`)
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(buf)}}}
}

// mcpConfig is the file the agent is pointed at with --mcp-config. It is
// written to disk rather than passed inline because it is JSON full of quotes,
// and an argument like that is exactly what Windows launcher dispatch mangles.
type mcpConfig struct {
	MCPServers map[string]mcpServer `json:"mcpServers"`
}

type mcpServer struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

// NewGrant issues a single-use approval URL for one task and writes the config
// file the agent will be launched with.
func (b *Broker) NewGrant(taskID string) (*Grant, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, fmt.Errorf("approval: generating a token: %w", err)
	}
	token := hex.EncodeToString(raw[:])

	cfg := mcpConfig{MCPServers: map[string]mcpServer{
		serverName: {Type: "http", URL: b.baseURL + "/t/" + token},
	}}
	buf, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(b.configDir, "approval-"+token+".json")
	// 0600: the file contains the token, and the token is the authority.
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return nil, fmt.Errorf("approval: writing %s: %w", path, err)
	}

	b.mu.Lock()
	b.grants[token] = taskID
	b.mu.Unlock()

	return &Grant{broker: b, token: token, ConfigPath: path, ServerName: serverName, ToolName: toolName}, nil
}

// Close revokes the grant and removes its config file. After this the URL
// answers every request with a denial.
func (g *Grant) Close() {
	if g == nil || g.broker == nil {
		return
	}
	g.broker.mu.Lock()
	delete(g.broker.grants, g.token)
	g.broker.mu.Unlock()
	_ = os.Remove(g.ConfigPath)
}
