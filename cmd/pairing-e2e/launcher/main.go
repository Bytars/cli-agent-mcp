// SPDX-License-Identifier: Apache-2.0

// Command launcher stands in for a client program that starts the server.
//
// Pairing decides on WHO LAUNCHED the server, so none of it can be tested by
// running the binary from a shell: the parent would be the shell every time,
// and all six failure modes of issue #29 would collapse into the same case.
// This program runs the server as a CHILD, so pairing.ParentExe() reports THIS
// path — and copying the same binary to several paths is the only way the
// harness can produce "a different client" at all.
//
// It has to be a real process rather than an injected fake. The thing under
// test is an OS parent-process lookup resolving a path on disk; stubbing that
// out would prove only that the harness agrees with itself.
//
// Usage: launcher <server-exe> [args...]
package main

import (
	"os"
	"os/exec"

	"github.com/Bytars/cli-agent-mcp/internal/winspawn"
)

func main() {
	if len(os.Args) < 2 {
		os.Stderr.WriteString("usage: launcher <server-exe> [args...]\n")
		os.Exit(2)
	}
	// winspawn.Harden: the harness runs on a developer's desktop and starts one
	// server per check, so issue #18's console flash would arrive twenty times
	// in a run. It does not disturb the pipes below.
	cmd := winspawn.Harden(exec.Command(os.Args[1], os.Args[2:]...))
	// Stdin closed on purpose. There is no MCP client on the other end here, so
	// the server prints its pairing verdict and exits instead of blocking on a
	// wire nobody is writing to.
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}
