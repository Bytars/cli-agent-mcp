// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/config"
)

// DefaultLabel is the client most installs pair first.
const DefaultLabel = "claude-desktop"

// Run implements `cli-agent-mcp pair`. It returns a process exit code.
func Run(args []string, resolveStateDir func(string) string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", "", "State directory holding the pairing record (default: CLI_AGENT_MCP_STATE_DIR, else the per-user one).")
	label := fs.String("label", DefaultLabel, "Name for the token, so it can be revoked on its own.")
	install := fs.Bool("install", false, "Write the token straight into the client's config file.")
	configPath := fs.String("config", "", "Config file for --install (default: the client's usual location).")
	noBind := fs.Bool("no-bind", false, "Do not bind this token to the program that launches the server.")
	status := fs.Bool("status", false, "Show the pairing record and exit.")
	revoke := fs.String("revoke", "", "Revoke the token with this label.")
	unbind := fs.String("unbind", "", "Forget the launcher bound to this label, so the next start records a new one.")
	unpair := fs.Bool("unpair", false, "Remove the whole pairing record and go back to running unpaired.")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: cli-agent-mcp pair [options]

Issue the secret that authorizes an MCP client to drive this server. Run it once
per client; from then on a launcher that cannot present the secret gets a server
that refuses to do anything.

This authorizes who may start and use the server. It does not encrypt the MCP
conversation — that runs over a private pipe between the client and this process,
with nothing on it for anyone else to read.

Options:
`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
Examples:
  cli-agent-mcp pair --install              Pair Claude Desktop and edit its config.
  cli-agent-mcp pair --label cowork         Issue a second token, printed for pasting.
  cli-agent-mcp pair --status               Show what is currently paired.
  cli-agent-mcp pair --revoke cowork        Take one client's access away.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Resolve exactly the way the server does, and for the reason internal/inspect
	// already had to learn: the server takes its state directory from
	// CLI_AGENT_MCP_STATE_DIR (config.go), so a `pair` that consulted only its
	// own flag wrote the record into the per-user default while the server read
	// somewhere else entirely.
	//
	// Nothing failed when that happened. `pair` printed `paired "cowork"` and a
	// token snippet, and the server went on serving every launcher — the operator
	// believed the hole was closed and it was wide open. A silent no-op that
	// fails toward insecure is worse than an error, which is why the resolved
	// path is now printed by every operation below rather than assumed.
	dir := *stateDir
	fromEnv := false
	if strings.TrimSpace(dir) == "" {
		if env := strings.TrimSpace(config.Load().StateDir); env != "" {
			dir, fromEnv = env, true
		}
	}
	dir = resolveStateDir(dir)
	if fromEnv {
		fmt.Printf("using %s (from CLI_AGENT_MCP_STATE_DIR)\n\n", dir)
	}

	switch {
	case *status:
		return runStatus(dir)
	case *unpair:
		return runUnpair(dir)
	case *revoke != "":
		return runRevoke(dir, *revoke)
	case *unbind != "":
		return runUnbind(dir, *unbind)
	}
	return runMint(dir, *label, *noBind, *install, *configPath)
}

func runStatus(dir string) int {
	f, paired, err := Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("record: %s\n", Path(dir))
	if !paired {
		fmt.Print(`status: NOT PAIRED

Any process on this machine can start this server and delegate work to a coding
agent that inherits your environment. Run `)
		fmt.Print("`cli-agent-mcp pair --install`")
		fmt.Print(" to close that.\n")
		return 0
	}
	if len(f.Tokens) == 0 {
		fmt.Println("status: paired, but no tokens — nothing can authenticate. Run `cli-agent-mcp pair` to issue one.")
		return 0
	}

	fmt.Printf("status: paired (%d token(s))\n\n", len(f.Tokens))
	toks := append([]Token(nil), f.Tokens...)
	sort.Slice(toks, func(i, j int) bool { return toks[i].Created.Before(toks[j].Created) })
	for _, t := range toks {
		fmt.Printf("  %s\n", t.Label)
		fmt.Printf("    created   %s\n", t.Created.Local().Format(time.RFC3339))
		if t.LastUsed.IsZero() {
			fmt.Printf("    last used never — the client has not started the server with this token yet\n")
		} else {
			fmt.Printf("    last used %s\n", t.LastUsed.Local().Format(time.RFC3339))
		}
		switch {
		case t.NoBind:
			fmt.Printf("    launcher  unbound (--no-bind): any program holding this token may start the server\n")
		case t.Parent == nil:
			fmt.Printf("    launcher  not recorded yet; the next successful start is what this token gets bound to\n")
		default:
			fmt.Printf("    launcher  %s\n", t.Parent.Exe)
		}
		fmt.Println()
	}
	return 0
}

func runUnpair(dir string) int {
	if err := Unpair(dir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("unpaired: every token is gone and the server will serve any launcher again.")
	fmt.Println("Tokens still sitting in a client config are now dead weight; clear them out.")
	return 0
}

func runRevoke(dir, label string) int {
	ok, err := Revoke(dir, label)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "error: no token labelled %q\n", label)
		return 1
	}
	fmt.Printf("revoked %q. A client still holding that token is now locked out; restart it after re-pairing.\n", label)
	return 0
}

func runUnbind(dir, label string) int {
	ok, err := Unbind(dir, label)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if !ok {
		fmt.Fprintf(os.Stderr, "error: no token labelled %q\n", label)
		return 1
	}
	fmt.Printf("unbound %q. The next program that starts the server with this token becomes the one it accepts.\n", label)
	return 0
}

func runMint(dir, label string, noBind, install bool, configPath string) int {
	_, wasPaired, err := Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	secret, err := Mint(dir, label, noBind)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	exe, err := os.Executable()
	if err != nil {
		exe = "cli-agent-mcp"
	}

	if install {
		path, created, err := InstallToken(configPath, exe, secret)
		if err != nil {
			// The token is already minted and valid; the user just has to place
			// it themselves. Falling back to printing beats leaving them with a
			// half-done pairing and no secret to show for it.
			fmt.Fprintf(os.Stderr, "warning: could not update the client config (%v)\n", err)
			fmt.Fprintln(os.Stderr, "The token below is valid — add it by hand.")
			printSnippet(exe, secret, label)
			return 1
		}
		if created {
			// A config that did not exist is a config the client was not using
			// (issue #25). Saying "updated <path>" here is how a machine ended
			// up with enforcement on and no way to present the token: the write
			// succeeded, the message read as success, and the client — a
			// connector defined outside the filesystem — never saw the file.
			//
			// The token is still valid, so the secret is printed too: on this
			// path the user almost certainly has to place it somewhere else.
			fmt.Printf("paired %q, and CREATED %s\n\n", label, path)
			fmt.Print(`That file did not exist until now, which usually means your client was not
reading it. If this server is launched by something that keeps its own
configuration — an extension, or a connector defined in your account rather than
on disk — the token below has to go THERE instead, or into the environment the
launcher passes down.

Before you rely on this, check it: restart the client and look at the server's
first log line. It must say

    paired: authorized as ` + strconv.Quote(label) + `

If instead it says "refusing to serve", the token did not reach it. Run
` + "`cli-agent-mcp pair --unpair`" + ` to get your client working again, then place the
token where that launcher will actually read it.

`)
			printSnippet(exe, secret, label)
		} else {
			fmt.Printf("paired %q and updated %s\n", label, path)
			fmt.Println("Restart the client so it picks up the new configuration.")
		}
	} else {
		fmt.Printf("paired %q\n", label)
		printSnippet(exe, secret, label)
	}

	// Always name the record. Pairing only takes effect for a server reading
	// this same directory, and when the two disagree nothing signals it: the
	// server just keeps serving. Printing the path is what lets someone compare
	// it against the "task state:" line the server logs at startup.
	fmt.Printf("\nrecord: %s\n", Path(dir))

	if !wasPaired {
		fmt.Print(`
Enforcement is now on: a launcher that cannot present a token gets a server that
refuses every tool call. Keep this terminal's output out of anything shared — the
secret is shown once and only its hash is stored.

Enforcement applies to a server reading THAT record. If you start one with a
different CLI_AGENT_MCP_STATE_DIR, it reads a different record and stays open.
`)
	}
	if !noBind {
		fmt.Print(`
This token also binds to whichever program first starts the server with it. If you
later move or reinstall that client, run ` + "`cli-agent-mcp pair --unbind " + label + "`" + `.
`)
	}
	return 0
}

// printSnippet shows the config entry to paste, with the secret in place.
func printSnippet(exe, secret, label string) {
	entry := map[string]any{
		"mcpServers": map[string]any{
			ServerKey: map[string]any{
				"command": exe,
				"env":     map[string]any{EnvVar: secret},
			},
		},
	}
	buf, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		// Nothing here can realistically fail to marshal, but a user left with
		// no secret at all would have no way to finish pairing.
		fmt.Printf("\n%s=%s\n", EnvVar, secret)
		return
	}
	fmt.Printf("\nAdd this to the client's config (merging with what is already there):\n\n%s\n", buf)
	fmt.Printf("\nShown once. If you lose it, run `cli-agent-mcp pair --label %s` again to issue a new one.\n", label)
}

// Labels names the issued tokens, for the server's startup line.
func Labels(f *File) string {
	if f == nil || len(f.Tokens) == 0 {
		return "none"
	}
	names := make([]string, 0, len(f.Tokens))
	for _, t := range f.Tokens {
		names = append(names, t.Label)
	}
	return strings.Join(names, ", ")
}
