package inspect

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/state"
	"github.com/Bytars/cli-agent-mcp/internal/task"
)

// pollInterval is how often a follower checks for new output. Fast enough that
// a tail feels live, slow enough that watching a dozen tasks costs nothing.
const pollInterval = 400 * time.Millisecond

// settleDrains is how many extra polls run after a task's record says it
// finished. The record is written before the last transcript lines have
// necessarily been flushed, so stopping the instant the status changes would
// cut off the end of the run — which is the part being waited for.
const settleDrains = 3

// Run dispatches the read-only subcommands and returns a process exit code.
func Run(args []string) int {
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "tasks":
		return runTasks(args[1:])
	case "logs":
		return runLogs(args[1:])
	case "ui":
		return runUI(args[1:])
	default:
		return usage()
	}
}

func usage() int {
	fmt.Fprint(os.Stderr, `Usage:
  cli-agent-mcp tasks            List the tasks and their status.
  cli-agent-mcp logs [TASK]      Show and follow one task's log.
  cli-agent-mcp ui               Open the local web viewer.

Add --help to any of them to see its options.
`)
	return 2
}

// ---- shared plumbing ----------------------------------------------------

// parseWithPositionals lets flags and positional arguments interleave, so both
// `logs -f task-1` and `logs task-1 -f` work. Go's flag package stops at the
// first non-flag argument; this feeds it what is left until nothing remains.
func parseWithPositionals(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// palette paints status and structure. Colour is off unless stdout is a real
// terminal, so redirecting to a file or piping to a tool yields clean text.
type palette struct{ on bool }

func newPalette(disabled bool) palette {
	if disabled || os.Getenv("NO_COLOR") != "" || !isTerminal(os.Stdout) {
		return palette{}
	}
	return palette{on: enableANSI()}
}

const (
	cBold  = "1"
	cDim   = "2"
	cRed   = "31"
	cGreen = "32"
	cCyan  = "36"
	cGray  = "90"
)

func (p palette) paint(code, s string) string {
	if !p.on || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

// statusCell pads before painting: escape codes are zero-width on screen but
// would be counted by a %-*s applied afterwards, so the columns would drift.
func (p palette) statusCell(s task.Status, width int) string {
	padded := fmt.Sprintf("%-*s", width, string(s))
	switch s {
	case task.StatusRunning:
		return p.paint(cCyan, padded)
	case task.StatusDone:
		return p.paint(cGreen, padded)
	case task.StatusFailed:
		return p.paint(cRed, padded)
	default:
		return p.paint(cGray, padded)
	}
}

// header prints where we are reading from and whether a server is alive to
// produce new output.
func header(src *Source, p palette) {
	fmt.Println(p.paint(cDim, "state: "+src.Dir()))
	if o := src.Owner(); o != nil {
		fmt.Println(p.paint(cDim, fmt.Sprintf("server running: pid %d since %s", o.PID, o.Started.Format("15:04:05"))))
	} else {
		fmt.Println(p.paint(cDim, "no server running (history only)"))
	}
}

func matches(t task.Snapshot, agentName, status string) bool {
	if agentName != "" && !strings.EqualFold(t.Agent, agentName) {
		return false
	}
	if status != "" && !strings.EqualFold(string(t.Status), status) {
		return false
	}
	return true
}

func shortID(id string) string {
	if i := strings.LastIndex(id, "-"); i >= 0 && i+1 < len(id) {
		return id[i+1:]
	}
	return id
}

func oneLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	r := []rune(s)
	if len(r) <= max || max < 2 {
		return s
	}
	return string(r[:max-1]) + "…"
}

// ---- tasks --------------------------------------------------------------

func runTasks(args []string) int {
	fs := flag.NewFlagSet("tasks", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("state-dir", "", "State directory to read (default: CLI_AGENT_MCP_STATE_DIR, else the per-user one).")
	agentName := fs.String("agent", "", "Show only this agent's tasks (claude, cursor, …).")
	status := fs.String("status", "", "Show only tasks in this state (running, done, failed, canceled, orphaned).")
	limit := fs.Int("n", 20, "How many tasks to show, newest first (0 = all).")
	asJSON := fs.Bool("json", false, "Emit the records as they are, in JSON.")
	noColor := fs.Bool("no-color", false, "Disable colour.")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: cli-agent-mcp tasks [options]\n\nList the delegated tasks, newest first.\n\nOptions:\n")
		fs.PrintDefaults()
	}
	if _, err := parseWithPositionals(fs, args); err != nil {
		return 2
	}

	src, err := Open(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer src.Close()

	tasks, err := src.Tasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	filtered := make([]task.Snapshot, 0, len(tasks))
	for _, t := range tasks {
		if matches(t, *agentName, *status) {
			filtered = append(filtered, t)
		}
	}
	if *limit > 0 && len(filtered) > *limit {
		filtered = filtered[:*limit]
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(filtered); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}

	p := newPalette(*noColor)
	header(src, p)
	fmt.Println()
	printTable(filtered, p)
	if len(filtered) == 0 {
		return 0
	}
	fmt.Println()
	fmt.Println(p.paint(cDim, "follow one live:  cli-agent-mcp logs <id>   ·   all at once:  cli-agent-mcp logs --all"))
	return 0
}

// printTable renders the listing shared by `tasks` and the interactive picker.
func printTable(tasks []task.Snapshot, p palette) {
	if len(tasks) == 0 {
		fmt.Println("No tasks yet.")
		return
	}
	idW := 2
	for _, t := range tasks {
		if n := len(t.ID); n > idW {
			idW = n
		}
	}
	const statusW = 9
	now := time.Now()
	fmt.Println(p.paint(cBold, fmt.Sprintf("%3s  %-*s  %-*s  %-8s  %8s  %7s  %s",
		"#", idW, "ID", statusW, "STATUS", "AGENT", "TIME", "LINES", "PROMPT")))
	for i, t := range tasks {
		prompt := LastPrompt(t)
		if prompt == "" {
			prompt = "(no prompt)"
		}
		fmt.Printf("%3d  %-*s  %s  %-8s  %8s  %7d  %s\n",
			i+1, idW, t.ID,
			p.statusCell(t.Status, statusW),
			t.Agent,
			FormatDuration(Elapsed(t, now)),
			t.TotalLines,
			oneLine(prompt, 60))
	}
}

// ---- logs ---------------------------------------------------------------

func runLogs(args []string) int {
	fs := flag.NewFlagSet("logs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("state-dir", "", "State directory to read.")
	tailN := fs.Int("n", 200, "How many earlier lines to show before following (0 = only what is new, -1 = everything).")
	raw := fs.Bool("raw", false, "Show the agent's raw JSONL instead of the compact view.")
	forceFollow := fs.Bool("f", false, "Follow indefinitely, even once the task has finished (Ctrl-C to quit).")
	noFollow := fs.Bool("no-follow", false, "Print what is there and exit, without following.")
	all := fs.Bool("all", false, "Follow every running task at once, prefixing each line with its task.")
	agentName := fs.String("agent", "", "With --all or the picker: limit to the named agent.")
	noColor := fs.Bool("no-color", false, "Disable colour.")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: cli-agent-mcp logs [TASK] [options]

Show a task's log and follow it live for as long as it keeps running.

TASK may be the full id, an unambiguous fragment of one, "latest" (the most
recent) or "running" (the only one in flight). With no TASK, and if the terminal
is interactive, a picker appears.

Options:
`)
		fs.PrintDefaults()
	}
	positional, err := parseWithPositionals(fs, args)
	if err != nil {
		return 2
	}

	src, err := Open(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer src.Close()

	p := newPalette(*noColor)
	// Ctrl-C has to exit quietly: this is a viewer, and interrupting it is the
	// normal way to stop watching — it must not read as a failure.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *all {
		return tailAll(ctx, src, p, *agentName, *tailN, !*raw)
	}

	var ref string
	switch {
	case len(positional) > 0:
		ref = positional[0]
	case isTerminal(os.Stdin):
		chosen, ok := pick(src, p, *agentName)
		if !ok {
			return 0
		}
		ref = chosen.ID
	default:
		fmt.Fprintln(os.Stderr, `error: name a task (id, fragment, "latest" or "running"), or use --all.`)
		return 2
	}

	snap, err := src.Resolve(ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	return tailOne(ctx, src, p, snap, tailOpts{
		backlog: *tailN,
		compact: !*raw,
		follow:  !*noFollow,
		forever: *forceFollow,
	})
}

// pick shows the listing and asks which task to open. It is the "see the agents
// and choose one" path: no id to copy, just a number.
func pick(src *Source, p palette, agentName string) (task.Snapshot, bool) {
	tasks, err := src.Tasks()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return task.Snapshot{}, false
	}
	filtered := make([]task.Snapshot, 0, len(tasks))
	for _, t := range tasks {
		if matches(t, agentName, "") {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) > 20 {
		filtered = filtered[:20]
	}

	header(src, p)
	fmt.Println()
	printTable(filtered, p)
	if len(filtered) == 0 {
		return task.Snapshot{}, false
	}

	fmt.Println()
	fmt.Printf("Choose a task [1-%d] (Enter = 1, q to quit): ", len(filtered))
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	choice := strings.TrimSpace(line)
	if err != nil && choice == "" {
		fmt.Println()
		return task.Snapshot{}, false
	}
	switch strings.ToLower(choice) {
	case "q", "quit", "exit":
		return task.Snapshot{}, false
	case "":
		return filtered[0], true
	}
	if n, err := strconv.Atoi(choice); err == nil {
		if n < 1 || n > len(filtered) {
			fmt.Fprintf(os.Stderr, "error: %d is out of range.\n", n)
			return task.Snapshot{}, false
		}
		return filtered[n-1], true
	}
	// Not a number: treat it as a task reference, so pasting an id also works.
	snap, err := src.Resolve(choice)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return task.Snapshot{}, false
	}
	return snap, true
}

type tailOpts struct {
	backlog int
	compact bool
	follow  bool
	forever bool
}

func tailOne(ctx context.Context, src *Source, p palette, snap task.Snapshot, opts tailOpts) int {
	f, err := src.Follow(snap.ID, opts.backlog)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}

	fmt.Println(p.paint(cDim, fmt.Sprintf("── %s · %s · %s ──", snap.ID, snap.Agent, snap.Cwd)))
	if prompt := LastPrompt(snap); prompt != "" {
		fmt.Println(p.paint(cDim, "  "+oneLine(prompt, 100)))
	}
	fmt.Println()

	drain := func() {
		lines, err := f.Next()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error reading the transcript:", err)
			return
		}
		for _, l := range src.Render(snap.Agent, lines, opts.compact) {
			fmt.Println(l)
		}
	}

	drain()
	current := snap
	if cur, ok := src.Task(snap.ID); ok {
		current = cur
	}
	if !opts.follow || (current.Status != task.StatusRunning && !opts.forever) {
		// One more read: the record may have settled between opening the
		// follower and now, leaving the final lines unread.
		drain()
		printFooter(current, p, false)
		return exitFor(current)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	settled := 0
	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			fmt.Println(p.paint(cDim, "— interrupted; the task carries on —"))
			return 0
		case <-ticker.C:
			drain()
			if cur, ok := src.Task(snap.ID); ok {
				current = cur
			}
			if opts.forever || current.Status == task.StatusRunning {
				settled = 0
				continue
			}
			if settled++; settled >= settleDrains {
				printFooter(current, p, true)
				return exitFor(current)
			}
		}
	}
}

func printFooter(t task.Snapshot, p palette, followed bool) {
	fmt.Println()
	detail := fmt.Sprintf("── %s · %s · %s · %d lines",
		t.ID, string(t.Status), FormatDuration(Elapsed(t, time.Now())), t.TotalLines)
	if t.ExitCode != nil && *t.ExitCode != 0 {
		detail += fmt.Sprintf(" · exit %d", *t.ExitCode)
	}
	fmt.Println(p.paint(cDim, detail+" ──"))
	if t.Error != "" {
		fmt.Println(p.paint(cRed, "  "+t.Error))
	}
	if !followed && t.Status == task.StatusRunning {
		fmt.Println(p.paint(cDim, "  still running — drop --no-follow to watch it live"))
	}
	if t.Status == task.StatusOrphaned {
		fmt.Println(p.paint(cDim, "  another server process started it; this viewer can only read it"))
	}
}

// exitFor makes the command usable in a script: a failed task is a failed
// command, so `cli-agent-mcp logs latest --no-follow && deploy` behaves.
func exitFor(t task.Snapshot) int {
	if t.Status == task.StatusFailed {
		return 1
	}
	return 0
}

// ---- logs --all ---------------------------------------------------------

// prefixColors cycle so two concurrent tasks are told apart at a glance.
var prefixColors = []string{"36", "35", "33", "32", "34", "31"}

type stream struct {
	snap   task.Snapshot
	f      *state.Follower
	prefix string
	dead   int // consecutive drains since the task settled
}

// tailAll follows every running task at once, and picks up tasks that start
// later. This is the "watch everything" view: one terminal, everything the
// worker fleet is doing, each line tagged with whose it is.
func tailAll(ctx context.Context, src *Source, p palette, agentName string, backlog int, compact bool) int {
	header(src, p)
	fmt.Println(p.paint(cDim, "following every running task — Ctrl-C to quit"))
	fmt.Println()

	streams := map[string]*stream{}
	next := 0

	attach := func() {
		tasks, err := src.Tasks()
		if err != nil {
			return
		}
		for _, t := range tasks {
			if t.Status != task.StatusRunning || !matches(t, agentName, "") {
				continue
			}
			if _, ok := streams[t.ID]; ok {
				continue
			}
			f, err := src.Follow(t.ID, backlog)
			if err != nil {
				continue
			}
			color := prefixColors[next%len(prefixColors)]
			next++
			streams[t.ID] = &stream{
				snap:   t,
				f:      f,
				prefix: p.paint(color, fmt.Sprintf("%-10s", shortID(t.ID))),
			}
			fmt.Println(p.paint(cDim, fmt.Sprintf("+ %s (%s) %s", t.ID, t.Agent, oneLine(LastPrompt(t), 70))))
		}
	}

	attach()
	if len(streams) == 0 {
		fmt.Println(p.paint(cDim, "nothing running right now; waiting…"))
	}

	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	rescan := time.NewTicker(2 * time.Second)
	defer rescan.Stop()

	for {
		select {
		case <-ctx.Done():
			fmt.Println()
			fmt.Println(p.paint(cDim, "— interrupted; the tasks carry on —"))
			return 0

		case <-rescan.C:
			attach()

		case <-tick.C:
			for id, s := range streams {
				lines, err := s.f.Next()
				if err != nil {
					continue
				}
				for _, l := range src.Render(s.snap.Agent, lines, compact) {
					fmt.Println(s.prefix + " " + l)
				}
				cur, ok := src.Task(id)
				if !ok {
					continue
				}
				s.snap = cur
				if cur.Status == task.StatusRunning {
					s.dead = 0
					continue
				}
				// Keep draining briefly past the transition, then let it go —
				// otherwise a long session accumulates followers over dead tasks.
				if s.dead++; s.dead >= settleDrains {
					fmt.Println(p.paint(cDim, fmt.Sprintf("- %s finished: %s", id, string(cur.Status))))
					delete(streams, id)
				}
			}
		}
	}
}
