// SPDX-License-Identifier: Apache-2.0

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

	"github.com/Bytars/cli-agent-mcp/internal/approval"
	"github.com/Bytars/cli-agent-mcp/internal/grants"
	"github.com/Bytars/cli-agent-mcp/internal/task"
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
	mgr     *task.Manager
	grants  *grants.Store

	mu       sync.Mutex
	sessions map[string]*mcp.ServerSession // task id -> who to ask
}

func newPermissionDesk(timeout time.Duration) *permissionDesk {
	return &permissionDesk{timeout: timeout, sessions: map[string]*mcp.ServerSession{}}
}

// Decide is the broker's Decider. It has three answers in descending order of
// how much of the user's attention they cost.
//
// A permission already granted is applied without asking again — asking twice
// is the fastest way to train someone to stop reading the question. A client
// that can be elicited is asked directly. Anything else is parked against the
// task and waits: the orchestrating model is talking to the same person and
// polls this server constantly, so the question reaches them by that route
// instead, and the worker holds rather than losing the work it was doing.
func (d *permissionDesk) Decide(ctx context.Context, req approval.Request) approval.Decision {
	command := commandOf(req.Input)

	if d.grants.Allows(req.ToolName, command) {
		return approval.Decision{Allow: true}
	}

	d.mu.Lock()
	session := d.sessions[req.TaskID]
	d.mu.Unlock()

	if session != nil && cannotAsk(session) == "" {
		return d.ask(ctx, session, req)
	}
	return d.park(ctx, req, command)
}

// park holds the request against its task until a person answers it through
// agent_answer_permission.
func (d *permissionDesk) park(ctx context.Context, req approval.Request, command string) approval.Decision {
	if d.mgr == nil {
		return approval.Decision{Message: "nobody can be asked about this, so it was not run"}
	}

	ans := d.mgr.AskPermission(ctx, req.TaskID, req.ToolName, inputDetail(req.Input), command, d.timeout)
	if !ans.Allow {
		msg := ans.Message
		if msg == "" {
			msg = "the user declined this action. Do not retry it; find another way or stop and explain what you cannot do without it."
		}
		return approval.Decision{Message: msg}
	}

	// Remembering is deliberately NOT done here. It used to be, and the failure
	// mode was that agent_answer_permission told the user the permission was
	// recorded for every future task while the store quietly refused to save it:
	// the tool answering the question could not see the outcome of an action
	// taken on this goroutine, after it had already replied. Whoever reports the
	// result has to be the one who performed it, so the grant is added in the
	// tool handler instead.
	return approval.Decision{Allow: true}
}

// commandOf pulls the program out of a shell tool's arguments, which is what a
// grant is keyed on. Tools that are not shells have no command, and a grant for
// them covers the whole tool.
func commandOf(input map[string]any) string {
	if v, ok := input["command"]; ok {
		return grants.CommandVerb(fmt.Sprint(v))
	}
	return ""
}

// Approver returns the approver for one client session, or nil when the
// approval endpoint is not running at all.
//
// Every client gets one, including those that cannot elicit: Decide falls back
// to parking the request on its task, where the orchestrator relays it and
// agent_answer_permission releases the worker. Returning nil means something
// else entirely — the worker is never given a way to ask, and a tool that is
// neither pre-approved nor denied stalls until the task timeout.
func (d *permissionDesk) Approver(session *mcp.ServerSession) task.Approver {
	if d == nil || d.broker == nil || session == nil {
		return nil
	}
	return &sessionApprover{desk: d, session: session}
}

// cannotAsk returns why this client cannot be asked a permission question, or
// "" when it can. It is phrased as the obstacle rather than the capability
// because both callers want to explain the absence.
//
// Two things have to hold. The client must have declared during initialize that
// it can elicit — asking one that cannot would block every request until it
// timed out. And the negotiated protocol must still permit a server to open an
// elicitation on its own: from 2026-07-28 the spec (SEP-2322) forbids
// server-initiated requests and requires the question to be embedded in the
// result of a call the client itself made. A permission request does not arrive
// during such a call — it comes from a worker process, often long after the
// delegating call returned — so there is nothing to embed it in.
func cannotAsk(session *mcp.ServerSession) string {
	params := session.InitializeParams()
	if params == nil {
		return "the client's capabilities are not known"
	}
	if params.Capabilities == nil || params.Capabilities.Elicitation == nil {
		return "this client did not declare the elicitation capability, so it has no way to put the question to you"
	}
	if params.ProtocolVersion >= mrtrProtocolVersion {
		return "MCP " + params.ProtocolVersion + " no longer lets a server open an elicitation on its own (SEP-2322), " +
			"and a worker's permission request does not arrive during a call this client made, so there is nothing to attach it to"
	}
	return ""
}

// mrtrProtocolVersion is the release that replaced server-initiated elicitation
// with multi round-trip requests.
const mrtrProtocolVersion = "2026-07-28"

// status reports whether a worker can ask this client for permission, and why
// not when it cannot. agent_diagnose surfaces it, because the symptom of this
// being off is a task that quietly does less than it was asked to, with nothing
// in the transcript pointing at the cause.
func (d *permissionDesk) status(session *mcp.ServerSession, configured bool) (bool, string) {
	if !configured {
		return false, "turned off on this server (CLI_AGENT_MCP_ASK_PERMISSION=false)"
	}
	if d == nil || d.broker == nil {
		return false, "the approval endpoint could not be started; see the server log"
	}
	if session == nil {
		return false, "no client session"
	}
	if reason := cannotAsk(session); reason != "" {
		// Not being elicitable is no longer the end of it: the request is parked
		// and relayed instead, which is slower but reaches the same person.
		return true, "by parking the request — " + reason
	}
	return true, ""
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
