// SPDX-License-Identifier: Apache-2.0

package inspect

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Bytars/cli-agent-mcp/internal/task"
)

// LiveHTML is the whole viewer — markup, styles and script in one file, with no
// external requests. Keeping it self-contained means the page works with no
// network at all, which is rather the point of a local viewer.
//
//go:embed live.html
var LiveHTML string

// runUI serves the local web viewer.
//
// It is a second process reading the same files the server writes, so it can be
// started, stopped and restarted at any time without touching a run in flight.
// It is also strictly read-only: cancelling a task needs the process that owns
// the worker, which is the MCP server, not this.
func runUI(args []string) int {
	fs := flag.NewFlagSet("ui", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("state-dir", "", "State directory to read.")
	port := fs.Int("port", 7788, "Port to listen on.")
	host := fs.String("host", "127.0.0.1", "Interface to listen on.")
	allowRemote := fs.Bool("allow-remote", false, "Allow listening beyond localhost.")
	openBrowser := fs.Bool("open", false, "Open the browser on startup.")
	noToken := fs.Bool("no-token", false, "Serve without a session token, to anything that can reach the port.")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: cli-agent-mcp ui [options]

Serve a local web viewer: task list on the left, live log on the right. Read-only
— to cancel a task, use the task board in your MCP client.

Options:
`)
		fs.PrintDefaults()
	}
	if _, err := parseWithPositionals(fs, args); err != nil {
		return 2
	}

	// A transcript holds everything the worker saw and did: prompts, file
	// contents, command output. Binding that to a public interface is a
	// decision, not a default.
	if !isLoopback(*host) && !*allowRemote {
		fmt.Fprintf(os.Stderr,
			"error: %q is not localhost: anyone who can reach that port would be able to attempt\n"+
				"       reading the full transcripts (prompts, file contents, command output).\n"+
				"       If that is genuinely what you want, add --allow-remote.\n", *host)
		return 2
	}
	// Off-machine, the session token is the only thing left standing between a
	// transcript and the network. Refuse the combination rather than serve it.
	if !isLoopback(*host) && *noToken {
		fmt.Fprint(os.Stderr,
			"error: --no-token with a non-local address would publish every transcript to anyone who\n"+
				"       can reach the port, with nothing to authenticate them. Drop one of the two.\n")
		return 2
	}

	src, err := Open(*dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	defer src.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/", src.handleIndex)
	mux.HandleFunc("/api/tasks", src.handleTasks)
	mux.HandleFunc("/api/log", src.handleLog)

	var token string
	if !*noToken {
		if token, err = newSessionToken(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
	}

	addr := net.JoinHostPort(*host, strconv.Itoa(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: could not listen on %s: %v\n", addr, err)
		return 1
	}

	srv := &http.Server{
		Handler: &guard{
			next:  mux,
			token: token,
			// The loopback Host check only applies to a loopback listener.
			// Someone who deliberately bound elsewhere reaches it by a name
			// this process cannot know, and the token is their protection.
			local: isLoopback(*host),
		},
		ReadHeaderTimeout: 10 * time.Second,
	}

	url := "http://" + addr + "/"
	if token != "" {
		url += "?" + queryParam + "=" + token
	}
	fmt.Printf("viewer at %s\n", url)
	if token == "" {
		fmt.Println("warning:  --no-token: anything that can reach this port reads every transcript")
	} else {
		fmt.Println("          (the link carries a one-off session token; it dies with this process)")
	}
	fmt.Printf("state:    %s\n", src.Dir())
	if o := src.Owner(); o != nil {
		fmt.Printf("server running: pid %d\n", o.PID)
	} else {
		fmt.Println("no server running (history only)")
	}
	fmt.Println("Ctrl-C to stop")
	if *openBrowser {
		launchBrowser(url)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		fmt.Println("\nviewer stopped")
		return 0
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		return 0
	}
}

// isLoopback reports whether an address only reachable from this machine was
// requested. An empty host means "every interface", which is not loopback.
func isLoopback(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" {
		return false
	}
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

func launchBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// Through rundll32 rather than `cmd /c start`: no second parser gets to
		// reinterpret the URL.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not open the browser (%v); visit %s yourself\n", err, url)
	}
}

// ---- HTTP handlers ------------------------------------------------------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Every response is a point-in-time view of files that keep changing.
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Source) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(LiveHTML))
}

type ownerJSON struct {
	PID     int    `json:"pid"`
	Started string `json:"started"`
}

type tasksResponse struct {
	Dir   string          `json:"dir"`
	Owner *ownerJSON      `json:"owner,omitempty"`
	Tasks []task.Snapshot `json:"tasks"`
}

func (s *Source) handleTasks(w http.ResponseWriter, _ *http.Request) {
	tasks, err := s.Tasks()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := tasksResponse{Dir: s.Dir(), Tasks: tasks}
	if o := s.Owner(); o != nil {
		resp.Owner = &ownerJSON{PID: o.PID, Started: o.Started.Format(time.RFC3339)}
	}
	writeJSON(w, resp)
}

type logResponse struct {
	ID    string        `json:"task_id"`
	Since int           `json:"since"`
	Total int           `json:"total"`
	Lines []string      `json:"lines"`
	Task  task.Snapshot `json:"task"`
}

// handleLog serves a transcript slice. `since` is a raw line index and the
// response's `total` is the cursor for the next call, so the page appends
// deltas instead of re-fetching a growing transcript once a second.
func (s *Source) handleLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	since, _ := strconv.Atoi(q.Get("since"))
	compact := q.Get("raw") != "1"

	snap, ok := s.Task(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such task")
		return
	}
	raw, total, err := s.Lines(id, since)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, logResponse{
		ID:    id,
		Since: since,
		Total: total,
		Lines: s.Render(snap.Agent, raw, compact),
		Task:  snap,
	})
}
