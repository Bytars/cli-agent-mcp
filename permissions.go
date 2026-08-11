package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andresh0816/cli-agent-mcp/internal/approval"
	"github.com/andresh0816/cli-agent-mcp/internal/task"
)

// permissionDesk routes a worker's permission request back to the person who
// delegated the task.
//
// The two halves arrive from opposite directions and have to be joined
// somewhere: the approval endpoint knows which task is asking, and only the
// orchestrating session can put the question to a human. The desk holds the
// mapping between them for exactly as long as a turn is running.
type permissionDesk struct {
	broker  *approval.Broker
	timeout time.Duration

	mu       sync.Mutex
	sessions map[string]*mcp.ServerSession // task id -> who to ask
}

func newPermissionDesk(timeout time.Duration) *permissionDesk {
	return &permissionDesk{timeout: timeout, sessions: map[string]*mcp.ServerSession{}}
}

// Decide is the broker's Decider: it finds the session that owns the asking
// task and puts the question to it.
func (d *permissionDesk) Decide(ctx context.Context, req approval.Request) approval.Decision {
	d.mu.Lock()
	session := d.sessions[req.TaskID]
	d.mu.Unlock()

	if session == nil {
		return approval.Decision{Message: "there is no longer anyone connected who can approve this, so it was not run."}
	}
	return d.ask(ctx, session, req)
}

// Approver returns the approver for one client session, or nil when
// interactive approval is not available to it.
//
// nil rather than something that always denies, because "ask the user" and
// "refuse everything" are very different runs. A client that cannot elicit gets
// exactly the behaviour it had before: the operator's pre-approved tool list
// decides and anything outside it stalls — bad, but the failure the operator
// configured for, not a new one introduced here.
func (d *permissionDesk) Approver(session *mcp.ServerSession) task.Approver {
	if d == nil || d.broker == nil || session == nil || !canElicit(session) {
		return nil
	}
	return &sessionApprover{desk: d, session: session}
}

// canElicit reports whether the connected client told us during initialize that
// it can put a question to its user. Asking one that cannot would block every
// permission request until it timed out.
func canElicit(session *mcp.ServerSession) bool {
	params := session.InitializeParams()
	return params != nil && params.Capabilities != nil && params.Capabilities.Elicitation != nil
}

// sessionApprover is one client session's ability to answer for the tasks it
// started.
type sessionApprover struct {
	desk    *permissionDesk
	session *mcp.ServerSession
}

func (a *sessionApprover) Grant(taskID string) (string, string, func(), bool) {
	g, err := a.desk.broker.NewGrant(taskID)
	if err != nil {
		log.Printf("warning: interactive approval unavailable for task %s: %v", taskID, err)
		return "", "", nil, false
	}

	a.desk.mu.Lock()
	a.desk.sessions[taskID] = a.session
	a.desk.mu.Unlock()

	release := func() {
		g.Close()
		a.desk.mu.Lock()
		delete(a.desk.sessions, taskID)
		a.desk.mu.Unlock()
	}
	return g.ConfigPath, g.PermissionTool(), release, true
}

// approveSchema is the form the user is shown. One boolean, because the answer
// to "may it run this" is yes or no and anything else invites a half-answer
// that still has to be resolved to one of the two.
var approveSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "approve": {
      "type": "boolean",
      "title": "Allow this action?",
      "description": "Yes lets the agent run it. No blocks it and tells the agent why, so it can try another way."
    }
  },
  "required": ["approve"]
}`)

// ask puts one tool call to the user and turns their answer into a verdict the
// worker understands.
//
// Every path that is not an explicit yes resolves to no. A timeout, a dismissed
// prompt, a transport error and a decline all mean the same thing here: nobody
// said this could run.
func (d *permissionDesk) ask(ctx context.Context, session *mcp.ServerSession, req approval.Request) approval.Decision {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	res, err := session.Elicit(ctx, &mcp.ElicitParams{
		Mode:            "form",
		Message:         permissionQuestion(req),
		RequestedSchema: approveSchema,
	})
	if err != nil {
		return approval.Decision{Message: fmt.Sprintf(
			"could not reach the user to approve this (%v); it was not run. "+
				"Continue with what you can do without it, and say clearly what you skipped.", err)}
	}

	switch res.Action {
	case "accept":
		if approved, _ := res.Content["approve"].(bool); approved {
			return approval.Decision{Allow: true}
		}
		return approval.Decision{Message: "the user declined this action. Do not retry it; find another way or stop and explain what you cannot do without it."}
	case "decline":
		return approval.Decision{Message: "the user declined this action. Do not retry it; find another way or stop and explain what you cannot do without it."}
	default:
		return approval.Decision{Message: "the user dismissed the request without answering, so this was not run."}
	}
}

// permissionQuestion renders the request the way a person needs to see it: what
// is being asked for, on whose behalf, and — for a shell tool — the actual
// command, since "may the agent run Bash" is not a question anyone can answer.
func permissionQuestion(req approval.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "The delegated agent wants to use %s", req.ToolName)
	if detail := inputDetail(req.Input); detail != "" {
		fmt.Fprintf(&b, ":\n\n%s", detail)
	}
	fmt.Fprintf(&b, "\n\n(task %s)", req.TaskID)
	return b.String()
}

// inputDetail pulls the part of a tool's arguments worth showing. The fields
// checked first are the ones that carry the actual effect — a command, a path,
// a URL — because the rest is noise next to them.
func inputDetail(input map[string]any) string {
	for _, key := range []string{"command", "file_path", "path", "url", "pattern", "query"} {
		if v, ok := input[key]; ok {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				return truncate(s, 600)
			}
		}
	}
	if len(input) == 0 {
		return ""
	}
	buf, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return truncate(string(buf), 600)
}
