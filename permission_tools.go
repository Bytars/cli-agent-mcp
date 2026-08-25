// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Bytars/cli-agent-mcp/internal/grants"
	"github.com/Bytars/cli-agent-mcp/internal/task"
)

type answerInput struct {
	TaskID    string `json:"task_id" jsonschema:"The task whose worker is waiting."`
	Allow     bool   `json:"allow" jsonschema:"True to let the worker do it, false to refuse. Ask the user first — this is their decision, not yours."`
	Remember  bool   `json:"remember,omitempty" jsonschema:"Record the permission so it is never asked for again, on this or any future task. Set it only when the user says to allow it from now on, not merely that they allow it once."`
	Message   string `json:"message,omitempty" jsonschema:"Optional reason shown to the agent when refusing, so it can work around the refusal instead of guessing."`
	RequestID string `json:"request_id,omitempty" jsonschema:"Which request to answer, when a task somehow has more than one waiting. Normally leave unset."`
}

type revokeInput struct {
	Tool    string `json:"tool" jsonschema:"The tool whose permission to withdraw, e.g. \"PowerShell\"."`
	Command string `json:"command,omitempty" jsonschema:"The command it was granted for, e.g. \"docker\". Omit to revoke every grant for that tool."`
}

type permissionsResult struct {
	Granted []grants.Grant           `json:"granted"`
	Waiting []task.PermissionRequest `json:"waiting,omitempty"`
}

// permissionsText renders permissions for a person: what a worker is blocked on
// comes first, because it is the only part that needs an answer right now.
func permissionsText(res permissionsResult) string {
	var b strings.Builder

	if len(res.Waiting) > 0 {
		fmt.Fprintf(&b, "WAITING FOR AN ANSWER (%d):\n", len(res.Waiting))
		for _, w := range res.Waiting {
			fmt.Fprintf(&b, "  task %s — %s: %s  (asked %s ago)\n",
				w.TaskID, w.Tool, truncate(w.Detail, 160), w.Age().Round(time.Second))
		}
		b.WriteString("\nAnswer with agent_answer_permission.\n\n")
	}

	if len(res.Granted) == 0 {
		b.WriteString("No permissions have been granted permanently. " +
			"A worker will ask before using anything outside the server's pre-approved list.")
		return b.String()
	}
	fmt.Fprintf(&b, "GRANTED PERMANENTLY (%d):\n", len(res.Granted))
	for _, g := range res.Granted {
		fmt.Fprintf(&b, "  %-40s granted %s\n", g.String(), g.GrantedAt.Format("2006-01-02 15:04"))
	}
	return b.String()
}

// registerPermissionTools adds the three tools that let the person who
// delegated a task answer for it: release a worker that is blocked, see what
// has been allowed, and take an allowance back.
func registerPermissionTools(srv *mcp.Server, mgr *task.Manager, grantStore *grants.Store) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_answer_permission",
		Description: "Answer a worker that is WAITING for permission to use a tool it was not pre-approved for. " +
			"A task in that state is running but making no progress, and only a person can release it — so put the request to the user, in their words, and call this with their answer. " +
			"Set remember=true when they say to allow it from now on: the permission is then recorded and that tool never has to be asked for again, on this task or any future one. " +
			"Denying is safe — the worker is told why and carries on with what it can do without it.",
		Annotations: mutatingTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, in answerInput) (*mcp.CallToolResult, task.Snapshot, error) {
		r, err := mgr.AnswerPermission(in.TaskID, in.RequestID, task.PermissionAnswer{
			Allow:    in.Allow,
			Remember: in.Remember,
			Message:  in.Message,
		})
		if err != nil {
			return errResult(err.Error()), task.Snapshot{}, nil
		}
		t, _ := mgr.Get(r.TaskID)
		snap := t.Snapshot()

		verb := "Denied"
		if in.Allow {
			verb = "Allowed"
		}
		msg := fmt.Sprintf("%s %s for task %s. The worker has resumed.", verb, r.Tool, r.TaskID)
		if in.Allow && in.Remember {
			msg += fmt.Sprintf(" %s is now pre-approved for every future task, so it will not be asked again — undo that with agent_revoke_permission.",
				grants.Grant{Tool: r.Tool, Command: r.Command})
		}
		return textResult(msg), snap, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_list_permissions",
		Description: "List the permissions the user has granted permanently, and any request a worker is waiting on right now. " +
			"Use it when they ask what the agent is allowed to do, or when a task seems stuck.",
		Annotations: readOnlyTool(),
	}, func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, permissionsResult, error) {
		res := permissionsResult{Granted: grantStore.List(), Waiting: mgr.PendingPermissions()}
		return textResult(permissionsText(res)), res, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name: "agent_revoke_permission",
		Description: "Withdraw a permanently granted permission, so workers have to ask for it again. " +
			"Give the tool name, and the command to narrow it (e.g. tool=\"PowerShell\", command=\"docker\"); omitting the command revokes every grant for that tool.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptr(false), IdempotentHint: true, OpenWorldHint: ptr(false)},
	}, func(ctx context.Context, req *mcp.CallToolRequest, in revokeInput) (*mcp.CallToolResult, permissionsResult, error) {
		removed, err := grantStore.Remove(in.Tool, in.Command)
		if err != nil {
			return errResult(err.Error()), permissionsResult{}, nil
		}
		res := permissionsResult{Granted: grantStore.List(), Waiting: mgr.PendingPermissions()}
		if !removed {
			return textResult("Nothing to revoke: no such permission was granted.\n\n" + permissionsText(res)), res, nil
		}
		return textResult("Revoked. Workers will ask about it again.\n\n" + permissionsText(res)), res, nil
	})
}
