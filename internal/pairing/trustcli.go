// SPDX-License-Identifier: Apache-2.0

package pairing

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/config"
)

func trimmed(s string) string { return strings.TrimSpace(s) }
func envStateDir() string     { return config.Load().StateDir }

// RunTrust implements `cli-agent-mcp trust`.
//
// Most people should never run this. The server records its launcher on first
// start and answers to it from then on, so the ordinary install involves no
// command at all — that is the whole point of issue #27.
//
// It exists for the two moments when the recorded fact stops being true: a
// client that moved or updated to a new path, and a second client that should
// also be allowed in. Both are recoveries, and both are one command with no
// secret in them, which is what makes being locked out a nuisance here rather
// than the emergency it was with the token.
func RunTrust(args []string, resolveStateDir func(string) string) int {
	fs := flag.NewFlagSet("trust", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	stateDir := fs.String("state-dir", "", "State directory holding the record (default: CLI_AGENT_MCP_STATE_DIR, else the per-user one).")
	add := fs.Bool("add", false, "Trust the program that launched this command.")
	addExe := fs.String("add-exe", "", "Trust this executable by path, for when you cannot run the command from the client itself.")
	remove := fs.String("remove", "", "Stop trusting this executable.")
	reset := fs.Bool("reset", false, "Forget every trusted launcher; the next start records whoever launches it.")
	status := fs.Bool("status", false, "Show which programs may start this server.")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: cli-agent-mcp trust [options]

Decide which program may start this server. There is no secret here: the server
records whoever launches it the first time and answers to that program from then
on, so a normal install needs none of this.

Run it when that stops being true — your client updated to a new path, or you
want a second client allowed in.

Options:
`)
		fs.PrintDefaults()
		fmt.Fprint(os.Stderr, `
Examples:
  cli-agent-mcp trust --status     Show what may start this server.
  cli-agent-mcp trust --add        Trust whatever launched this command.
  cli-agent-mcp trust --reset      Forget everything and start over.
`)
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	dir := resolveDirLikeTheServer(*stateDir, resolveStateDir)

	switch {
	case *status:
		return trustStatus(dir)
	case *reset:
		return trustReset(dir)
	case *remove != "":
		return trustRemove(dir, *remove)
	case *addExe != "":
		return trustAdd(dir, *addExe)
	case *add:
		// Deliberately the parent of THIS process: run it from the client and
		// the client is what gets trusted. Run it from a terminal and the
		// terminal does — which is why --status prints what was recorded rather
		// than just saying "done".
		exe := ParentExe()
		if exe == "" {
			fmt.Fprintln(os.Stderr, "error: this platform cannot name the program that launched this command; use --add-exe with a path")
			return 1
		}
		return trustAdd(dir, exe)
	}
	fs.Usage()
	return 2
}

func trustStatus(dir string) int {
	f, exists, err := Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("record: %s\n", Path(dir))
	if !exists {
		fmt.Println("status: nothing recorded yet — the next program to start this server becomes the trusted one.")
		return 0
	}
	if len(f.Tokens) > 0 {
		fmt.Printf("status: this record uses TOKENS (%d), so launchers are not what authorizes here.\n", len(f.Tokens))
		fmt.Println("Run `cli-agent-mcp pair --status` instead.")
		return 0
	}
	if len(f.TrustedLaunchers) == 0 {
		fmt.Println("status: a record exists but trusts no launcher and holds no token — nothing can start this server.")
		fmt.Println("Run `cli-agent-mcp trust --reset` to start over.")
		return 0
	}
	fmt.Printf("status: answers to %d program(s)\n\n", len(f.TrustedLaunchers))
	for _, l := range f.TrustedLaunchers {
		how := "added by hand"
		if l.FirstUse {
			how = "recorded automatically on first launch"
		}
		fmt.Printf("  %s\n    %s, %s\n\n", l.Exe, how, l.Recorded.Local().Format(time.RFC3339))
	}
	return 0
}

func trustAdd(dir, exe string) int {
	f, _, err := Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if f == nil {
		f = &File{Version: fileVersion}
	}
	if len(f.Tokens) > 0 {
		fmt.Fprintln(os.Stderr, "error: this record authorizes with tokens, so adding a launcher would not change who gets in.")
		fmt.Fprintln(os.Stderr, "Run `cli-agent-mcp pair --unpair` first if you want to switch to launcher trust.")
		return 1
	}
	if !f.Trust(exe, false) {
		fmt.Printf("%s was already trusted; nothing to do.\n", exe)
		return 0
	}
	if err := Save(dir, f); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("trusted: %s\n", exe)
	fmt.Println("Restart that program; it can start this server from now on.")
	return 0
}

func trustRemove(dir, exe string) int {
	f, exists, err := Load(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	if !exists || !f.Untrust(exe) {
		fmt.Fprintf(os.Stderr, "error: %s is not on the list\n", exe)
		return 1
	}
	if err := Save(dir, f); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Printf("removed: %s\n", exe)
	if len(f.TrustedLaunchers) == 0 {
		// Saying this matters: a list emptied by hand refuses everyone, which is
		// not what someone removing one entry usually intends.
		fmt.Println("That was the last one. Nothing can start this server now — run `cli-agent-mcp trust --reset` to go back to trusting the next launcher.")
	}
	return 0
}

func trustReset(dir string) int {
	if err := Unpair(dir); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println("reset: the record is gone. The next program to start this server becomes the trusted one.")
	return 0
}

// resolveDirLikeTheServer applies the same precedence `pair` uses, for the
// reason spelled out there: a subcommand that resolved differently from the
// server would write where nobody reads (issue #22).
func resolveDirLikeTheServer(flagValue string, resolve func(string) string) string {
	dir := flagValue
	if trimmed(dir) == "" {
		if env := trimmed(envStateDir()); env != "" {
			dir = env
			fmt.Printf("using %s (from %s)\n\n", dir, "CLI_AGENT_MCP_STATE_DIR")
		}
	}
	return resolve(dir)
}
