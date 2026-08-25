// SPDX-License-Identifier: Apache-2.0

package task

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"
)

// A worker that needs a permission it was not given has three possible fates,
// and only one of them is any good.
//
// It can stall on a prompt nobody can answer, which is what a headless agent
// does by default and why task timeouts exist. It can be refused outright,
// which is honest but means the task quietly does less than it was asked to.
// Or the question can travel to the person who delegated the work.
//
// The direct route for that is MCP elicitation, and where the client supports
// it, that is what happens. Where it does not — and today's Claude Desktop does
// not declare the capability — the question still has somewhere to go: the
// orchestrating model is holding a conversation with that same person, and it
// polls this server constantly. So the request is parked here, announced in the
// transcript where any watcher will see it, and the worker waits. The
// orchestrator relays it, the human answers, and agent_answer_permission
// releases the worker.
//
// The worker blocking is the point rather than a cost: it is what turns "the
// agent couldn't do that" into "the agent is waiting for you".

// PermissionRequest is one parked question.
type PermissionRequest struct {
	ID      string    `json:"id"`
	TaskID  string    `json:"task_id"`
	Tool    string    `json:"tool"`
	Detail  string    `json:"detail,omitempty"`  // the command, path or URL at stake
	Command string    `json:"command,omitempty"` // the program being run, for remembering
	AskedAt time.Time `json:"asked_at"`

	answer chan PermissionAnswer
}

// PermissionAnswer is what a person decided.
type PermissionAnswer struct {
	Allow    bool
	Remember bool
	Message  string // shown to the agent when refused
}

// Age reports how long this request has been waiting.
func (r *PermissionRequest) Age() time.Duration { return time.Since(r.AskedAt) }

// permissionDesk is the manager's registry of parked requests.
type permissionDesk struct {
	mu      sync.Mutex
	pending map[string]*PermissionRequest // request id -> request
}

func newDesk() *permissionDesk {
	return &permissionDesk{pending: map[string]*PermissionRequest{}}
}

func newRequestID() string {
	var b [6]byte
	// crypto/rand.Read never returns an error: it crashes the process rather
	// than hand back short or predictable bytes. There is no failure here to
	// handle, only one to pretend to handle.
	_, _ = rand.Read(b[:])
	return "perm-" + hex.EncodeToString(b[:])
}

// AskPermission parks a request against a task and waits for an answer.
//
// It returns the answer, or a refusal when the wait runs out or the task goes
// away. Every path that is not an explicit yes is a no: nobody said this could
// run.
func (m *Manager) AskPermission(ctx context.Context, taskID, tool, detail, command string, wait time.Duration) PermissionAnswer {
	t, ok := m.Get(taskID)
	if !ok {
		return PermissionAnswer{Message: "the task that asked for this no longer exists"}
	}

	req := &PermissionRequest{
		ID:      newRequestID(),
		TaskID:  taskID,
		Tool:    tool,
		Detail:  detail,
		Command: command,
		AskedAt: time.Now(),
		answer:  make(chan PermissionAnswer, 1),
	}

	m.desk.mu.Lock()
	m.desk.pending[req.ID] = req
	m.desk.mu.Unlock()

	t.mu.Lock()
	t.pending = req
	// Written into the transcript so it reaches every watcher by the route they
	// already use: a new line is what wakes agent_watch and what the board
	// polls for. Without it a parked request would be invisible until someone
	// thought to ask for the task's status.
	t.appendLine(
		fmt.Sprintf("[permission] %s wants to use %s: %s", req.ID, tool, detail),
		fmt.Sprintf("⏸ WAITING FOR PERMISSION — %s wants to use %s: %s\n   Answer with agent_answer_permission (task_id=%s, allow=true|false)",
			shortID(taskID), tool, truncate(detail, 200), taskID),
		false)
	t.mu.Unlock()
	t.persist()

	defer func() {
		m.desk.mu.Lock()
		delete(m.desk.pending, req.ID)
		m.desk.mu.Unlock()

		t.mu.Lock()
		if t.pending == req {
			t.pending = nil
		}
		t.mu.Unlock()
		t.persist()
	}()

	var timeout <-chan time.Time
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		timeout = timer.C
	}

	select {
	case ans := <-req.answer:
		verdict := "DENIED"
		if ans.Allow {
			verdict = "ALLOWED"
		}
		t.mu.Lock()
		t.appendLine(
			fmt.Sprintf("[permission] %s %s", req.ID, verdict),
			fmt.Sprintf("▶ permission %s — continuing", verdict),
			false)
		t.mu.Unlock()
		return ans

	case <-timeout:
		t.mu.Lock()
		t.appendLine(
			fmt.Sprintf("[permission] %s timed out after %s", req.ID, wait),
			fmt.Sprintf("✗ permission request timed out after %s — not run", wait),
			false)
		t.mu.Unlock()
		return PermissionAnswer{Message: fmt.Sprintf(
			"nobody answered within %s, so this was not run. Continue with what you can do "+
				"without it and say clearly what you skipped.", wait)}

	case <-ctx.Done():
		return PermissionAnswer{Message: "the request was abandoned before anyone answered; it was not run"}
	}
}

// AnswerPermission releases a parked request. When id is empty it answers the
// task's current one, which is what a caller relaying a conversation has.
func (m *Manager) AnswerPermission(taskID, id string, ans PermissionAnswer) (*PermissionRequest, error) {
	m.desk.mu.Lock()
	var req *PermissionRequest
	switch {
	case id != "":
		req = m.desk.pending[id]
	default:
		for _, p := range m.desk.pending {
			if p.TaskID == taskID {
				req = p
				break
			}
		}
	}
	if req != nil {
		delete(m.desk.pending, req.ID)
	}
	m.desk.mu.Unlock()

	if req == nil {
		return nil, fmt.Errorf("there is no permission request waiting for task %q "+
			"(it may have been answered already, or timed out)", taskID)
	}
	select {
	case req.answer <- ans:
	default:
		// Buffered by one and removed from the map under the lock, so this can
		// only mean the waiter is already gone.
		return nil, fmt.Errorf("permission request %s is no longer waiting", req.ID)
	}
	return req, nil
}

// PendingPermissions lists every parked request, oldest first.
func (m *Manager) PendingPermissions() []PermissionRequest {
	m.desk.mu.Lock()
	defer m.desk.mu.Unlock()

	out := make([]PermissionRequest, 0, len(m.desk.pending))
	for _, p := range m.desk.pending {
		out = append(out, *p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].AskedAt.Before(out[j-1].AskedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// shortID trims a task id to its counter part for a transcript line, where the
// full id is noise.
func shortID(id string) string {
	if len(id) > 14 {
		return id[:14] + "…"
	}
	return id
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
