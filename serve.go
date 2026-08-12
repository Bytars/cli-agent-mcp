package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andresh0816/cli-agent-mcp/internal/ui"
)

// serveHTTP runs the server over streamable HTTP instead of stdio.
//
// It exists to fix something stdio cannot. Over stdio the client owns the
// process, so every client that wants these tools starts its own copy — and a
// second copy cannot see, watch or cancel the first one's workers. The whole
// orphaned-instance apparatus exists to make that survivable. One long-lived
// HTTP server removes the cause instead: Cowork, Claude Desktop and anything
// else connect to the same process, and there is only ever one registry.
//
// The trade is that a socket has no parent to inherit trust from. This process
// can run arbitrary commands on the machine, so the endpoint is bound to
// loopback and gated on a bearer token by default, and both defaults have to be
// overridden deliberately.
func serveHTTP(ctx context.Context, srv *mcp.Server, addr, token string) error {
	if addr == "" {
		addr = defaultHTTPAddr
	}

	generated := false
	if token == "" {
		var raw [24]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return fmt.Errorf("generating an access token: %w", err)
		}
		token = hex.EncodeToString(raw[:])
		generated = true
	}

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)

	mux := http.NewServeMux()
	mux.Handle("/mcp", requireToken(token, handler))

	// The board as an ordinary web page.
	//
	// As an MCP view it only appears in hosts that implement the views
	// extension, and when a host does not, the tool quietly returns text
	// instead — so the panel is not merely unavailable, it is invisible in a way
	// that looks like the board being unimpressive. Served here it works in any
	// browser, from the same document, against the same endpoint.
	//
	// The token rides in the query string because a browser navigating to a page
	// cannot set an Authorization header; the page strips it from the address
	// bar on load and keeps it in memory for its own calls.
	mux.HandleFunc("/board", func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(token)) != 1 {
			http.Error(w, "unauthorized: open the /board URL printed at startup", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		// The page holds a live credential; keeping it out of any other site's
		// frame costs nothing and closes the obvious way to steal one.
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = io.WriteString(w, ui.BoardHTML)
	})
	// A liveness check that does not require the token, so a supervisor can tell
	// the difference between "not running" and "running, and you have the wrong
	// credential" without holding one.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	if !isLoopback(ln.Addr()) {
		log.Printf("WARNING: listening on %s, which is not loopback. Anything that can reach this address and holds the token can run commands on this machine.", ln.Addr())
	}
	log.Printf("http transport: http://%s/mcp", ln.Addr())
	log.Printf("task board:     http://%s/board?token=%s", ln.Addr(), token)
	if generated {
		// Printed once, on stderr, because there is no other way for the operator
		// to learn a token the process invented. Set CLI_AGENT_MCP_HTTP_TOKEN to
		// pin it instead of reading it back from the log on every restart.
		log.Printf("access token (generated for this run): %s", token)
	}

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const defaultHTTPAddr = "127.0.0.1:7777"

// requireToken gates the endpoint on a bearer token, compared in constant time
// so a wrong guess reveals nothing about how wrong it was.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="cli-agent-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopback(addr net.Addr) bool {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// httpAddrFromArgs reads the --http flag, accepting both "--http :7777" and
// "--http=:7777", and reports whether it was given at all.
func httpAddrFromArgs(args []string) (addr string, enabled bool) {
	for i, a := range args {
		switch {
		case a == "--http":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				return args[i+1], true
			}
			return defaultHTTPAddr, true
		case strings.HasPrefix(a, "--http="):
			if v := strings.TrimPrefix(a, "--http="); v != "" {
				return v, true
			}
			return defaultHTTPAddr, true
		}
	}
	return "", false
}

// hostToken reads the configured bearer token from the environment.
func hostToken() string { return strings.TrimSpace(os.Getenv("CLI_AGENT_MCP_HTTP_TOKEN")) }
