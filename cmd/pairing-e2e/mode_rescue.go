// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/winspawn"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func init() {
	register("rescue", mode{
		Point: 3,
		What: "Recovering access with no terminal. A locked-out user has no working client and no way to work\n" +
			"out which record the server reads, so the rescue has to be a tool the server itself performs —\n" +
			"and it has to refuse to unpair a server that is working.",
		Run: runRescue,
	})
}

// runRescue is the only mode that speaks MCP, because it is the only one whose
// subject is a tool call rather than a start-up verdict.
//
// It is also the only one where the harness is itself the launcher: the server
// is started as a direct child, so ParentExe() reports this process — which the
// planted record does not trust, which is exactly the locked server the rescue
// is for.
func runRescue(r *rig) {
	state := r.freshState("rescue")
	if err := plantForeignRecord(state); err != nil {
		r.check("foreign record could be written", "^$", err.Error())
		return
	}

	// The first start is SERVED, with a warning. That is by design (announce.go)
	// and it is why the rescue needs two starts to have anything to rescue:
	// asserting on the first would be asserting about a server that is not
	// locked, and the tool correctly does nothing to one of those.
	r.check("1st start warns and serves, without blocking yet",
		"SERVING THIS ONCE", r.run(r.env(state), r.server))

	// Second start: now refusing. This is the server under test.
	res, err := callRescue(r, state)
	r.check("the locked server answers the MCP call",
		"^$", errText(err))
	if err == nil {
		r.check("the rescue ran, rather than being refused by the lock itself",
			"^false$", boolText(res.IsError))
		// A rescue that removes the record and does not say the client has to
		// restart leaves the user staring at the same locked server: this
		// process keeps the verdict it was born with.
		r.check("it says the client must be restarted",
			"restart", toolText(res))
	}
	r.check("the record the server was reading is gone",
		"^false$", boolText(exists(recordPath(state))))

	// THE CONTROL, and it is the one that matters most in this file. The rescue
	// is reachable by whoever is being shut out, so the single thing standing
	// between it and a general unpair button is that it declines when the
	// server is not locked. Without this check, a tool that unpaired
	// unconditionally would pass every assertion above.
	healthy := r.freshState("rescue-control")
	res, err = callRescue(r, healthy)
	r.check("CONTROL: the healthy server answers too",
		"^$", errText(err))
	if err == nil {
		r.check("CONTROL: it refuses to unpair a server that is working",
			"^true$", boolText(res.IsError))
	}
	// The record here is the one the healthy server wrote for itself on first
	// start, trusting its launcher. Still being there is the proof that the
	// refusal above was a refusal and not just a differently worded success.
	r.check("CONTROL: the record is still in place",
		"^true$", boolText(exists(recordPath(healthy))))
}

// plantForeignRecord writes a record that trusts some other program entirely,
// already outside the learning window.
//
// Backdated for the usual reason — a record inside its first day adopts the
// caller instead of refusing it, and there would be nothing locked to rescue.
func plantForeignRecord(stateDir string) error {
	b, err := json.Marshal(map[string]any{
		"version": 1,
		"trusted_launchers": []any{map[string]any{
			"exe":       `C:\Otro\Programa\cliente.exe`,
			"recorded":  "2020-01-01T00:00:00Z",
			"first_use": true,
		}},
	})
	if err != nil {
		return err
	}
	return os.WriteFile(recordPath(stateDir), b, 0o600)
}

// callRescue connects to a freshly started server over stdio and calls the
// rescue tool, the way the client of a locked-out user would.
//
// It goes through the real MCP client rather than hand-written JSON-RPC so the
// path exercised is the one a client actually takes — including the initialize
// handshake, which a refusing server has to keep answering or the tool could
// never be reached.
func callRescue(r *rig, stateDir string) (*mcp.CallToolResult, error) {
	return callTool(r, stateDir, "agent_pairing_rescue")
}

// callTool starts a server and calls one tool on it.
//
// It exists because reading stderr is not enough to know whether a server
// serves. Measured: with Result.Allowed() mutated to exclude UnreadableRecord —
// a total regression of #29 point 4 — every line this harness used to check
// stayed identical, because the verdict text and the lockdown decision are made
// in different places. The log kept saying SERVING while every tool was refused.
//
// So the modes that claim a server serves, or does not, ask it.
func callTool(r *rig, stateDir, name string) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := winspawn.Harden(exec.CommandContext(ctx, r.server))
	cmd.Env = r.env(stateDir)
	cmd.Dir = r.dir
	// The server's own log goes nowhere: its verdict is asserted on separately,
	// and interleaving it with the report would bury the checks.
	cmd.Stderr = nil

	client := mcp.NewClient(&mcp.Implementation{Name: "pairing-e2e", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, err
	}
	defer session.Close()

	return session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: map[string]any{},
	})
}

// servedOrNot reduces a tool call to one word so the behavioural claim goes
// through check() with a pattern, like every other assertion here — and so a
// failure prints WHY it was refused instead of just "false".
func servedOrNot(res *mcp.CallToolResult, err error) string {
	switch {
	case err != nil:
		return "the client could not even connect: " + err.Error()
	case res == nil:
		return "the server answered with nothing at all"
	case res.IsError:
		return "refused: " + toolText(res)
	}
	return "served"
}

// The three helpers below exist so every assertion in this file goes through
// check with its pattern, like every other mode. A mode that fell back to bare
// if-statements would print its outcomes in a different shape and stop being
// countable in the same tally.

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
			b.WriteString(" ")
		}
	}
	if b.Len() == 0 {
		return "(the tool returned no text at all)"
	}
	return b.String()
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
